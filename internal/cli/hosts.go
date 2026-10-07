package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/hostmode"
)

// Host mode helpers (FR-CORE-043): `gcpemu hosts` and the hints `gcpemu
// env` prints to stderr.

func (o *rootOpts) hostsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hosts",
		Short: "Print an /etc/hosts block mapping real Google hostnames to the host-mode frontend",
		Long: "Prints an /etc/hosts block that sends the emulated *.googleapis.com hosts and every\n" +
			"LOCATION-docker.pkg.dev to the instance's host-mode frontend (start it with --host-mode).\n" +
			"Install it with:  gcpemu hosts | sudo tee -a /etc/hosts\n" +
			"and trust the CA with `gcpemu ca install`. Remove the block when done.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			c, err := clientFor(cfg)
			if err != nil {
				return err
			}
			st, err := fetchHostMode(c)
			if err != nil {
				return err
			}
			if !st.Enabled {
				return fmt.Errorf("host mode is off for instance %q; start it with --host-mode", cfg.Instance)
			}
			if st.State != hostmode.StateReady {
				return fmt.Errorf("host mode is %s: %s", st.State, st.Error)
			}
			return writeHostsBlock(cmd.OutOrStdout(), cfg.Instance, st)
		},
	}
}

func fetchHostMode(c *adminClient) (hostmode.Status, error) {
	var st hostmode.Status
	b, err := c.get("/_emu/v1/hostmode")
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// writeHostsBlock prints a delimited /etc/hosts block.
func writeHostsBlock(w io.Writer, instance string, st hostmode.Status) error {
	fmt.Fprintf(w, "# BEGIN gcpemu %s (host mode)\n", instance)
	for _, h := range st.Hosts {
		fmt.Fprintf(w, "%s\t%s\n", st.IP, h)
	}
	_, err := fmt.Fprintf(w, "# END gcpemu %s\n", instance)
	return err
}

// hostModeHints tells the user how to route names to the host-mode
// frontend. It writes to stderr so that `eval $(gcpemu env)` is unaffected.
func hostModeHints(c *adminClient, w io.Writer) {
	st, err := fetchHostMode(c)
	if err != nil || !st.Enabled {
		return
	}
	if st.State != hostmode.StateReady {
		fmt.Fprintf(w, "# gcpemu host mode is %s %s\n", st.State, st.Error)
		return
	}
	link := st.Interface
	if link == "" {
		link = "<bridge-interface>"
	}
	var routed []string
	for _, d := range st.Domains {
		routed = append(routed, "~"+d)
	}
	fmt.Fprintf(w, `# gcpemu host mode: real Google hostnames are served at %[1]s (HTTPS :443, HTTP :80, DNS :53).
# Route their DNS there with systemd-resolved (until the bridge restarts):
#   sudo resolvectl dns %[2]s %[1]s && sudo resolvectl domain %[2]s %[3]s
# or install a static block:  gcpemu hosts | sudo tee -a /etc/hosts
# Trust the CA:  gcpemu ca install   (or per shell: eval "$(gcpemu env --trust)"; CA: %[4]s)
`, st.IP, link, strings.Join(routed, " "), st.CAFile)
}
