package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
