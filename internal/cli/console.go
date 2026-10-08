package cli

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
)

func (o *rootOpts) consoleCmd() *cobra.Command {
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "console",
		Short: "Open the running instance's Web console in the default browser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			c, err := clientFor(cfg)
			if err != nil {
				return err
			}
			b, err := c.get("/_emu/v1/info")
			if err != nil {
				return fmt.Errorf("instance %q is not running: %w", cfg.Instance, err)
			}
			var info struct{ Console bool }
			if err := json.Unmarshal(b, &info); err != nil {
				return err
			}
			if !info.Console {
				return fmt.Errorf("instance %q does not serve the Web console: it was started with --console=false, "+
					"with CI=true and no --console, or with --bind beyond loopback", cfg.Instance)
			}
			url := c.base + "/console/"
			fmt.Fprintln(cmd.OutOrStdout(), url)
			if printOnly {
				return nil
			}
			return openBrowser(url)
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "only print the console URL")
	return cmd
}

// openBrowser opens url in the default browser (a variable for tests).
var openBrowser = func(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	if err := exec.Command(name, url).Start(); err != nil {
		return fmt.Errorf("open a browser with %s: %w; open %s yourself", name, err, url)
	}
	return nil
}
