package agent

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// ImageRepos are the repositories of the published image, tried in order
// for a release build's binary (NFR-PORT-004). The release workflow tags
// vX.Y.Z as X.Y.Z.
var ImageRepos = []string{"ghcr.io/linuxuser586/gcpemu", "docker.io/linuxuser586/gcpemu"}

// imageFile is the binary's path in the published image.
const imageFile = "/gcpemu"

// Images is the part of the container runtime Resolver copies a release
// build's binary out of the published image with.
type Images interface {
	// ImageFile pulls ref for platform ("linux/arm64") and returns the
	// contents of file in it.
	ImageFile(ctx context.Context, ref, platform, file string) ([]byte, error)
}

// Resolver finds a static linux gcpemu binary for the container runtime's
// architecture, to copy into Managed containers (ADR 0005). The host's own
// executable is used when the host is linux on that architecture.
// Otherwise the binary is, in order: $GCPEMU_AGENT_BINARY; a sibling
// gcpemu-linux-<arch> next to the executable; the cache at
// <CacheDir>/<Version>/linux-<arch>/gcpemu; and, for a release build
// (Version exactly vX.Y.Z), the one in the published image X.Y.Z, which is
// then cached.
type Resolver struct {
	// Arch is the runtime's GOARCH; "" means the host's.
	Arch string
	// Version is the emulator's version (instance.Version).
	Version string
	// CacheDir holds binaries copied out of the published image; ""
	// disables the cache and the pull.
	CacheDir string
	// Offline skips the pull.
	Offline bool
	// Images pulls the published image; nil skips the pull.
	Images Images

	// Test hooks; zero values mean the real host.
	goos       string
	executable func() (string, error)
	getenv     func(string) string

	mu   sync.Mutex
	path string
}

var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// Release reports whether version is a release build's (exactly vX.Y.Z).
func Release(version string) bool { return releaseVersion.MatchString(version) }

// Binary returns the binary's path. A found binary is remembered; a
// failure is not, so a later call retries the pull.
func (r *Resolver) Binary(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.path != "" {
		return r.path, nil
	}
	p, err := r.resolve(ctx)
	if err != nil {
		return "", err
	}
	r.path = p
	return p, nil
}

func (r *Resolver) arch() string {
	if r.Arch != "" {
		return r.Arch
	}
	return runtime.GOARCH
}

func (r *Resolver) resolve(ctx context.Context) (string, error) {
	arch := r.arch()
	goos, executable, getenv := r.goos, r.executable, r.getenv
	if goos == "" {
		goos = runtime.GOOS
	}
	if executable == nil {
		executable = os.Executable
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	exe, exeErr := executable()
	if goos == "linux" && arch == runtime.GOARCH && getenv(BinaryEnv) == "" {
		if exeErr != nil {
			return "", exeErr
		}
		if dynamic(exe) {
			return "", fmt.Errorf("%s is dynamically linked, so it can't run as a Sidecar agent in containers; build gcpemu with CGO_ENABLED=0 (as `make build` does) or set %s to a static build", exe, BinaryEnv)
		}
		return exe, nil
	}

	if p := getenv(BinaryEnv); p != "" {
		return p, check(p, arch, BinaryEnv)
	}
	if exeErr == nil {
		for _, dir := range exeDirs(exe) {
			p := filepath.Join(dir, "gcpemu-linux-"+arch)
			if _, err := os.Stat(p); err == nil {
				return p, check(p, arch, "")
			}
		}
	}
	var cached string
	if r.CacheDir != "" {
		cached = filepath.Join(r.CacheDir, r.Version, "linux-"+arch, "gcpemu")
		if _, err := os.Stat(cached); err == nil {
			return cached, check(cached, arch, "")
		}
	}

	if !Release(r.Version) {
		return "", fmt.Errorf("no linux/%s gcpemu binary for the Sidecar agents in containers (a %s build doesn't fetch one): run `make agent` to build bin/gcpemu-linux-%s next to bin/gcpemu, or set %s", arch, r.Version, arch, BinaryEnv)
	}
	if cached == "" || r.Images == nil {
		return "", fmt.Errorf("no linux/%s gcpemu binary for the Sidecar agents in containers: put gcpemu-linux-%s next to gcpemu, or set %s", arch, arch, BinaryEnv)
	}
	if r.Offline {
		return "", fmt.Errorf("no linux/%s gcpemu binary for the Sidecar agents in containers, and --offline is set: put gcpemu-linux-%s next to gcpemu, set %s, or start once online to cache it in %s", arch, arch, BinaryEnv, filepath.Dir(cached))
	}
	if err := r.pull(ctx, arch, cached); err != nil {
		return "", err
	}
	return cached, nil
}

// pull copies the binary out of the published image into the cache at
// dest, atomically.
func (r *Resolver) pull(ctx context.Context, arch, dest string) error {
	var errs []error
	var data []byte
	for _, repo := range ImageRepos {
		ref := repo + ":" + strings.TrimPrefix(r.Version, "v")
		b, err := r.Images.ImageFile(ctx, ref, "linux/"+arch, imageFile)
		if err == nil {
			data = b
			break
		}
		errs = append(errs, fmt.Errorf("%s: %w", ref, err))
	}
	if data == nil {
		return fmt.Errorf("fetch the linux/%s gcpemu binary for the Sidecar agents in containers (or set %s): %w", arch, BinaryEnv, errors.Join(errs...))
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), ".gcpemu-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o755)
	}
	if err == nil {
		err = check(tmp, arch, "")
	}
	if err == nil {
		err = os.Rename(tmp, dest)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cache the linux/%s gcpemu binary: %w", arch, err)
	}
	return nil
}

// exeDirs returns the executable's directory and, when it is reached
// through a symlink (a Homebrew bin), the directory of its target.
func exeDirs(exe string) []string {
	dirs := []string{filepath.Dir(exe)}
	if real, err := filepath.EvalSymlinks(exe); err == nil && filepath.Dir(real) != dirs[0] {
		dirs = append(dirs, filepath.Dir(real))
	}
	return dirs
}

// check verifies that path is a static linux ELF executable for arch. via
// names the setting it came from, if any.
func check(path, arch, via string) error {
	name := path
	if via != "" {
		name = fmt.Sprintf("%s (%s)", path, via)
	}
	got, err := elfArch(path)
	if err != nil {
		return fmt.Errorf("%s is not a linux gcpemu binary: %w", name, err)
	}
	if got != arch {
		return fmt.Errorf("%s is a linux/%s binary, but the container runtime is linux/%s", name, got, arch)
	}
	if dynamic(path) {
		return fmt.Errorf("%s is dynamically linked, so it can't run as a Sidecar agent in containers; build it with CGO_ENABLED=0", name)
	}
	return nil
}

// elfArch returns the GOARCH of the ELF executable at path (the ELF
// machine name for architectures gcpemu is not built for).
func elfArch(path string) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	switch f.Machine {
	case elf.EM_X86_64:
		return "amd64", nil
	case elf.EM_AARCH64:
		return "arm64", nil
	}
	return strings.ToLower(strings.TrimPrefix(f.Machine.String(), "EM_")), nil
}

// dynamic reports whether the ELF executable at path needs a dynamic
// loader (it has a PT_INTERP program header).
func dynamic(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return true
		}
	}
	return false
}
