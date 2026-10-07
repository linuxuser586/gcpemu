package cli

import (
	"net/http"

	"github.com/spf13/cobra"
)

// cdnCmd groups Cloud CDN cache commands (FR-CDN-008).
func (o *rootOpts) cdnCmd() *cobra.Command {
	root := &cobra.Command{Use: "cdn", Short: "Manage the Cloud CDN cache"}
	purge := &cobra.Command{
		Use:   "purge",
		Short: "Remove every object from the Cloud CDN cache",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.adminCall(cmd, http.MethodPost, "/_emu/v1/cdn/purge", nil)
		},
	}
	stats := &cobra.Command{
		Use:   "stats",
		Short: "Show Cloud CDN cache entries, bytes used and size limit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.adminCall(cmd, http.MethodGet, "/_emu/v1/cdn", nil)
		},
	}
	root.AddCommand(purge, stats)
	return root
}
