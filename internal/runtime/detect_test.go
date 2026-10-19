package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeHost is a fake HOME, TMPDIR and environment. Sockets are plain
// files, and only endpoints in up respond.
type fakeHost struct {
	t    *testing.T
	root string
	home string
	env  map[string]string
	up   map[string]bool
	goos string
	// pipes are the named pipes present, as //./pipe/NAME.
	pipes map[string]bool
	// osType is what /info reports per endpoint; "" is an /info that did
	// not answer, taken as linux.
	osType map[string]string
}

func newFakeHost(t *testing.T) *fakeHost {
	root := t.TempDir()
	h := &fakeHost{t: t, root: root, home: filepath.Join(root, "home"), up: map[string]bool{},
		env: map[string]string{"TMPDIR": filepath.Join(root, "tmp")}, goos: "linux",
		pipes: map[string]bool{}, osType: map[string]string{}}
	return h
}

// rooted reports whether p is absolute. A path starting with / counts
// too, so the literal /var/run/docker.sock in the socket list stays
// rooted on Windows.
func rooted(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/")
}

// sock adds a socket at p, relative to HOME unless rooted. Only one
// under the fake root is created on disk (for the Podman globs); others
// exist only to the faked socket check.
func (h *fakeHost) sock(p string, up bool) string {
	if !rooted(p) {
		p = filepath.Join(h.home, p)
	}
	h.up["unix://"+p] = up
	if !strings.HasPrefix(p, h.root) {
		return p
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *fakeHost) write(p, content string) {
	p = filepath.Join(h.home, p)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// context stores a Docker context named name with endpoint host.
func (h *fakeHost) context(name, host string) {
	sum := sha256.Sum256([]byte(name))
	meta, err := json.Marshal(map[string]any{"Name": name,
		"Endpoints": map[string]any{"docker": map[string]any{"Host": host, "SkipTLSVerify": false}}})
	if err != nil {
		h.t.Fatal(err)
	}
	h.write(filepath.Join(".docker", "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"), string(meta))
}

func (h *fakeHost) locate() (Detection, error) {
	_, d, err := locate(hostEnv{
		getenv: func(k string) string { return h.env[k] },
		home:   h.home,
		goos:   h.goos,
		isPipe: func(addr string) bool { return h.pipes[addr] },
		isSocket: func(p string) bool {
			// Only fake sockets count, not the real host's.
			_, ok := h.up["unix://"+p]
			return ok
		},
		probe: func(endpoint string) (*Client, Info, error) {
			if h.up[endpoint] {
				return &Client{Endpoint: endpoint}, Info{OSType: h.osType[endpoint]}, nil
			}
			return nil, Info{}, errors.New("connection refused")
		},
	})
	return d, err
}

// A socket the user may not open names the --group-add value, not a
// GNU-only stat command (#175).
func TestGroupAddHint(t *testing.T) {
	got := groupAddHint(func(string) (int, bool) { return 991, true }, "/var/run/docker.sock")
	if !strings.Contains(got, "--group-add 991") {
		t.Errorf("known group: %q", got)
	}
	for _, g := range []func(string) (int, bool){nil, func(string) (int, bool) { return 0, false }} {
		if got := groupAddHint(g, "/var/run/docker.sock"); !strings.Contains(got, "--group-add") || strings.Contains(got, "stat -c") {
			t.Errorf("unknown group: %q", got)
		}
	}
}

func TestLocateSocketOrder(t *testing.T) {
	// Each socket in list order; each case's socket responds, and every
	// socket after it exists and responds too, so order decides.
	order := []string{
		"/var/run/docker.sock",
		".docker/run/docker.sock",
		".orbstack/run/docker.sock",
		".colima/default/docker.sock",
		"TMPDIR/podman/podman-machine-default-api.sock",
		".local/share/containers/podman/machine/podman-machine-default/podman.sock",
		"XDG/podman/podman.sock",
		"XDG/docker.sock",
	}
	for i, want := range order {
		t.Run(want, func(t *testing.T) {
			h := newFakeHost(t)
			xdg := filepath.Join(h.home, "..", "xdg")
			h.env["XDG_RUNTIME_DIR"] = xdg
			path := func(p string) string {
				if r, ok := strings.CutPrefix(p, "TMPDIR/"); ok {
					return filepath.Join(h.env["TMPDIR"], r)
				}
				if r, ok := strings.CutPrefix(p, "XDG/"); ok {
					return filepath.Join(xdg, r)
				}
				return p
			}
			for j, p := range order {
				if j >= i {
					h.sock(path(p), true)
				}
			}
			// An earlier socket that exists but doesn't respond is skipped.
			if i > 0 {
				h.sock(path(order[i-1]), false)
			}
			d, err := h.locate()
			if err != nil {
				t.Fatal(err)
			}
			wantPath := path(want)
			if !rooted(wantPath) {
				wantPath = filepath.Join(h.home, wantPath)
			}
			if d.Endpoint != "unix://"+wantPath || d.Source != "socket list" {
				t.Errorf("got %s via %s, want %s via socket list", d.Endpoint, d.Source, wantPath)
			}
			if !slices.Contains(d.Found, "unix://"+wantPath) {
				t.Errorf("Found %v lacks %s", d.Found, wantPath)
			}
		})
	}
}

func TestLocatePodmanMachineFirstMatchOfEachGlob(t *testing.T) {
	h := newFakeHost(t)
	tmp := h.sock(filepath.Join(h.env["TMPDIR"], "podman", "a-api.sock"), true)
	h.sock(filepath.Join(h.env["TMPDIR"], "podman", "b-api.sock"), true)
	machine := h.sock(".local/share/containers/podman/machine/a/podman.sock", true)
	h.sock(".local/share/containers/podman/machine/b/podman.sock", true)
	d, err := h.locate()
	if err != nil {
		t.Fatal(err)
	}
	if d.Endpoint != "unix://"+tmp {
		t.Errorf("Endpoint = %s, want %s", d.Endpoint, tmp)
	}
	if want := []string{"unix://" + tmp, "unix://" + machine}; !slices.Equal(d.Found, want) {
		t.Errorf("Found = %v, want %v", d.Found, want)
	}
}

func TestLocateDockerHost(t *testing.T) {
	h := newFakeHost(t)
	h.sock(".docker/run/docker.sock", true)
	h.env["DOCKER_HOST"] = "tcp://127.0.0.1:2375"
	h.up["tcp://127.0.0.1:2375"] = true
	h.env["DOCKER_CONTEXT"] = "colima"
	h.context("colima", "unix:///nowhere")
	d, err := h.locate()
	if err != nil {
		t.Fatal(err)
	}
	if d.Endpoint != "tcp://127.0.0.1:2375" || d.Source != "$DOCKER_HOST" {
		t.Errorf("got %s via %s", d.Endpoint, d.Source)
	}
}

func TestLocateDockerHostFailsFast(t *testing.T) {
	h := newFakeHost(t)
	h.sock(".docker/run/docker.sock", true)
	h.env["DOCKER_HOST"] = "unix:///gone.sock"
	_, err := h.locate()
	if err == nil || !strings.Contains(err.Error(), "$DOCKER_HOST") || !strings.Contains(err.Error(), "/gone.sock") {
		t.Errorf("err = %v, want it to name $DOCKER_HOST and the socket", err)
	}
}

func TestLocateContextFromEnv(t *testing.T) {
	h := newFakeHost(t)
	h.sock(".docker/run/docker.sock", true)
	colima := h.sock(".colima/other/docker.sock", true)
	h.context("colima-other", "unix://"+colima)
	h.env["DOCKER_CONTEXT"] = "colima-other"
	h.write(".docker/config.json", `{"currentContext":"desktop-linux"}`)
	d, err := h.locate()
	if err != nil {
		t.Fatal(err)
	}
	if d.Endpoint != "unix://"+colima || !strings.Contains(d.Source, `"colima-other"`) || !strings.Contains(d.Source, "$DOCKER_CONTEXT") {
		t.Errorf("got %s via %s", d.Endpoint, d.Source)
	}
}

func TestLocateContextFromConfig(t *testing.T) {
	h := newFakeHost(t)
	orb := h.sock(".orbstack/run/docker.sock", true)
	h.sock(".docker/run/docker.sock", true)
	h.context("orbstack", "unix://"+orb)
	h.write(".docker/config.json", `{"auths":{},"currentContext":"orbstack"}`)
	d, err := h.locate()
	if err != nil {
		t.Fatal(err)
	}
	if d.Endpoint != "unix://"+orb || !strings.Contains(d.Source, `"orbstack"`) || !strings.Contains(d.Source, "config.json") {
		t.Errorf("got %s via %s", d.Endpoint, d.Source)
	}
}

func TestLocateDockerConfigOverridesDir(t *testing.T) {
	h := newFakeHost(t)
	colima := h.sock(".colima/default/docker.sock", true)
	h.sock(".docker/run/docker.sock", true)
	h.context("colima", "unix://"+colima)
	// Move the contexts and config to $DOCKER_CONFIG.
	cfg := filepath.Join(h.home, "elsewhere")
	if err := os.Rename(filepath.Join(h.home, ".docker", "contexts"), filepath.Join(h.home, "contexts")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(h.home, "contexts"), filepath.Join(cfg, "contexts")); err != nil {
		t.Fatal(err)
	}
	h.write("elsewhere/config.json", `{"currentContext":"colima"}`)
	h.env["DOCKER_CONFIG"] = cfg
	d, err := h.locate()
	if err != nil {
		t.Fatal(err)
	}
	if d.Endpoint != "unix://"+colima {
		t.Errorf("got %s via %s, want the colima context", d.Endpoint, d.Source)
	}
}

func TestLocateContextFailsFast(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *fakeHost)
		want  []string
	}{
		{"env context down", func(h *fakeHost) {
			h.context("colima", "unix://"+h.sock(".colima/default/docker.sock", false))
			h.env["DOCKER_CONTEXT"] = "colima"
		}, []string{"$DOCKER_CONTEXT", `"colima"`}},
		{"env context missing", func(h *fakeHost) {
			h.env["DOCKER_CONTEXT"] = "nope"
		}, []string{"$DOCKER_CONTEXT", `"nope"`}},
		{"config context down", func(h *fakeHost) {
			h.context("orbstack", "unix:///nowhere/docker.sock")
			h.write(".docker/config.json", `{"currentContext":"orbstack"}`)
		}, []string{"config.json", `"orbstack"`, "/nowhere/docker.sock"}},
		{"config context missing", func(h *fakeHost) {
			h.write(".docker/config.json", `{"currentContext":"gone"}`)
		}, []string{"config.json", `"gone"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t)
			h.sock(".docker/run/docker.sock", true) // would answer if fallen through to
			tc.setup(h)
			d, err := h.locate()
			if err == nil {
				t.Fatalf("picked %s via %s, want an error", d.Endpoint, d.Source)
			}
			if d.Source == "" {
				t.Error("Detection.Source is empty, want the failing source")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v, want it to contain %s", err, w)
				}
			}
		})
	}
}

func TestLocateDefaultContextFallsThrough(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *fakeHost)
	}{
		{"no config", func(*fakeHost) {}},
		{"config without context", func(h *fakeHost) { h.write(".docker/config.json", `{"auths":{}}`) }},
		{"config default", func(h *fakeHost) { h.write(".docker/config.json", `{"currentContext":"default"}`) }},
		{"env default", func(h *fakeHost) {
			h.env["DOCKER_CONTEXT"] = "default"
			h.write(".docker/config.json", `{"currentContext":"gone"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t)
			want := h.sock(".docker/run/docker.sock", true)
			tc.setup(h)
			d, err := h.locate()
			if err != nil {
				t.Fatal(err)
			}
			if d.Endpoint != "unix://"+want || d.Source != "socket list" {
				t.Errorf("got %s via %s", d.Endpoint, d.Source)
			}
		})
	}
}

