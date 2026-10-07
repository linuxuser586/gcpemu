package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func (o *rootOpts) envCmd() *cobra.Command {
	var shell string
	var trustCA bool
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Print environment variables that point clients at the instance",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			c, err := clientFor(cfg)
			if err != nil {
				return err
			}
			path := "/_emu/v1/env"
			if trustCA {
				path += "?trust=true"
			}
			b, err := c.get(path)
			if err != nil {
				return err
			}
			vars := map[string]string{}
			if err := json.Unmarshal(b, &vars); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if shell == "github" {
				if p := os.Getenv("GITHUB_ENV"); p != "" {
					f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
					if err != nil {
						return err
					}
					defer f.Close()
					out = f
				}
			}
			if err := writeEnv(out, shell, vars); err != nil {
				return err
			}
			hostModeHints(c, cmd.ErrOrStderr())
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "bash", "output format: bash, fish, github")
	cmd.Flags().BoolVar(&trustCA, "trust", false, "also set SSL_CERT_FILE to a bundle of the system roots plus the emulator CA")
	return cmd
}

func writeEnv(w io.Writer, shell string, vars map[string]string) error {
	for _, k := range sortedKeys(vars) {
		v := vars[k]
		switch shell {
		case "bash", "sh", "zsh":
			fmt.Fprintf(w, "export %s=%s\n", k, shQuote(v))
		case "fish":
			fmt.Fprintf(w, "set -gx %s %s;\n", k, shQuote(v))
		case "github":
			fmt.Fprintf(w, "%s=%s\n", k, v)
		default:
			return fmt.Errorf("unknown shell %q (want bash, fish or github)", shell)
		}
	}
	return nil
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
