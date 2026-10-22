package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/link"
)

// linkCmd implements `gcpemu link`: Pairing with other Instances, so that
// each accepts the other's asserted Principals over a link (ADR 0027).
func (o *rootOpts) linkCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "link",
		Short: "Pair this Instance with other Instances, as Linked Instances",
		Long: "Pairing makes two Instances each pin the other's Instance CA, so that each accepts the other\n" +
			"over a link. It is approved on both sides: `link pair` on one Instance, which shows the other's\n" +
			"identity, and `link approve` on the other, which must be started with --link-bind.",
	}
	root.AddCommand(o.linkPairCmd(), o.linkListCmd(), o.linkUnpairCmd(), o.linkRequestsCmd(), o.linkApproveCmd(), o.linkRejectCmd())
	return root
}

// linkPairCmd is `gcpemu link pair`.
func (o *rootOpts) linkPairCmd() *cobra.Command {
	var name string
	var yes bool
	cmd := &cobra.Command{
		Use:   "pair ADDRESS",
		Short: "Ask the Instance whose --link-bind is ADDRESS (host, or host:port) to pair",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			addr := link.Dialable(args[0])
			var id link.Identity
			if err := o.linkCall(cmd, http.MethodPost, "/_emu/v1/links/identify", map[string]string{"address": addr}, 0, &id); err != nil {
				return err
			}
			fmt.Fprintf(out, "Instance %q at %s\n  Instance ID     %s\n  CA fingerprint  %s\n  link protocol   version %d\n", id.Name, addr, id.ID, id.Fingerprint, id.Version)
			if own, err := o.ownFingerprint(cmd); err == nil {
				fmt.Fprintf(out, "This Instance's CA fingerprint, which the other side will show: %s\n", own)
			}
			if !yes {
				if err := confirm(cmd, "Pair with it? Check the fingerprint with its owner; `gcpemu link requests` there shows this Instance's."); err != nil {
					return err
				}
			}
			fmt.Fprintf(out, "Waiting for Instance %q to approve: run `gcpemu link approve` there.\n", id.Name)
			var p link.Pairing
			body := map[string]string{"address": addr, "fingerprint": id.Fingerprint, "name": name}
			if err := o.linkCall(cmd, http.MethodPost, "/_emu/v1/links", body, link.DefaultApprovalTimeout+30*time.Second, &p); err != nil {
				return err
			}
			fmt.Fprintf(out, "Paired with Instance %q as link %q.\n", p.Instance, p.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name of the link on this Instance (default the other Instance's name)")
	cmd.Flags().BoolVar(&yes, "yes", false, "pair without asking to confirm the other Instance's identity")
	return cmd
}

// linkListCmd is `gcpemu link list`.
func (o *rootOpts) linkListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List this Instance's links",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var r struct {
				Links []link.Link `json:"links"`
			}
			if err := o.linkCall(cmd, http.MethodGet, "/_emu/v1/links", nil, 0, &r); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return json.NewEncoder(out).Encode(r)
			}
			if len(r.Links) == 0 {
				fmt.Fprintln(out, "no links; pair with another Instance with `gcpemu link pair`")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tINSTANCE\tINSTANCE ID\tADDRESS\tSTATE\tCA FINGERPRINT")
			for _, l := range r.Links {
				addr := l.Address
				if addr == "" {
					addr = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", l.Name, l.Instance, l.InstanceID, addr, l.State, l.Fingerprint)
			}
			_ = tw.Flush()
			for _, l := range r.Links {
				if l.Problem != "" {
					fmt.Fprintf(out, "%s: %s\n", l.Name, l.Problem)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// linkUnpairCmd is `gcpemu link unpair`.
func (o *rootOpts) linkUnpairCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unpair NAME",
		Short: "Remove the link NAME: this Instance stops accepting the other at once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.linkCall(cmd, http.MethodDelete, "/_emu/v1/links/"+url.PathEscape(args[0]), nil, 0, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Unpaired link %q; unpair it on the other Instance too.\n", args[0])
			return nil
		},
	}
}

// pairingRequests returns the incoming Pairing requests.
func (o *rootOpts) pairingRequests(cmd *cobra.Command) ([]link.Request, error) {
	var r struct {
		Requests []link.Request `json:"requests"`
	}
	err := o.linkCall(cmd, http.MethodGet, "/_emu/v1/links/requests", nil, 0, &r)
	return r.Requests, err
}

// ownFingerprint returns this Instance's CA fingerprint, which the other
// side of a Pairing shows its user to compare.
func (o *rootOpts) ownFingerprint(cmd *cobra.Command) (string, error) {
	var r struct {
		Fingerprint string `json:"fingerprint"`
	}
	err := o.linkCall(cmd, http.MethodGet, "/_emu/v1/links", nil, 0, &r)
	return r.Fingerprint, err
}

// printRequest shows an incoming Pairing request, warning when approving
// it replaces a link pinned to another CA.
func printRequest(w io.Writer, r link.Request) {
	fmt.Fprintf(w, "Pairing request %s from Instance %q\n  Instance ID     %s\n  CA fingerprint  %s\n", r.ID, r.Name, r.InstanceID, r.Fingerprint)
	if r.Address != "" {
		fmt.Fprintf(w, "  address         %s\n", r.Address)
	}
	if r.Replaces != "" {
		fmt.Fprintf(w, "  WARNING: approving it replaces link %q, pinned to another CA; approve it only if that Instance's CA was rotated\n", r.Replaces)
	}
}

// linkRequestsCmd is `gcpemu link requests`.
func (o *rootOpts) linkRequestsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "requests",
		Short: "List the Pairing requests waiting for this Instance's approval",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reqs, err := o.pairingRequests(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(reqs) == 0 {
				fmt.Fprintln(out, "no Pairing requests are waiting")
			} else if own, err := o.ownFingerprint(cmd); err == nil {
				fmt.Fprintf(out, "This Instance's CA fingerprint, which the requester was shown: %s\n", own)
			}
			for _, r := range reqs {
				printRequest(out, r)
			}
			return nil
		},
	}
}

