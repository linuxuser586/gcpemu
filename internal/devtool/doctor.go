package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// minJust is the version CI pins.
const minJust = "1.56.0"

// runDoctor checks the tools for building and testing gcpemu (#541);
// `gcpemu doctor` checks a host for running it. Required tools fail,
// optional ones warn and name the recipes they block. Nothing is installed.
func runDoctor([]string) error {
	var fails, warns int
	ok := func(s string) { fmt.Println("ok    " + s) }
	fail := func(s string) { fmt.Println("FAIL  " + s); fails++ }
	warn := func(s string) { fmt.Println("warn  " + s); warns++ }

	// Required.
	if out, err := output("", "just", "--version"); err != nil {
		fail("just: not found, need >= " + minJust + "; install: https://github.com/casey/just#installation")
	} else if have := strings.TrimPrefix(out, "just "); versionAtLeast(have, minJust) {
		ok("just " + have)
	} else {
		fail("just " + have + ": need >= " + minJust + "; install: https://github.com/casey/just#installation")
	}

	needGo, err := goModVersion("go.mod")
	if err != nil {
		return err
	}
	gook := false
	if _, err := exec.LookPath("go"); err != nil {
		fail("go: not found, need >= " + needGo + "; install: https://go.dev/dl")
	} else if out, err := output("", "go", "env", "GOVERSION"); err != nil {
		fail("go: " + out)
	} else {
		have, _, _ := strings.Cut(out, " ")
		have = strings.TrimPrefix(have, "go")
		if versionAtLeast(have, needGo) {
			ok("go " + have)
			gook = true
		} else {
			fail("go " + have + ": need >= " + needGo + "; install: https://go.dev/dl")
		}
	}

	if out, err := output("", "git", "--version"); err != nil {
		fail("git: not found; install: https://git-scm.com/downloads")
	} else {
		have, _, _ := strings.Cut(out, " (")
		ok(have)
	}

	// Optional: the recipes each one blocks.
	needNode, pnpmPin, err := consoleTools("console/package.json")
	if err != nil {
		return err
	}
	const consoleRecipes = "(just console, console-e2e, release)"
	if have, err := output("", "node", "--version"); err != nil {
		warn("node: not found, need >= " + needNode + " " + consoleRecipes + "; install: https://nodejs.org")
	} else if versionAtLeast(strings.TrimPrefix(have, "v"), needNode) {
		ok("node " + have)
	} else {
		warn("node " + have + ": need >= " + needNode + " " + consoleRecipes + "; install: https://nodejs.org")
	}

	if have, err := output("console", "pnpm", "--version"); err != nil {
		warn("pnpm: not found, console pins " + pnpmPin + " " + consoleRecipes + "; run: corepack enable")
	} else if major(have) == major(pnpmPin) {
		ok("pnpm " + have)
	} else {
		warn("pnpm " + have + ": console pins " + pnpmPin + " " + consoleRecipes + "; run: corepack enable")
	}

	const dockerRecipes = "(just agent, e2e, compat, tofu)"
	if _, err := exec.LookPath("docker"); err != nil {
		warn("docker: not found " + dockerRecipes + "; install Docker, Colima or Podman")
	} else if have, err := output("", "docker", "version", "--format", "{{.Server.Version}}"); err == nil && have != "" {
		ok("docker (server " + have + ")")
	} else {
		warn("docker: no daemon answers " + dockerRecipes + "; start Docker, Colima or Podman")
	}

	// The check `just race`'s precondition makes.
	if gook {
		cc, _ := output("", "go", "env", "CC")
		if cgo, _ := output("", "go", "env", "CGO_ENABLED"); cgo == "1" {
			ok("cgo (CC=" + cc + ")")
		} else {
			warn("cgo: off, no C compiler " + cc + " (just race); install one, e.g. xcode-select --install, build-essential or MinGW-w64")
		}
	}

	if have, err := output("", "npm", "--version"); err != nil {
		warn("npm: not found (just action); it comes with Node.js")
	} else {
		ok("npm " + have)
	}

	if _, err := exec.LookPath("tofu"); err != nil {
		warn("tofu: not found (just tofu, e2e); install: https://opentofu.org/docs/intro/install/")
	} else {
		ok("tofu")
	}

	// `just compat` downloads its other clients; these must be on PATH, and
	// each test skips without its own.
	var missing []string
	for _, c := range []string{"gcloud", "kubectl", "dig", "aws"} {
		if _, err := exec.LookPath(c); err != nil {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		ok("gcloud, kubectl, dig, aws")
	} else {
		warn("just compat: missing " + strings.Join(missing, ", ") + "; install: https://cloud.google.com/sdk/docs/install (gcloud components install kubectl), dig from BIND (dnsutils, bind-utils, brew install bind), the aws CLI v2 from https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html")
	}

	fmt.Println()
	fmt.Printf("%d required and %d optional tools missing or too old. For running the emulator: bin/gcpemu doctor\n", fails, warns)
	if fails > 0 {
		return errReported
	}
	return nil
}

// output runs name in dir (the current directory when empty) and returns
// its trimmed standard output, or its trimmed combined output on failure.
func output(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	// corepack must not stop to ask before downloading the pinned pnpm.
	cmd.Env = append(os.Environ(), "COREPACK_ENABLE_DOWNLOAD_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			out = append(out, ee.Stderr...)
		}
		return strings.TrimSpace(string(out)), err
	}
	return strings.TrimSpace(string(out)), nil
}

// goModVersion is the go directive of the go.mod at path.
func goModVersion(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "go "); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", s.Err()
}

// consoleTools reads the Node.js minimum (engines.node, ">=N") and the
// pnpm version packageManager pins from the console's package.json.
func consoleTools(path string) (node, pnpm string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var pkg struct {
		PackageManager string `json:"packageManager"`
		Engines        struct {
			Node string `json:"node"`
		} `json:"engines"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return "", "", fmt.Errorf("%s: %w", path, err)
	}
	node = strings.TrimSpace(strings.TrimPrefix(pkg.Engines.Node, ">="))
	pnpm = strings.TrimPrefix(pkg.PackageManager, "pnpm@")
	return node, pnpm, nil
}

func major(v string) string {
	m, _, _ := strings.Cut(v, ".")
	return m
}
