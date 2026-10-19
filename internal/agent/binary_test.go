package agent

import (
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeELF returns a minimal ELF64 executable header for machine, with a
// PT_INTERP program header when dyn is set.
func fakeELF(machine elf.Machine, dyn bool) []byte {
	var b bytes.Buffer
	h := elf.Header64{
		Type: uint16(elf.ET_EXEC), Machine: uint16(machine), Version: uint32(elf.EV_CURRENT),
		Ehsize: 64, Phentsize: 56,
	}
	copy(h.Ident[:], elf.ELFMAG)
	h.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	h.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	h.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	if dyn {
		h.Phoff, h.Phnum = 64, 1
	}
	_ = binary.Write(&b, binary.LittleEndian, h)
	if dyn {
		_ = binary.Write(&b, binary.LittleEndian, elf.Prog64{Type: uint32(elf.PT_INTERP)})
	}
	return b.Bytes()
}

var machines = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

func writeELF(t *testing.T, path, arch string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fakeELF(machines[arch], false), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeImages serves the published image's binary from a map of refs.
type fakeImages struct {
	files map[string][]byte // ref → /gcpemu
	pulls []string          // ref@platform
}

func (f *fakeImages) ImageFile(_ context.Context, ref, platform, file string) ([]byte, error) {
	f.pulls = append(f.pulls, ref+"@"+platform)
	if file != "/gcpemu" {
		return nil, errors.New("unexpected file " + file)
	}
	b, ok := f.files[ref]
	if !ok {
		return nil, errors.New("manifest unknown")
	}
	return b, nil
}

// env is a darwin host with gcpemu at dir/bin/gcpemu.
type env struct {
	dir, exe string
	vars     map[string]string
	images   *fakeImages
}

func newEnv(t *testing.T) *env {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{dir: dir, exe: filepath.Join(dir, "bin", "gcpemu"), vars: map[string]string{}, images: &fakeImages{files: map[string][]byte{}}}
	if err := os.MkdirAll(filepath.Dir(e.exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.exe, []byte("mach-o"), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) resolver(arch, version string) *Resolver {
	return &Resolver{
		Arch: arch, Version: version, CacheDir: filepath.Join(e.dir, "cache"), Images: e.images,
		goos:       "darwin",
		executable: func() (string, error) { return e.exe, nil },
		getenv:     func(k string) string { return e.vars[k] },
	}
}

func (e *env) cached(version, arch string) string {
	return filepath.Join(e.dir, "cache", version, "linux-"+arch, "gcpemu")
}

func TestReleasePullsAndCaches(t *testing.T) {
	e := newEnv(t)
	e.images.files["ghcr.io/linuxuser586/gcpemu:1.2.3"] = fakeELF(elf.EM_AARCH64, false)
	p, err := e.resolver("arm64", "v1.2.3").Binary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := e.cached("v1.2.3", "arm64"); p != want {
		t.Fatalf("path = %s, want %s", p, want)
	}
	if got := strings.Join(e.images.pulls, ","); got != "ghcr.io/linuxuser586/gcpemu:1.2.3@linux/arm64" {
		t.Fatalf("pulls = %s", got)
	}
	// Windows has no exec bit; the tar header into the container sets
	// the mode anyway.
	if fi, err := os.Stat(p); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("cached binary not executable: %v %v", fi, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".gcpemu-*")); len(left) > 0 {
		t.Fatalf("temporary files left: %v", left)
	}

	// A second start uses the cache.
	e.images.pulls = nil
	if p2, err := e.resolver("arm64", "v1.2.3").Binary(context.Background()); err != nil || p2 != p {
		t.Fatalf("second start = %s, %v", p2, err)
	}
	if len(e.images.pulls) > 0 {
		t.Fatalf("second start pulled %v", e.images.pulls)
	}
}

func TestReleaseFallsBackToDockerHub(t *testing.T) {
	e := newEnv(t)
	e.images.files["docker.io/linuxuser586/gcpemu:1.2.3"] = fakeELF(elf.EM_X86_64, false)
	p, err := e.resolver("amd64", "v1.2.3").Binary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p != e.cached("v1.2.3", "amd64") {
		t.Fatalf("path = %s", p)
	}
	want := "ghcr.io/linuxuser586/gcpemu:1.2.3@linux/amd64,docker.io/linuxuser586/gcpemu:1.2.3@linux/amd64"
	if got := strings.Join(e.images.pulls, ","); got != want {
		t.Fatalf("pulls = %s, want %s", got, want)
	}
}

func TestReleasePullFailsNamesBothImages(t *testing.T) {
	e := newEnv(t)
	r := e.resolver("arm64", "v1.2.3")
	_, err := r.Binary(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ghcr.io/") || !strings.Contains(err.Error(), "docker.io/") {
		t.Fatalf("err = %v", err)
	}
	// A failure is not remembered.
	e.images.files["ghcr.io/linuxuser586/gcpemu:1.2.3"] = fakeELF(elf.EM_AARCH64, false)
	if _, err := r.Binary(context.Background()); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestPulledBinaryWrongArchIsNotCached(t *testing.T) {
	e := newEnv(t)
	e.images.files["ghcr.io/linuxuser586/gcpemu:1.2.3"] = fakeELF(elf.EM_X86_64, false)
	_, err := e.resolver("arm64", "v1.2.3").Binary(context.Background())
	if err == nil || !strings.Contains(err.Error(), "linux/amd64") || !strings.Contains(err.Error(), "linux/arm64") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(e.cached("v1.2.3", "arm64")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrong binary cached: %v", err)
	}
}

func TestReleaseOffline(t *testing.T) {
	e := newEnv(t)
	e.images.files["ghcr.io/linuxuser586/gcpemu:1.2.3"] = fakeELF(elf.EM_AARCH64, false)
	r := e.resolver("arm64", "v1.2.3")
	r.Offline = true
	if _, err := r.Binary(context.Background()); err == nil || !strings.Contains(err.Error(), "--offline") {
		t.Fatalf("err = %v", err)
	}
	if len(e.images.pulls) > 0 {
		t.Fatalf("offline pulled %v", e.images.pulls)
	}
}

func TestSiblingBeforeCacheAndPull(t *testing.T) {
	e := newEnv(t)
	sib := writeELF(t, filepath.Join(e.dir, "bin", "gcpemu-linux-arm64"), "arm64")
	writeELF(t, e.cached("v1.2.3", "arm64"), "arm64")
	e.images.files["ghcr.io/linuxuser586/gcpemu:1.2.3"] = fakeELF(elf.EM_AARCH64, false)
	p, err := e.resolver("arm64", "v1.2.3").Binary(context.Background())
	if err != nil || p != sib {
		t.Fatalf("path = %s, %v; want %s", p, err, sib)
	}
	if len(e.images.pulls) > 0 {
		t.Fatalf("pulled %v with a sibling present", e.images.pulls)
	}
}

func TestSiblingOfSymlinkTarget(t *testing.T) {
	e := newEnv(t)
	sib := writeELF(t, filepath.Join(e.dir, "bin", "gcpemu-linux-arm64"), "arm64")
	link := filepath.Join(e.dir, "links", "gcpemu")
	_ = os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(e.exe, link); err != nil {
		t.Skip(err)
	}
	e.exe = link
	p, err := e.resolver("arm64", "dev").Binary(context.Background())
	if err != nil || p != sib {
		t.Fatalf("path = %s, %v; want %s", p, err, sib)
	}
}

func TestEnvBeforeSibling(t *testing.T) {
	e := newEnv(t)
	writeELF(t, filepath.Join(e.dir, "bin", "gcpemu-linux-arm64"), "arm64")
	e.vars[BinaryEnv] = writeELF(t, filepath.Join(e.dir, "env", "gcpemu"), "arm64")
	p, err := e.resolver("arm64", "dev").Binary(context.Background())
	if err != nil || p != e.vars[BinaryEnv] {
		t.Fatalf("path = %s, %v", p, err)
	}
}

func TestCacheBeforePull(t *testing.T) {
	e := newEnv(t)
	c := writeELF(t, e.cached("v1.2.3", "amd64"), "amd64")
	p, err := e.resolver("amd64", "v1.2.3").Binary(context.Background())
	if err != nil || p != c {
		t.Fatalf("path = %s, %v", p, err)
	}
	if len(e.images.pulls) > 0 {
		t.Fatalf("pulled %v with a cached binary", e.images.pulls)
	}
}

func TestArchMismatch(t *testing.T) {
	for _, tc := range []struct{ name, via string }{{"env", BinaryEnv}, {"sibling", ""}, {"cache", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			switch tc.name {
			case "env":
				e.vars[BinaryEnv] = writeELF(t, filepath.Join(e.dir, "env", "gcpemu"), "amd64")
			case "sibling":
				writeELF(t, filepath.Join(e.dir, "bin", "gcpemu-linux-arm64"), "amd64")
			case "cache":
				writeELF(t, e.cached("v1.2.3", "arm64"), "amd64")
			}
			_, err := e.resolver("arm64", "v1.2.3").Binary(context.Background())
			if err == nil || !strings.Contains(err.Error(), "linux/amd64 binary, but the container runtime is linux/arm64") ||
				!strings.Contains(err.Error(), tc.via) {
				t.Fatalf("err = %v", err)
			}
			if len(e.images.pulls) > 0 {
				t.Fatalf("pulled %v", e.images.pulls)
			}
		})
	}
}

func TestDynamicSiblingRefused(t *testing.T) {
	e := newEnv(t)
	p := filepath.Join(e.dir, "bin", "gcpemu-linux-arm64")
	if err := os.WriteFile(p, fakeELF(elf.EM_AARCH64, true), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.resolver("arm64", "dev").Binary(context.Background()); err == nil || !strings.Contains(err.Error(), "dynamically linked") {
		t.Fatalf("err = %v", err)
	}
}

func TestDevBuildHintsTaskAgent(t *testing.T) {
	for _, v := range []string{"dev", "v1.2.3-4-gabcdef0", "v1.2.3-dirty", "abcdef0"} {
		e := newEnv(t)
		e.images.files["ghcr.io/linuxuser586/gcpemu:"+v] = fakeELF(elf.EM_AARCH64, false)
		_, err := e.resolver("arm64", v).Binary(context.Background())
		if err == nil || !strings.Contains(err.Error(), "task agent") || !strings.Contains(err.Error(), BinaryEnv) {
			t.Fatalf("%s: err = %v", v, err)
		}
		if len(e.images.pulls) > 0 {
			t.Fatalf("%s: pulled %v", v, e.images.pulls)
		}
	}
}

func TestLinuxHostSameArchUsesExecutable(t *testing.T) {
	e := newEnv(t)
	e.exe = writeELF(t, filepath.Join(e.dir, "bin", "gcpemu"), runtime.GOARCH)
	r := e.resolver(runtime.GOARCH, "dev")
	r.goos = "linux"
	if p, err := r.Binary(context.Background()); err != nil || p != e.exe {
		t.Fatalf("path = %s, %v", p, err)
	}
}

func TestLinuxHostOtherArchUsesSibling(t *testing.T) {
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	e := newEnv(t)
	e.exe = writeELF(t, filepath.Join(e.dir, "bin", "gcpemu"), runtime.GOARCH)
	sib := writeELF(t, filepath.Join(e.dir, "bin", "gcpemu-linux-"+other), other)
	r := e.resolver(other, "dev")
	r.goos = "linux"
	if p, err := r.Binary(context.Background()); err != nil || p != sib {
		t.Fatalf("path = %s, %v; want %s", p, err, sib)
	}
}

func TestRelease(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.2.3": true, "v0.10.0": true, "dev": false, "1.2.3": false,
		"v1.2.3-rc.1": false, "v1.2.3-dirty": false, "v1.2": false, "abcdef0": false,
	} {
		if got := Release(v); got != want {
			t.Errorf("Release(%q) = %v", v, got)
		}
	}
}