func TestLocateNothing(t *testing.T) {
	h := newFakeHost(t)
	h.sock(".colima/default/docker.sock", false)
	_, err := h.locate()
	if err == nil || !strings.Contains(err.Error(), "no container runtime found") {
		t.Errorf("err = %v", err)
	}
}

// pipe adds the named pipe NAME and returns its endpoint.
func (h *fakeHost) pipe(name string, up bool) string {
	h.pipes["//./pipe/"+name] = true
	ep := "npipe:////./pipe/" + name
	h.up[ep] = up
	return ep
}

// On Windows, detection probes Docker Desktop's Linux engine, then
// docker_engine, then Podman machine's pipe (#375).
func TestLocateWindowsPipeOrder(t *testing.T) {
	order := []string{"dockerDesktopLinuxEngine", "docker_engine", "podman-machine-default"}
	for i, want := range order {
		t.Run(want, func(t *testing.T) {
			h := newFakeHost(t)
			h.goos = "windows"
			h.sock("/var/run/docker.sock", true) // not looked for on Windows
			var all []string
			for j, p := range order {
				all = append(all, h.pipe(p, j >= i))
			}
			d, err := h.locate()
			if err != nil {
				t.Fatal(err)
			}
			if d.Endpoint != "npipe:////./pipe/"+want || d.Source != "named pipe list" {
				t.Errorf("got %s via %s, want %s via named pipe list", d.Endpoint, d.Source, want)
			}
			if !slices.Equal(d.Found, all) {
				t.Errorf("Found = %v, want %v", d.Found, all)
			}
		})
	}
}

