package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Detection says which runtime endpoint detection picked and why.
type Detection struct {
	// Endpoint is the chosen socket or URL.
	Endpoint string
	// Source names what chose it: "$DOCKER_HOST", "Docker context \"x\""
	// (from $DOCKER_CONTEXT or config.json) or "socket list".
	Source string
	// Found lists the runtime sockets present on this host, chosen or not.
	Found []string
}

// Detect finds a runtime and returns a client for it; see Locate.
func Detect() (*Client, error) {
	cl, _, err := Locate()
	return cl, err
}

// Locate finds a runtime. It tries $DOCKER_HOST, then the Docker context
// ($DOCKER_CONTEXT, else currentContext in config.json), then a fixed list
// of sockets (Docker Desktop, OrbStack, Colima, Podman machine, Linux).
// An explicit choice ($DOCKER_HOST, or a context other than "default") that
// does not respond is an error naming it, so gcpemu and `docker` agree.
func Locate() (*Client, Detection, error) {
	home, _ := os.UserHomeDir()
	return locate(hostEnv{
		getenv:   os.Getenv,
		home:     home,
		isSocket: isSocket,
		group:    fileGroup,
		probe:    probe,
	})
}

// hostEnv is what detection reads from the host; tests fake it.
type hostEnv struct {
	getenv   func(string) string
	home     string
	isSocket func(path string) bool
	// group returns the group id owning path, if known.
	group func(path string) (int, bool)
	// probe connects to endpoint and returns a usable client.
	probe func(endpoint string) (*Client, error)
}

func locate(e hostEnv) (*Client, Detection, error) {
	d := Detection{Found: e.sockets()}
	explicit := func(endpoint, source string) (*Client, Detection, error) {
		d.Endpoint, d.Source = endpoint, source
		cl, err := e.probe(endpoint)
		if err != nil {
			return nil, d, fmt.Errorf("%s chosen by %s does not respond: %w", endpoint, source, err)
		}
		return cl, d, nil
	}
	if h := e.getenv("DOCKER_HOST"); h != "" {
		return explicit(h, "$DOCKER_HOST")
	}
	name, from, err := e.context()
	if err != nil {
		return nil, d, err
	}
	if name != "" && name != "default" {
		source := fmt.Sprintf("Docker context %q from %s", name, from)
		endpoint, err := e.contextEndpoint(name)
		if err != nil {
			d.Source = source
			return nil, d, fmt.Errorf("%s: %w", source, err)
		}
		return explicit(endpoint, source)
	}
	var denied []string
	for _, sock := range d.Found {
		cl, err := e.probe("unix://" + sock)
		if err == nil {
			d.Endpoint, d.Source = "unix://"+sock, "socket list"
			return cl, d, nil
		}
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
			denied = append(denied, sock)
		}
	}
	if len(denied) > 0 {
		return nil, d, fmt.Errorf("container runtime socket %s found but permission denied (uid %d): %s", denied[0], os.Getuid(), groupAddHint(e.group, denied[0]))
	}
	return nil, d, errors.New("no container runtime found: start Docker Engine (>= 24), Docker Desktop, OrbStack, Colima or Podman (>= 4.9) with its Docker-compatible socket, or set DOCKER_HOST")
}

// groupAddHint says how to let the OCI image use sock: the --group-add
// value is the socket's group as the container sees it, which works
// whatever OS the docker command runs on.
func groupAddHint(group func(string) (int, bool), sock string) string {
	if group != nil {
		if gid, ok := group(sock); ok {
			return fmt.Sprintf("in the OCI image, run with --group-add %d (the socket's group); otherwise join that group", gid)
		}
	}
	return "in the OCI image, run with --group-add set to the socket's group id; otherwise join that group"
}

// sockets returns the well-known runtime sockets that exist, in the order
// detection tries them.
func (e hostEnv) sockets() []string {
	paths := []string{
		"/var/run/docker.sock",
		filepath.Join(e.home, ".docker", "run", "docker.sock"), // Docker Desktop
		filepath.Join(e.home, ".orbstack", "run", "docker.sock"),
		filepath.Join(e.home, ".colima", "default", "docker.sock"),
	}
	// Podman machine (macOS): the first match of each glob, so no podman
	// command runs.
	for _, g := range []string{
		filepath.Join(e.tmpdir(), "podman", "*-api.sock"),
		filepath.Join(e.home, ".local", "share", "containers", "podman", "machine", "*", "podman.sock"),
	} {
		if m, _ := filepath.Glob(g); len(m) > 0 {
			paths = append(paths, m[0])
		}
	}
	if d := e.getenv("XDG_RUNTIME_DIR"); d != "" {
		paths = append(paths, filepath.Join(d, "podman", "podman.sock"), filepath.Join(d, "docker.sock"))
	}
	paths = append(paths, "/run/podman/podman.sock")
	var found []string
	seen := map[string]bool{}
	for _, p := range paths {
		if !seen[p] && e.isSocket(p) {
			found = append(found, p)
		}
		seen[p] = true
	}
	return found
}

func (e hostEnv) tmpdir() string {
	if d := e.getenv("TMPDIR"); d != "" {
		return d
	}
	return "/tmp"
}

func (e hostEnv) configDir() string {
	if d := e.getenv("DOCKER_CONFIG"); d != "" {
		return d
	}
	return filepath.Join(e.home, ".docker")
}

// context returns the chosen Docker context name and where it came from;
// "" when none is chosen.
func (e hostEnv) context() (name, from string, err error) {
	if c := e.getenv("DOCKER_CONTEXT"); c != "" {
		return c, "$DOCKER_CONTEXT", nil
	}
	p := filepath.Join(e.configDir(), "config.json")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("reading Docker config: %w", err)
	}
	var cfg struct {
		CurrentContext string `json:"currentContext"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", "", fmt.Errorf("reading Docker config %s: %w", p, err)
	}
	return cfg.CurrentContext, p, nil
}

// contextEndpoint reads a Docker context's endpoint from its metadata,
// stored under the SHA-256 of its name.
func (e hostEnv) contextEndpoint(name string) (string, error) {
	sum := sha256.Sum256([]byte(name))
	p := filepath.Join(e.configDir(), "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("no such context")
	}
	if err != nil {
		return "", fmt.Errorf("reading context metadata: %w", err)
	}
	var m struct {
		Endpoints map[string]struct{ Host string }
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("context metadata %s: %w", p, err)
	}
	h := m.Endpoints["docker"].Host
	if h == "" {
		return "", fmt.Errorf("context metadata %s has no docker endpoint", p)
	}
	return h, nil
}

func isSocket(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// probe returns a client for endpoint once it answers /info.
func probe(endpoint string) (*Client, error) {
	cl, err := New(endpoint)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cl.Info(ctx); err != nil {
		return nil, err
	}
	cl.detectSelf(ctx)
	return cl, nil
}
