package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func (o *rootOpts) envCmd() *cobra.Command {
	var shell string
	var trustCA, noUniverse bool
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
			q := url.Values{}
			if trustCA {
				q.Set("trust", "true")
			}
			if noUniverse {
				q.Set("universe", "false")
			}
			path := "/_emu/v1/env"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			b, err := c.get(path)
			if err != nil {
				return err
			}
			vars := map[string]string{}
			if err := json.Unmarshal(b, &vars); err != nil {
				return err
			}
			guardADC(c, vars, cmd.ErrOrStderr())
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
			buildLogHints(vars, cmd.ErrOrStderr())
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "bash", "output format: bash, fish, powershell (or pwsh), github")
	cmd.Flags().BoolVar(&trustCA, "trust", false, "also set SSL_CERT_FILE to a bundle of the system roots plus the emulator CA")
	cmd.Flags().BoolVar(&noUniverse, "no-universe", false, "leave out CLOUDSDK_CORE_UNIVERSE_DOMAIN, so gcloud stays in Google's universe and keeps its stored credentials; GOOGLE_CLOUD_UNIVERSE_DOMAIN stays, since emulator keys need it")
	return cmd
}

// buildLogHints says how to run `gcloud builds submit` when gcloud can't
// tail the Build log (SRS 11.3): it reads the log from
// storage.UNIVERSE_DOMAIN, ignoring endpoint overrides, so under the
// Invalid universe it fails even for a Build that succeeds. It writes to
// stderr so that `eval $(gcpemu env)` is unaffected.
func buildLogHints(vars map[string]string, w io.Writer) {
	universe := vars["CLOUDSDK_CORE_UNIVERSE_DOMAIN"]
	if vars["CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDBUILD"] == "" || universe == "" {
		return
	}
	fmt.Fprintf(w, `# gcpemu: gcloud builds submit can't stream the Build log: gcloud reads it from storage.%s.
#   Pass --async, or set options.logging: NONE in the build config (gcloud then waits without
#   streaming); --suppress-logs still reads the log. Read it with
#   gcloud storage cat gs://BUCKET/log-BUILD_ID.txt (the Build's logsBucket), or open its logUrl.
#   In Host mode, eval "$(gcpemu env --no-universe)" lets gcloud stream it.
`, universe)
}

func writeEnv(w io.Writer, shell string, vars map[string]string) error {
	for _, k := range sortedKeys(vars) {
		v := vars[k]
		switch shell {
		case "bash", "sh", "zsh":
			fmt.Fprintf(w, "export %s=%s\n", k, shQuote(v))
		case "fish":
			fmt.Fprintf(w, "set -gx %s %s;\n", k, shQuote(v))
		case "powershell", "pwsh":
			fmt.Fprintf(w, "$env:%s = %s\n", k, psQuote(v))
		case "github":
			fmt.Fprintf(w, "%s=%s\n", k, v)
		default:
			return fmt.Errorf("unknown shell %q (want bash, fish, powershell or github)", shell)
		}
	}
	return nil
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// psQuote quotes s as a PowerShell single-quoted string, in which only a
// doubled quote is special. PowerShell also takes the typographic single
// quotes U+2018–U+201B as quotes, so those are doubled too.
func psQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\'' || r >= 0x2018 && r <= 0x201b {
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}
