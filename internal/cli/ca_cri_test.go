package cli

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// fakeContainerd answers `containerd --version` with version and
// `containerd config dump` with dump, or fails it with dumpErr.
func fakeContainerd(version, dump string, dumpErr error) func(args ...string) ([]byte, error) {
	return func(args ...string) ([]byte, error) {
		if len(args) == 1 && args[0] == "--version" {
			return []byte(version), nil
		}
		if dumpErr != nil {
			return nil, dumpErr
		}
		return []byte(dump), nil
	}
}

const (
	containerd17 = "containerd github.com/containerd/containerd 1.7.24\n"
	// dump17 is a 1.7 `config dump` of Ubuntu's package: CRI enabled, no
	// config_path, an empty mirrors table.
	dump17 = `disabled_plugins = []
version = 2

[plugins]
  [plugins."io.containerd.grpc.v1.cri"]
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = ""
      [plugins."io.containerd.grpc.v1.cri".registry.auths]
      [plugins."io.containerd.grpc.v1.cri".registry.configs]
      [plugins."io.containerd.grpc.v1.cri".registry.headers]
      [plugins."io.containerd.grpc.v1.cri".registry.mirrors]
`
	// dump2 is a containerd 2 `config dump`, migrated to version 3, with
	// config_path CONFIG_PATH.
	dump2 = `disabled_plugins = []
version = 3

[plugins]
  [plugins.'io.containerd.cri.v1.images']
    [plugins.'io.containerd.cri.v1.images'.registry]
      config_path = 'CONFIG_PATH'
`
)

// Kubernetes (CRI) pulls read the hosts.toml files `ca install` writes
// only when containerd's effective CRI config_path lists
// /etc/containerd/certs.d (#583).
func TestCheckCRIConfigPath(t *testing.T) {
	h := testEtc(t, "containerd")
	certsD := h.etc + "/containerd/certs.d"
	file := h.etc + "/containerd/config.toml"
	restart := "add this to " + file + " (merged with that table if it is already there), then run `sudo systemctl restart containerd`:"
	for _, tc := range []struct {
		name          string
		version, dump string
		want          criCheck
	}{
		{"1.7 without config_path", containerd17, dump17, criCheck{kind: criNotIncluded, version: "1.7.24", fix: []string{
			restart,
			"  version = 2",
			`  [plugins."io.containerd.grpc.v1.cri".registry]`,
			`    config_path = "` + certsD + `"`,
		}}},
		{"2.1 without config_path", "containerd github.com/containerd/containerd/v2 v2.1.4 75cb2b7\n", strings.ReplaceAll(dump2, "CONFIG_PATH", ""),
			criCheck{kind: criNotIncluded, version: "2.1.4", fix: []string{
				restart,
				`  [plugins."io.containerd.cri.v1.images".registry]`,
				`    config_path = "` + certsD + `"`,
			}}},
		{"2.2 default", "containerd github.com/containerd/containerd/v2 v2.2.0 1c4457e\n", strings.ReplaceAll(dump2, "CONFIG_PATH", ""),
			criCheck{kind: criOK, version: "2.2.0"}},
		{"listed among others", "containerd github.com/containerd/containerd/v2 v2.0.6 x\n", strings.ReplaceAll(dump2, "CONFIG_PATH", "/opt/certs.d:"+certsD),
			criCheck{kind: criOK, version: "2.0.6"}},
		{"not listed", "containerd github.com/containerd/containerd/v2 v2.2.1 x\n", strings.ReplaceAll(dump2, "CONFIG_PATH", "/opt/certs.d:"+h.etc+"/docker/certs.d"),
			criCheck{kind: criNotIncluded, version: "2.2.1", fix: []string{
				restart,
				`  [plugins."io.containerd.cri.v1.images".registry]`,
				`    config_path = "/opt/certs.d:` + h.etc + `/docker/certs.d:` + certsD + `"`,
			}}},
		{"mirrors", containerd17, dump17 + "        [plugins.\"io.containerd.grpc.v1.cri\".registry.mirrors.\"docker.io\"]\n          endpoint = [\"https://mirror.example\"]\n",
			criCheck{kind: criNotIncluded, version: "1.7.24", fix: []string{
				"move the registry.mirrors of io.containerd.grpc.v1.cri into hosts.toml files in " + certsD + ", then set config_path to " + certsD + " and run `sudo systemctl restart containerd`",
			}}},
		{"CRI disabled", "containerd containerd.io 1.7.27 05044ec\n", "disabled_plugins = [\"cri\"]\n" + dump17[len("disabled_plugins = []\n"):],
			criCheck{kind: criDisabled, version: "1.7.27"}},
	} {
		h.containerd = fakeContainerd(tc.version, tc.dump, nil)
		if got := checkCRIConfigPath(h); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %#v, want %#v", tc.name, got, tc.want)
		}
	}
}