// linkApproveCmd is `gcpemu link approve`.
func (o *rootOpts) linkApproveCmd() *cobra.Command {
	var name string
	var yes bool
	cmd := &cobra.Command{
		Use:   "approve [REQUEST]",
		Short: "Approve waiting Pairing requests, asking about each unless --yes",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reqs, err := o.pairingRequests(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			var picked []link.Request
			for _, r := range reqs {
				if len(args) == 0 || r.ID == args[0] {
					picked = append(picked, r)
				}
			}
			switch {
			case len(args) > 0 && len(picked) == 0:
				return fmt.Errorf("no Pairing request %q is waiting; `gcpemu link requests` lists them", args[0])
			case len(picked) == 0:
				fmt.Fprintln(out, "no Pairing requests are waiting")
				return nil
			case name != "" && len(picked) > 1:
				return errors.New("--name names one link: name the request to approve")
			}
			for _, r := range picked {
				printRequest(out, r)
				if !yes {
					if err := confirm(cmd, "Approve it? The other Instance can then assert Principals to this one."); err != nil {
						fmt.Fprintln(out, "skipped")
						continue
					}
				}
				var p link.Pairing
				if err := o.linkCall(cmd, http.MethodPost, "/_emu/v1/links/requests/"+url.PathEscape(r.ID)+"/approve", map[string]string{"name": name}, 0, &p); err != nil {
					return err
				}
				fmt.Fprintf(out, "Paired with Instance %q as link %q.\n", p.Instance, p.Name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name of the link on this Instance (default the requesting Instance's name)")
	cmd.Flags().BoolVar(&yes, "yes", false, "approve without asking")
	return cmd
}

// linkRejectCmd is `gcpemu link reject`.
func (o *rootOpts) linkRejectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reject REQUEST",
		Short: "Reject a waiting Pairing request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.linkCall(cmd, http.MethodPost, "/_emu/v1/links/requests/"+url.PathEscape(args[0])+"/reject", nil, 0, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Rejected Pairing request %s.\n", args[0])
			return nil
		},
	}
}

// linkCall is adminRequest with in sent as JSON and the reply decoded
// into out; a failure is the Admin API's error message.
func (o *rootOpts) linkCall(cmd *cobra.Command, method, path string, in any, timeout time.Duration, out any) error {
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	b, err := o.adminRequest(cmd, method, path, body, timeout)
	if err != nil {
		var e struct{ Error string }
		if json.Unmarshal([]byte(err.Error()), &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return err
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// confirm asks question on a terminal and fails unless the answer is yes.
func confirm(cmd *cobra.Command, question string) error {
	if fi, err := os.Stdin.Stat(); cmd.InOrStdin() == os.Stdin && (err != nil || fi.Mode()&os.ModeCharDevice == 0) {
		return errors.New("not a terminal: re-run with --yes to confirm")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", question)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("aborted")
	}
	return nil
}
