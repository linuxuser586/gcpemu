package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestDynamic: a CGO_ENABLED=0 build is static; a cgo build needs the
// dynamic loader and is refused as an in-container agent.
func TestDynamic(t *testing.T) {
	if runtime.GOOS != "linux" || testing.Short() {
		t.Skip("builds ELF binaries")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	_ = os.WriteFile(src, []byte("package main\n\nimport \"C\"\n\nfunc main() {}\n"), 0o644)
	build := func(cgo, out string, file string) {
		t.Helper()
		cmd := exec.Command("go", "build", "-o", out, file)
		cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgo, "GOFLAGS=")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("go build (CGO_ENABLED=%s): %v\n%s", cgo, err, b)
		}
	}
	dyn := filepath.Join(dir, "dyn")
	build("1", dyn, src)
	if !dynamic(dyn) {
		t.Error("cgo binary not reported dynamic")
	}
	plain := filepath.Join(dir, "plain.go")
	_ = os.WriteFile(plain, []byte("package main\n\nfunc main() {}\n"), 0o644)
	static := filepath.Join(dir, "static")
	build("0", static, plain)
	if dynamic(static) {
		t.Error("CGO_ENABLED=0 binary reported dynamic")
	}
}