// When containerd cannot answer, the check says why rather than guess.
func TestCheckCRIConfigPathUnknown(t *testing.T) {
	h := testEtc(t, "containerd")
	h.containerd = func(...string) ([]byte, error) { return nil, &exec.Error{Name: "containerd", Err: exec.ErrNotFound} }
	if got, want := checkCRIConfigPath(h), (criCheck{kind: criUnknown, reason: "containerd is not on PATH"}); !reflect.DeepEqual(got, want) {
		t.Errorf("no binary: got %#v, want %#v", got, want)
	}
	h.containerd = fakeContainerd(containerd17, "", errors.New("failed to load TOML: /etc/containerd/config.toml: permission denied"))
	want := criCheck{kind: criUnknown, version: "1.7.24", reason: "`containerd config dump` failed: failed to load TOML: /etc/containerd/config.toml: permission denied"}
	if got := checkCRIConfigPath(h); !reflect.DeepEqual(got, want) {
		t.Errorf("failing dump: got %#v, want %#v", got, want)
	}
}

// After its containerd step, `ca install` prints the check's note unless
// Kubernetes (CRI) pulls already read etc/containerd/certs.d; it never
// changes containerd's config (#583).
func TestCRIInstallNote(t *testing.T) {
	h := testEtc(t, "containerd")
	certsD := h.etc + "/containerd/certs.d"
	note := func() string {
		var out strings.Builder
		criInstallNote(&out, h)
		return out.String()
	}
	h.containerd = fakeContainerd(containerd17, dump17, nil)
	want := "note: Kubernetes (CRI) image pulls by containerd 1.7.24 do not read the hosts.toml files in " + certsD +
		", as its CRI config_path does not list that folder (Docker's pulls are not affected)\n" +
		"      fix: add this to " + h.etc + "/containerd/config.toml (merged with that table if it is already there), then run `sudo systemctl restart containerd`:\n" +
		"        version = 2\n" +
		"        [plugins.\"io.containerd.grpc.v1.cri\".registry]\n" +
		"          config_path = \"" + certsD + "\"\n"
	if got := note(); got != want {
		t.Errorf("1.7: note %q, want %q", got, want)
	}
	h.containerd = fakeContainerd(containerd17, "", errors.New("boom"))
	if got, want := note(), "note: cannot check whether Kubernetes (CRI) image pulls by containerd read "+certsD+": `containerd config dump` failed: boom\n"; got != want {
		t.Errorf("failing dump: note %q, want %q", got, want)
	}
	h.containerd = fakeContainerd("containerd github.com/containerd/containerd/v2 v2.2.0 x\n", strings.ReplaceAll(dump2, "CONFIG_PATH", ""), nil)
	if got := note(); got != "" {
		t.Errorf("2.2: note %q, want none", got)
	}
	for name, h := range map[string]trustHost{"no etc/containerd": testEtc(t, "docker"), "darwin": {goos: "darwin"}} {
		h.containerd = fakeContainerd(containerd17, dump17, nil)
		var out strings.Builder
		criInstallNote(&out, h)
		if out.Len() != 0 {
			t.Errorf("%s: note %q, want none", name, out.String())
		}
	}
}

// With mirrors next to a config_path that lists other folders (containerd
// 2 allows both), the advice adds etc/containerd/certs.d to the list.
func TestCheckCRIConfigPathMirrorsWithList(t *testing.T) {
	h := testEtc(t, "containerd")
	certsD := h.etc + "/containerd/certs.d"
	dump := strings.ReplaceAll(dump2, "CONFIG_PATH", "/opt/certs.d") + "      [plugins.'io.containerd.cri.v1.images'.registry.mirrors.'docker.io']\n        endpoint = ['https://mirror.example']\n"
	h.containerd = fakeContainerd("containerd github.com/containerd/containerd/v2 v2.1.4 x\n", dump, nil)
	want := []string{"move the registry.mirrors of io.containerd.cri.v1.images into hosts.toml files in " + certsD + ", then add " + certsD + " to config_path and run `sudo systemctl restart containerd`"}
	if got := checkCRIConfigPath(h); got.kind != criNotIncluded || !reflect.DeepEqual(got.fix, want) {
		t.Errorf("got %#v, want fix %q", got, want)
	}
}
