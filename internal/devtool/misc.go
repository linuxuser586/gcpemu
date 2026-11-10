package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runVet is the part of `just vet` that go vet doesn't do: gofmt, and ADR
// 0015 (#419), only Windows builds link go-winio.
func runVet([]string) error {
	out, err := output("", "gofmt", "-l", ".")
	if err != nil {
		return fmt.Errorf("gofmt: %v\n%s", err, out)
	}
	if out != "" {
		fmt.Println(out)
		return errReported
	}
	// The go-winio packages (the module's subpackages too) that a linux or
	// darwin build of the binary depends on.
	const tmpl = `{{with .Module}}{{if eq .Path "github.com/Microsoft/go-winio"}}{{$.ImportPath}}{{end}}{{end}}`
	for _, goos := range []string{"linux", "darwin"} {
		cmd := exec.Command("go", "list", "-deps", "-f", tmpl, "./cmd/gcpemu")
		cmd.Env = append(os.Environ(), "GOOS="+goos)
		cmd.Stderr = os.Stderr
		b, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("go list: %w", err)
		}
		if deps := strings.TrimSpace(string(b)); deps != "" {
			fmt.Println(deps)
			return fmt.Errorf("the GOOS=%s build of ./cmd/gcpemu links github.com/Microsoft/go-winio; ADR 0015 restricts it to Windows builds", goos)
		}
	}
	return nil
}

// runDockerArch prints the container runtime's architecture, or the Go
// toolchain's GOARCH when docker doesn't answer: the arch `just agent`
// builds for.
func runDockerArch([]string) error {
	arch, err := output("", "docker", "version", "--format", "{{.Server.Arch}}")
	if err != nil || arch == "" {
		if arch, err = output("", "go", "env", "GOARCH"); err != nil {
			return fmt.Errorf("go env GOARCH: %v: %s", err, arch)
		}
	}
	fmt.Println(arch)
	return nil
}

// runNeedCgo is `just race`'s precondition: the race detector needs cgo,
// which Go turns off without a C compiler.
func runNeedCgo([]string) error {
	if cgo, _ := output("", "go", "env", "CGO_ENABLED"); cgo == "1" {
		return nil
	}
	cc, _ := output("", "go", "env", "CC")
	fmt.Fprintf(os.Stderr, "race: the race detector needs cgo and a C compiler (%s); install one, or run just test\n", cc)
	return errReported
}

// runVersion prints the version a build stamps into the binary.
func runVersion([]string) error {
	v, err := output("", "git", "describe", "--tags", "--always", "--dirty")
	if err != nil || v == "" {
		v = "dev"
	}
	fmt.Println(v)
	return nil
}

// runWhich prints the path of the program named on PATH, or nothing: just's
// own which() needs the unstable `set lists` in just 1.56.
func runWhich(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: which <program>")
	}
	if p, err := exec.LookPath(args[0]); err == nil {
		fmt.Println(p)
	}
	return nil
}
