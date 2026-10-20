package cli

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// criKind is what checkCRIConfigPath found.
type criKind int

const (
	// criOK: CRI pulls read etc/containerd/certs.d.
	criOK criKind = iota
	// criNotIncluded: CRI's config_path does not list it; fix says how.
	criNotIncluded
	// criDisabled: containerd's CRI plugin is off, as in Docker's
	// containerd.io package, so there are no CRI pulls to report.
	criDisabled
	// criUnknown: containerd could not say; reason says why.
	criUnknown
)

// criCheck is whether image pulls by a kubelet on the host, through the
// host's containerd's CRI, read the hosts.toml files `ca install` writes in
// etc/containerd/certs.d (#583).
type criCheck struct {
	kind    criKind
	version string   // containerd's, as `containerd --version` gives it
	fix     []string // criNotIncluded: the fix, a line each
	reason  string   // criUnknown
}

// criPlugins are the names containerd's disabled_plugins can give its
// CRI plugin, or the plugin that holds the CRI registry settings.
var criPlugins = []string{"cri", "io.containerd.grpc.v1.cri", "io.containerd.cri.v1.images"}

var containerdVersion = regexp.MustCompile(`^v?(\d+)\.(\d+)\.\S*$`)

// runContainerd runs the containerd binary, which answers --version and
// `config dump` (its merged config, imports included) without a running
// daemon.
func runContainerd(args ...string) ([]byte, error) {
	b, err := exec.Command("containerd", args...).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// The last line of stderr is the error, after any warnings.
		lines := strings.Split(strings.TrimSpace(string(exit.Stderr)), "\n")
		if l := strings.TrimPrefix(strings.TrimSpace(lines[len(lines)-1]), "containerd: "); l != "" {
			err = errors.New(l)
		}
	}
	return b, err
}

// checkCRIConfigPath reads the CRI registry config_path and mirrors from
// `containerd config dump`, under the plugin of the version `containerd
// --version` gives (containerd 2 dumps its config migrated to the newest
// version), rather than parsing containerd's config files and imports.
// An empty config_path with no mirrors means etc/containerd/certs.d and
// etc/docker/certs.d on Linux from containerd 2.2, which applies it when
// the plugin starts, so the dump does not show it; before 2.2 CRI then
// reads no certs.d at all. Trust needs etc/containerd/certs.d itself in
// the list. Mirrors are the deprecated alternative to config_path, so
// with them the fix is to move them into hosts.toml files first.
func checkCRIConfigPath(h trustHost) criCheck {
	etc := h.etc
	if etc == "" {
		etc = "/etc"
	}
	run := h.containerd
	out, err := run("--version")
	if err != nil {
		return criCheck{kind: criUnknown, reason: containerdError("--version", err)}
	}
	var version string
	major, minor := -1, 0
	for _, f := range strings.Fields(string(out)) {
		if m := containerdVersion.FindStringSubmatch(f); m != nil {
			version = strings.TrimPrefix(f, "v")
			major, _ = strconv.Atoi(m[1])
			minor, _ = strconv.Atoi(m[2])
			break
		}
	}
	if major < 0 {
		return criCheck{kind: criUnknown, reason: fmt.Sprintf("`containerd --version` gave no version: %q", strings.TrimSpace(string(out)))}
	}
	c := criCheck{version: version}
	out, err = run("config", "dump")
	if err != nil {
		return criCheck{kind: criUnknown, version: version, reason: containerdError("config dump", err)}
	}
	var dump struct {
		DisabledPlugins []string `toml:"disabled_plugins"`
		Plugins         map[string]struct {
			Registry struct {
				ConfigPath string         `toml:"config_path"`
				Mirrors    map[string]any `toml:"mirrors"`
			} `toml:"registry"`
		} `toml:"plugins"`
	}
	if _, err := toml.Decode(string(out), &dump); err != nil {
		return criCheck{kind: criUnknown, version: version, reason: "cannot read `containerd config dump`: " + err.Error()}
	}
	for _, p := range dump.DisabledPlugins {
		for _, cri := range criPlugins {
			if p == cri {
				return criCheck{kind: criDisabled, version: version}
			}
		}
	}
	// A config file names the plugin by its own version, which can be
	// older than the daemon's, so the fix says what to do in such a file.
	plugin, header := "io.containerd.cri.v1.images", "version = 3"
	where := "skip `version = 3` if the file already has it; in a file with `version = 2`, use `[plugins.\"io.containerd.grpc.v1.cri\".registry]` instead; merge with that table if it is already there"
	if major < 2 {
		plugin, header = "io.containerd.grpc.v1.cri", "version = 2"
		where = "skip `version = 2` if the file already has it, and merge with that table if it is already there"
	}
	reg := dump.Plugins[plugin].Registry
	certsD := etc + "/containerd/certs.d"
	configPath := reg.ConfigPath
	if configPath == "" && len(reg.Mirrors) == 0 && h.goos == "linux" && (major > 2 || major == 2 && minor >= 2) {
		configPath = certsD + ":" + etc + "/docker/certs.d"
	}
	var dirs []string
	if configPath != "" {
		dirs = strings.Split(configPath, ":")
	}
	for _, d := range dirs {
		if d == certsD {
			c.kind = criOK
			return c
		}
	}
	c.kind = criNotIncluded
	if len(reg.Mirrors) > 0 {
		set := "set config_path to " + certsD
		if len(dirs) > 0 {
			set = "add " + certsD + " to config_path"
		}
		c.fix = []string{fmt.Sprintf("move the registry.mirrors of %s into hosts.toml files in %s, then %s and run `sudo systemctl restart containerd`", plugin, certsD, set)}
		return c
	}
	c.fix = []string{"add this to " + etc + "/containerd/config.toml (" + where + "), then run `sudo systemctl restart containerd`:", "  " + header}
	c.fix = append(c.fix,
		`  [plugins."`+plugin+`".registry]`,
		`    config_path = "`+strings.Join(append(dirs, certsD), ":")+`"`)
	return c
}

