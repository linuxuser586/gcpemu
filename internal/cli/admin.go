package cli

import (
	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/admin"
)

func (o *rootOpts) adminCmd() *cobra.Command {
	root := &cobra.Command{Use: "admin", Short: "Describe the /_emu/v1/ admin API"}
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