// A runtime named explicitly that serves Windows containers fails start
// rather than falling back, on every OS (#375).
func TestLocateExplicitWindowsContainersFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *fakeHost) string
	}{
		{"DOCKER_HOST", func(h *fakeHost) string {
			h.env["DOCKER_HOST"] = "npipe:////./pipe/docker_engine"
			h.up["npipe:////./pipe/docker_engine"] = true
			return "npipe:////./pipe/docker_engine"
		}},
		{"context", func(h *fakeHost) string {
			h.context("remote", "tcp://10.0.0.5:2375")
			h.env["DOCKER_CONTEXT"] = "remote"
			h.up["tcp://10.0.0.5:2375"] = true
			return "tcp://10.0.0.5:2375"
		}},
	} {
		for _, goos := range []string{"windows", "linux", "darwin"} {
			t.Run(tc.name+"/"+goos, func(t *testing.T) {
				h := newFakeHost(t)
				h.goos = goos
				h.sock(".docker/run/docker.sock", true) // would answer if fallen through to
				h.pipe("dockerDesktopLinuxEngine", true)
				ep := tc.setup(h)
				h.osType[ep] = "windows"
				d, err := h.locate()
				if err == nil || !strings.Contains(err.Error(), "serves Windows containers; switch Docker Desktop to Linux containers") || !strings.Contains(err.Error(), ep) {
					t.Fatalf("got %s via %s, err %v; want the Windows containers error naming %s", d.Endpoint, d.Source, err, ep)
				}
			})
		}
	}
}

