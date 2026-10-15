package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The inward half of the Host attachment (ADR 0014): VPC workloads reach
// Offered ports at a subnetwork's Host address.

// hostTimeout bounds a Host attachment call, which may realise the
// subnetwork's container network and start the VM relay.
const hostTimeout = 3 * time.Minute

// hostOffers is a subnetwork's entry in /_emu/v1/host/offers.
type hostOffers struct {
	Subnetwork  string `json:"subnetwork"`
	HostAddress string `json:"hostAddress"`
	Offers      []struct {
		Port     int    `json:"port"`
		Protocol string `json:"protocol"`
	} `json:"offers"`
}

// ports lists the Offered ports as "8080/tcp".
func (h hostOffers) ports() []string {
	out := make([]string, 0, len(h.Offers))
	for _, o := range h.Offers {
		out = append(out, fmt.Sprintf("%d/%s", o.Port, o.Protocol))
	}
	return out
}

// hostCall calls the Host attachment API and decodes the answer into out.
func (o *rootOpts) hostCall(cmd *cobra.Command, method, path string, body, out any) error {
	var req []byte
	if body != nil {
		var err error
		if req, err = json.Marshal(body); err != nil {
			return err
		}
	}
	b, err := o.adminRequest(cmd, method, path, req, hostTimeout)
	if err != nil {
		// The admin API's {"error": "..."}, as a plain message.
		var e struct{ Error string }
		if json.Unmarshal([]byte(err.Error()), &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return err
	}
	return json.Unmarshal(b, out)
}

func subnetworkFlag(cmd *cobra.Command, v *string) {
	cmd.Flags().StringVar(v, "subnetwork", "", "the subnetwork, as projects/P/regions/R/subnetworks/S")
	_ = cmd.MarkFlagRequired("subnetwork")
}

func (o *rootOpts) offerCmd() *cobra.Command {
	var subnetwork string
	cmd := &cobra.Command{
		Use:   "offer <port>[/udp]...",
		Short: "Offer host ports to VPC workloads at a subnetwork's Host address",
		Long: `Offer host ports to the workloads on a subnetwork: they reach <Host address>:<port>,
which forwards to 127.0.0.1:<port> on the host, over TCP, or UDP with /udp.
A port stays offered until it is withdrawn, across restarts of a Persistent
Instance. Prints the Host address.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var h hostOffers
			if err := o.hostCall(cmd, http.MethodPost, "/_emu/v1/host/offers", map[string]any{"subnetwork": subnetwork, "ports": args}, &h); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), h.HostAddress)
			return nil
		},
	}
	subnetworkFlag(cmd, &subnetwork)
	return cmd
}

func (o *rootOpts) withdrawCmd() *cobra.Command {
	var subnetwork string
	cmd := &cobra.Command{
		Use:   "withdraw <port>[/udp]...",
		Short: "Withdraw ports offered at a subnetwork's Host address",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{"subnetwork": {subnetwork}, "port": args}
			var h hostOffers
			return o.hostCall(cmd, http.MethodDelete, "/_emu/v1/host/offers?"+q.Encode(), nil, &h)
		},
	}
	subnetworkFlag(cmd, &subnetwork)
	return cmd
}

func (o *rootOpts) hostAddressCmd() *cobra.Command {
	var subnetwork string
	cmd := &cobra.Command{
		Use:   "host-address",
		Short: "Print the address at which a subnetwork's workloads reach the host",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var h hostOffers
			if err := o.hostCall(cmd, http.MethodGet, "/_emu/v1/host/address?"+url.Values{"subnetwork": {subnetwork}}.Encode(), nil, &h); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), h.HostAddress)
			return nil
		},
	}
	subnetworkFlag(cmd, &subnetwork)
	return cmd
}

// The outward half of the Host attachment (ADR 0014): host clients reach
// GKE clusters' Kubernetes Services by name at Loopback aliases.

// hostServices is /_emu/v1/host/services.
type hostServices struct {
	Services []struct {
		Cluster   string `json:"cluster"`
		Namespace string `json:"namespace"`
		Service   string `json:"service"`
		Name      string `json:"name"`
		ShortName string `json:"shortName"`
		Address   string `json:"address"`
		Ports     []int  `json:"ports"`
	} `json:"services"`
	Collisions []struct {
		Name     string   `json:"name"`
		Clusters []string `json:"clusters"`
	} `json:"collisions"`
	Unserved []struct {
		Cluster   string `json:"cluster"`
		Namespace string `json:"namespace"`
		Service   string `json:"service"`
		Reason    string `json:"reason"`
	} `json:"unserved"`
	PoolExhausted bool `json:"poolExhausted"`
}

// printHostServices lists, for `gcpemu status`, the Services that hold
// Loopback aliases: each one's names, alias and served ports.
func printHostServices(out io.Writer, body []byte) {
	var hs hostServices
	if json.Unmarshal(body, &hs) != nil || len(hs.Services) == 0 {
		return
	}
	fmt.Fprintln(out, "kubernetes services for host clients:")
	for _, s := range hs.Services {
		name := s.Name
		if s.ShortName != "" {
			name += " (" + s.ShortName + ")"
		}
		ports := make([]string, 0, len(s.Ports))
		for _, p := range s.Ports {
			ports = append(ports, strconv.Itoa(p))
		}
		if len(ports) == 0 {
			fmt.Fprintf(out, "  %s at %s\n", name, s.Address)
			continue
		}
		fmt.Fprintf(out, "  %s at %s: %s\n", name, s.Address, strings.Join(ports, " "))
	}
}

// reportHostServices prints, for `gcpemu doctor`, the short Service names
// that do not resolve because more than one cluster has them, an exhausted
// alias pool and the Services left without an alias.
func reportHostServices(out io.Writer, body []byte) {
	var hs hostServices
	if json.Unmarshal(body, &hs) != nil {
		return
	}
	for _, c := range hs.Collisions {
		svc, _, _ := strings.Cut(strings.TrimSuffix(c.Name, ".svc.cluster.local"), ".")
		ns := strings.TrimPrefix(strings.TrimSuffix(c.Name, ".svc.cluster.local"), svc+".")
		fmt.Fprintf(out, "[WARN] %s names a Kubernetes Service in %s, so it does not resolve for host clients\n", c.Name, strings.Join(c.Clusters, " and "))
		fmt.Fprintf(out, "       fix: use the gke.internal names, e.g. %s.%s.svc.<cluster>.<location>.<project>.gke.internal\n", svc, ns)
	}
	if hs.PoolExhausted {
		fmt.Fprintln(out, "[WARN] Loopback aliases 127.0.0.2-127.0.0.254 are all held, so new Kubernetes Services get SERVFAIL for host clients")
		fmt.Fprintln(out, "       fix: delete the Cloud SQL instances, forwarding rules or Kubernetes Services you no longer use")
	}
	for _, u := range hs.Unserved {
		fmt.Fprintf(out, "[WARN] Kubernetes Service %s/%s of %s has no Loopback alias: %s\n", u.Namespace, u.Service, u.Cluster, u.Reason)
	}
}