// containerdError is why `containerd ARGS` could not answer.
func containerdError(args string, err error) string {
	if errors.Is(err, exec.ErrNotFound) {
		return "containerd is not on PATH"
	}
	return "`containerd " + args + "` failed: " + err.Error()
}

// criMatters says whom the CRI line concerns: a kubelet on the host,
// which pulls through containerd's CRI. Emulated GKE clusters' nodes pull
// with their own registry settings, nerdctl reads etc/containerd/certs.d
// whatever config_path says, and Docker reads only etc/docker/certs.d.
const criMatters = "this matters only if such a kubelet pulls from the emulated registry (emulated GKE clusters and Docker are not affected)"

// writeCRICheck writes c for etc as a doctor line (doctor) or as a `ca
// install` note; nothing when CRI is disabled.
func writeCRICheck(out io.Writer, c criCheck, etc string, doctor bool) {
	if etc == "" {
		etc = "/etc"
	}
	certsD := etc + "/containerd/certs.d"
	var line string
	switch c.kind {
	case criDisabled:
		return
	case criOK:
		line = "image pulls by a kubelet on this host (containerd " + c.version + "'s CRI) read the hosts.toml files in " + certsD
	case criNotIncluded:
		line = "image pulls by a kubelet on this host (containerd " + c.version + "'s CRI) do not read the hosts.toml files in " + certsD +
			", as CRI's config_path does not list that folder; " + criMatters
	case criUnknown:
		line = "cannot check whether image pulls by a kubelet on this host (containerd's CRI) read " + certsD + ": " + c.reason + "; " + criMatters
	}
	lead, indent := "note: ", "      "
	if doctor {
		lead, indent = "[--  ] ", "       "
		if c.kind == criOK {
			lead = "[ok  ] "
		}
	}
	fmt.Fprintln(out, lead+line)
	for i, l := range c.fix {
		if i == 0 {
			l = "fix: " + l
		}
		fmt.Fprintln(out, indent+l)
	}
}

// criInstallNote writes, after `ca install`'s containerd step, the note
// for checkCRIConfigPath unless CRI pulls already read the hosts.toml
// files (or CRI is disabled). It only prints the fix: changing
// config_path takes a containerd restart, which is the user's call.
func criInstallNote(out io.Writer, h trustHost) {
	etc := h.etc
	if etc == "" {
		etc = "/etc"
	}
	if h.goos != "linux" || !h.isDir(etc+"/containerd") {
		return
	}
	if c := checkCRIConfigPath(h); c.kind != criOK {
		writeCRICheck(out, c, etc, false)
	}
}
