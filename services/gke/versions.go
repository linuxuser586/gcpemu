package gke

import (
	"fmt"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Kubernetes versions (FR-GKE-002). Each GKE version maps to the k3s image
// that runs it. The table covers the three most recent minors; images are
// pulled on first use. Digest pinning is a release task.
type k8sVersion struct {
	GKE   string // "1.36.5-gke.100"
	Image string // "rancher/k3s:v1.36.5-k3s1"
	// Preload lists the system images (pause, CoreDNS, local-path
	// provisioner, servicelb) this k3s release runs. They are exported from
	// the host image cache into every node so that nodes without egress
	// (private clusters) still start, and so new clusters need no pulls.
	Preload []string
}

// versions is ordered newest first. Preload lists come from each k3s
// release's k3s-images.txt, minus the components the emulator disables
// (traefik, metrics-server and the helm controller that installs them).
var versions = []k8sVersion{
	{GKE: "1.37.1-gke.100", Image: "rancher/k3s:v1.37.1-k3s1", Preload: systemImages("1.14.7")},
	{GKE: "1.37.0-gke.100", Image: "rancher/k3s:v1.37.0-k3s1", Preload: systemImages("1.14.7")},
	{GKE: "1.36.5-gke.100", Image: "rancher/k3s:v1.36.5-k3s1", Preload: systemImages("1.14.7")},
	{GKE: "1.36.4-gke.100", Image: "rancher/k3s:v1.36.4-k3s1", Preload: systemImages("1.14.6")},
	{GKE: "1.35.9-gke.100", Image: "rancher/k3s:v1.35.9-k3s1", Preload: systemImages("1.14.7")},
	{GKE: "1.35.8-gke.100", Image: "rancher/k3s:v1.35.8-k3s1", Preload: systemImages("1.14.6")},
}

// systemImages lists the system images of a k3s release: pause, CoreDNS,
// the local-path provisioner and its helper (busybox), and servicelb.
func systemImages(coredns string) []string {
	return []string{
		"rancher/mirrored-pause:3.10.2",
		"rancher/mirrored-coredns-coredns:" + coredns,
		"rancher/local-path-provisioner:v0.0.37",
		"rancher/mirrored-library-busybox:1.37.0",
		"rancher/klipper-lb:v0.4.17",
	}
}

// defaultVersion is the REGULAR channel default and the version used when
// a cluster names none.
const defaultVersion = "1.36.5-gke.100"

// channelDefaults maps release channels to their default versions.
var channelDefaults = map[containerpb.ReleaseChannel_Channel]string{
	containerpb.ReleaseChannel_RAPID:    "1.37.1-gke.100",
	containerpb.ReleaseChannel_REGULAR:  defaultVersion,
	containerpb.ReleaseChannel_STABLE:   "1.35.9-gke.100",
	containerpb.ReleaseChannel_EXTENDED: "1.35.9-gke.100",
}

// channelVersions lists the versions a channel offers (newest first).
func channelVersions(ch containerpb.ReleaseChannel_Channel) []string {
	var minors []string
	switch ch {
	case containerpb.ReleaseChannel_RAPID:
		minors = []string{"1.37", "1.36", "1.35"}
	case containerpb.ReleaseChannel_REGULAR:
		minors = []string{"1.36", "1.35"}
	default:
		minors = []string{"1.35"}
	}
	var out []string
	for _, v := range versions {
		for _, m := range minors {
			if strings.HasPrefix(v.GKE, m+".") {
				out = append(out, v.GKE)
			}
		}
	}
	return out
}

// lookupVersion returns the table entry for a full GKE version.
func lookupVersion(v string) (k8sVersion, bool) {
	for _, e := range versions {
		if e.GKE == v {
			return e, true
		}
	}
	return k8sVersion{}, false
}

// resolveVersion turns a requested version ("", "latest", "-", "1.36",
// "1.36.5", "1.36.5-gke.100") into a full GKE version, honouring the
// release channel default when nothing is requested.
func resolveVersion(req string, ch containerpb.ReleaseChannel_Channel) (string, error) {
	switch req {
	case "", "-":
		if d, ok := channelDefaults[ch]; ok {
			return d, nil
		}
		return defaultVersion, nil
	case "latest":
		return versions[0].GKE, nil
	}
	for _, e := range versions {
		if e.GKE == req {
			return e.GKE, nil
		}
	}
	// Prefix match on minor or patch: the newest matching version wins.
	for _, e := range versions {
		if strings.HasPrefix(e.GKE, req+".") || strings.HasPrefix(e.GKE, req+"-") {
			return e.GKE, nil
		}
	}
	return "", apierr.InvalidArgument("Version %q is invalid.", req)
}

// minorOf returns "1.36" for "1.36.5-gke.100".
func minorOf(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// checkSkew validates a node version against the master version: nodes may
// not be newer than the control plane (GKE's version skew policy).
func checkSkew(master, node string) error {
	if compareVersions(node, master) > 0 {
		return apierr.InvalidArgument("Node version %q must not be newer than the master version %q.", node, master)
	}
	return nil
}

// compareVersions orders GKE versions by their numeric components.
func compareVersions(a, b string) int {
	pa, pb := versionNums(a), versionNums(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return len(pa) - len(pb)
}

func versionNums(v string) []int {
	var out []int
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r < '0' || r > '9' }) {
		var n int
		_, _ = fmt.Sscan(f, &n)
		out = append(out, n)
	}
	return out
}

// serverConfig builds the getServerConfig response.
func serverConfig() *containerpb.ServerConfig {
	all := make([]string, 0, len(versions))
	for _, v := range versions {
		all = append(all, v.GKE)
	}
	sc := &containerpb.ServerConfig{
		DefaultClusterVersion: defaultVersion,
		ValidNodeVersions:     all,
		ValidMasterVersions:   all,
		DefaultImageType:      "COS_CONTAINERD",
		ValidImageTypes:       []string{"COS_CONTAINERD", "UBUNTU_CONTAINERD"},
	}
	for _, ch := range []containerpb.ReleaseChannel_Channel{
		containerpb.ReleaseChannel_RAPID, containerpb.ReleaseChannel_REGULAR,
		containerpb.ReleaseChannel_STABLE, containerpb.ReleaseChannel_EXTENDED,
	} {
		vs := channelVersions(ch)
		sc.Channels = append(sc.Channels, &containerpb.ServerConfig_ReleaseChannelConfig{
			Channel: ch, DefaultVersion: channelDefaults[ch], ValidVersions: vs,
			UpgradeTargetVersion: vs[0],
		})
	}
	return sc
}
