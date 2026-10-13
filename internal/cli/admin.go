package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/admin"
	"github.com/linuxuser586/gcpemu/internal/gateway"
	"github.com/linuxuser586/gcpemu/internal/instance"
)

func (o *rootOpts) adminCmd() *cobra.Command {
	root := &cobra.Command{Use: "admin", Short: "Describe the /_emu/v1/ admin API and print its admin token"}
	root.AddCommand(&cobra.Command{
		Use:   "token",
		Short: "Print the running instance's admin token (only when bound beyond loopback)",
		Long: "Prints the admin token that the admin API and the Web console need when the\n" +
			"instance's API gateway is bound beyond loopback (send it in the " + gateway.AdminTokenHeader + "\n" +
			"header). gcpemu commands on this host send it on their own.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			if _, err := readEndpoints(cfg); err != nil {
				return err
			}
			b, err := os.ReadFile(filepath.Join(cfg.InstanceDir(), instance.AdminTokenFile))
			if err != nil {
				return fmt.Errorf("instance %q has no admin token: its API gateway is bound to loopback, where none is needed", cfg.Instance)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSpace(string(b)))
			return err
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "openapi",
		Short: "Print the admin API's OpenAPI 3 document",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := cmd.OutOrStdout().Write(admin.OpenAPI)
			return err
		},
	})
	return root
}