// A probed endpoint that serves Windows containers is skipped, and said
// to be (#375).
func TestLocateProbedWindowsContainersSkipped(t *testing.T) {
	h := newFakeHost(t)
	h.goos = "windows"
	h.pipe("dockerDesktopLinuxEngine", false)
	engine := h.pipe("docker_engine", true)
	h.osType[engine] = "windows"
	podman := h.pipe("podman-machine-default", true)
	d, err := h.locate()
	if err != nil {
		t.Fatal(err)
	}
	if d.Endpoint != podman {
		t.Errorf("Endpoint = %s, want %s", d.Endpoint, podman)
	}
	if want := []string{engine}; !slices.Equal(d.Skipped, want) {
		t.Errorf("Skipped = %v, want %v", d.Skipped, want)
	}

	// With nothing else, the error says why the one found was not used.
	h.up[podman] = false
	_, err = h.locate()
	if err == nil || !strings.Contains(err.Error(), engine) || !strings.Contains(err.Error(), "Windows containers") {
		t.Errorf("err = %v, want it to name %s and Windows containers", err, engine)
	}
}

// The same holds for a socket on Linux or macOS.
func TestLocateProbedSocketWindowsContainersSkipped(t *testing.T) {
	h := newFakeHost(t)
	first := "unix://" + h.sock("/var/run/docker.sock", true)
	h.osType[first] = "windows"
	second := "unix://" + h.sock(".docker/run/docker.sock", true)
	d, err := h.locate()
	if err != nil {
		t.Fatal(err)
	}
	if d.Endpoint != second || !slices.Equal(d.Skipped, []string{first}) {
		t.Errorf("got %s, skipped %v; want %s, skipped %s", d.Endpoint, d.Skipped, second, first)
	}
}

// A permission-denied pipe on Windows names no uid, which is -1 there.
func TestLocateWindowsPermissionDenied(t *testing.T) {
	h := newFakeHost(t)
	h.goos = "windows"
	h.pipe("docker_engine", false)
	env := hostEnv{getenv: func(string) string { return "" }, home: h.home, goos: "windows",
		isPipe: func(a string) bool { return h.pipes[a] },
		probe: func(string) (*Client, Info, error) {
			return nil, Info{}, os.ErrPermission
		}}
	_, _, err := locate(env)
	if err == nil || strings.Contains(err.Error(), "uid") || !strings.Contains(err.Error(), "docker_engine") {
		t.Errorf("err = %v", err)
	}
}
