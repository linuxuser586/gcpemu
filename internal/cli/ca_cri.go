package cli

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// containerd reads the hosts.toml files `ca install` writes under
// /etc/containerd/certs.d for Kubernetes (CRI) image pulls only when the
// CRI registry config_path lists that folder (#583). nerdctl reads them
// whatever config_path says, and Docker never does.

const containerdCertsD = "/etc/containerd/certs.d"

// criState is the outcome of checkCRIConfigPath.
type criState int

const (
	criNone     criState = iota // not a Linux host with /etc/containerd
	criOK                       // config_path lists containerdCertsD
	criMissing                  // it does not: fix says how to add it
	criDisabled                 // the CRI plugin is disabled
	criUnknown                  // cannot tell: reason says why
)

// criConfigPath is what checkCRIConfigPath found.
type criConfigPath struct {
	state   criState
	version string // containerd's, as `containerd --version` prints it
	fix     string // criMissing
	reason  string // criUnknown
}

// checkCRIConfigPath reports whether containerd's effective CRI registry
// config_path includes containerdCertsD, from `containerd --version` and
// the merged config `containerd config dump` prints (imports included),
// on a Linux host with /etc/containerd.
func checkCRIConfigPath(h trustHost) criConfigPath {
	etc := h.etc
	if etc == "" {
		etc = "/etc"
	}
	if h.goos != "linux" || !h.isDir(etc+"/containerd") {
		return criConfigPath{}
	}
	b, err := h.containerd("--version")
	if err != nil {
		return criConfigPath{state: criUnknown, reason: "`containerd --version` failed: " + err.Error()}
	}
	m := containerdVersion.FindSubmatch(b)
	if m == nil {
		return criConfigPath{state: criUnknown, reason: fmt.Sprintf("no version in `containerd --version` output %q", b)}
	}
	r := criConfigPath{version: string(m[1])}
	major, _ := strconv.Atoi(string(m[2]))
	minor, _ := strconv.Atoi(string(m[3]))
	b, err = h.containerd("config", "dump")
	if err != nil {
		r.state, r.reason = criUnknown, "`containerd config dump` failed: "+err.Error()
		return r
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
	if _, err := toml.Decode(string(b), &dump); err != nil {
		r.state, r.reason = criUnknown, "cannot read `containerd config dump` output: "+err.Error()
		return r
	}
	for _, p := range dump.DisabledPlugins {
		if p == "cri" || p == "io.containerd.grpc.v1.cri" || p == "io.containerd.cri.v1.images" {
			r.state = criDisabled
			return r
		}
	}
	plugin := "io.containerd.grpc.v1.cri"
	if major >= 2 {
		plugin = "io.containerd.cri.v1.images"
	}
	reg := dump.Plugins[plugin].Registry
	paths := reg.ConfigPath
	if paths == "" && len(reg.Mirrors) == 0 && (major > 2 || major == 2 && minor >= 2) {
		// 2.2's default, applied when the plugin starts from 2.2.2,
		// so the dump does not show it.
		paths = containerdCertsD + ":/etc/docker/certs.d"
	}
	if slices.Contains(strings.Split(paths, ":"), containerdCertsD) {
		r.state = criOK
		return r
	}
	r.state = criMissing
	switch {
	case len(reg.Mirrors) > 0:
		r.fix = "containerd's config.toml sets registry.mirrors, which containerd uses instead of certs.d: move those mirrors into hosts.toml files under " +
			containerdCertsD + " (https://github.com/containerd/containerd/blob/main/docs/hosts.md), make config_path include " + containerdCertsD +
			", keeping any folders it lists, then run " + criRestart + "\n"
	case paths == "":
		r.fix = criEdit + criRestart + ":\n" + criStanza(major, plugin, containerdCertsD)
	default:
		r.fix = "add " + containerdCertsD + " to the folders config_path lists: " + criEdit + criRestart + ":\n" +
			criStanza(major, plugin, paths+":"+containerdCertsD)
	}
	return r
}

const criRestart = "`sudo systemctl restart containerd`"

// criEdit introduces the stanza: pasting it into a config.toml that
// already has a version line or the table would stop containerd starting.
const criEdit = "make containerd's config.toml read as below, editing its version line and this table where they exist rather than adding them again, then run "

// containerdVersion finds the version in `containerd --version` output,
// such as "containerd github.com/containerd/containerd/v2 v2.2.0 HASH".
var containerdVersion = regexp.MustCompile(`\sv?((\d+)\.(\d+)\.\d+\S*)`)

// criStanza is the config.toml stanza that sets config_path to paths.
func criStanza(major int, plugin, paths string) string {
	version := "version = 2"
	if major >= 2 {
		version = "version = 3"
	}
	return version + "\n\n[plugins.\"" + plugin + "\".registry]\n  config_path = \"" + paths + "\"\n"
}

// write prints r as a doctor line; criNone and criDisabled print nothing.
func (r criConfigPath) write(out io.Writer) {
	switch r.state {
	case criOK:
		fmt.Fprintf(out, "[ok  ] Kubernetes (CRI) image pulls on containerd %s read %s (Docker pulls are unaffected)\n", r.version, containerdCertsD)
	case criMissing:
		fmt.Fprintf(out, "[--  ] Kubernetes (CRI) image pulls on containerd %s do not read %s, so they ignore its hosts.toml files (Docker pulls are unaffected)\n", r.version, containerdCertsD)
		for i, l := range strings.Split(strings.TrimSuffix(r.fix, "\n"), "\n") {
			switch {
			case i == 0:
				fmt.Fprintln(out, "       fix: "+l)
			case l == "":
				fmt.Fprintln(out)
			default:
				fmt.Fprintln(out, "         "+l)
			}
		}
	case criUnknown:
		fmt.Fprintf(out, "[--  ] cannot check whether Kubernetes (CRI) image pulls on containerd read %s: %s\n", containerdCertsD, r.reason)
	}
}

// runContainerd runs the containerd binary, as trustHost.containerd; a
// failure carries the last line of its stderr.
func runContainerd(args ...string) ([]byte, error) {
	c := exec.Command("containerd", args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	b, err := c.Output()
	if err != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			err = fmt.Errorf("%w: %s", err, s[strings.LastIndex(s, "\n")+1:])
		}
		return nil, err
	}
	return b, nil
}

// criInstallNote prints, after `ca install`'s containerd step, doctor's
// note when Kubernetes (CRI) pulls will not read the hosts.toml files it
// wrote. containerd's config is never changed.
func criInstallNote(out io.Writer, h trustHost) {
	if r := checkCRIConfigPath(h); r.state == criMissing || r.state == criUnknown {
		r.write(out)
	}
}
