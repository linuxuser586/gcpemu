package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// `gcpemu loopback` adds (or removes) the 127.0.0.2–127.0.0.254 aliases on
// a macOS host's lo0 (ADR 0005): each Cloud SQL instance and each
// forwarding rule gets its own address there. Linux routes all of
// 127.0.0.0/8 to loopback, so elsewhere there is nothing to do.

const (
	loopbackLabel     = "io.github.linuxuser586.gcpemu.loopback"
	loopbackDaemonDir = "/Library/LaunchDaemons"
	loopbackFirst     = 2
	loopbackLast      = 254
)

func loopbackCmd(goos string) *cobra.Command {
	root := &cobra.Command{Use: "loopback", Short: "Manage the 127.0.0.x loopback aliases (macOS)"}
	sub := func(install bool) *cobra.Command {
		var yes, dryRun bool
		use, short := "uninstall", "Remove the 127.0.0.x loopback aliases and their LaunchDaemon (macOS; asks for confirmation; uses sudo)"
		if install {
			use, short = "install", "Add 127.0.0.2-127.0.0.254 to lo0 now and at boot (macOS; asks for confirmation; uses sudo)"
		}
		c := &cobra.Command{
			Use:   use,
			Short: short,
			Long: short + "\n\nWithout the aliases only one Cloud SQL instance is reachable from the host, and\n" +
				"forwarding rules share 127.0.0.1. Other platforms route all of 127.0.0.0/8 to\n" +
				"loopback, so there this does nothing.",
			RunE: func(cmd *cobra.Command, args []string) error {
				if goos != "darwin" {
					fmt.Fprintf(cmd.OutOrStdout(), "Nothing to do: %s routes all of 127.0.0.0/8 to loopback.\n", goos)
					return nil
				}
				return runPlan(cmd, loopbackPlan(os.Geteuid() == 0, loopbackDaemonDir, install), yes, dryRun)
			},
		}
		c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation (sudo may still prompt for a password)")
		c.Flags().BoolVar(&dryRun, "dry-run", false, "only print the commands")
		return c
	}
	root.AddCommand(sub(true), sub(false))
	return root
}

// loopbackPlan returns the script that adds (install) or removes the
// aliases and the LaunchDaemon in dir that adds them again at boot. Both
// can run again. Install removes each alias and adds it again, and unloads
// the daemon before loading it, so it also brings an earlier install up to
// date; uninstall unloads the daemon only when it is loaded.
func loopbackPlan(root bool, dir string, install bool) []trustStep {
	plist := dir + "/" + loopbackLabel + ".plist"
	var b strings.Builder
	if install {
		b.WriteString("set -e\n" + loopbackAliasLoop("ifconfig lo0 -alias 127.0.0.$i 2>/dev/null || true; ifconfig lo0 "+loopbackAlias) + "\n")
		b.WriteString("mkdir -p " + dir + "\ncat > " + plist + " <<'GCPEMU_PLIST'\n" + loopbackPlist() + "GCPEMU_PLIST\n")
		b.WriteString("chown root:wheel " + plist + "\nchmod 644 " + plist + "\n")
		b.WriteString("launchctl bootout system/" + loopbackLabel + " 2>/dev/null || true\nlaunchctl bootstrap system " + plist + "\n")
	} else {
		b.WriteString("launchctl bootout system/" + loopbackLabel + " 2>/dev/null || true\n")
		b.WriteString("rm -f " + plist + "\n")
		b.WriteString(loopbackAliasLoop("ifconfig lo0 -alias 127.0.0.$i 2>/dev/null || true") + "\n")
	}
	return []trustStep{{args: sudoUnless(root, "sh", "-s"), stdin: []byte(b.String()), script: true}}
}

// loopbackAlias adds 127.0.0.$i as a /32. Without a netmask ifconfig gives
// each alias the classful /8, and 253 overlapping 127.0.0.0/8s keep
// mDNSResponder at 100% CPU on macOS 27 (#677).
const loopbackAlias = "alias 127.0.0.$i netmask 255.255.255.255 up"

// loopbackAliasLoop runs cmd for each i of the aliases.
func loopbackAliasLoop(cmd string) string {
	return fmt.Sprintf("i=%d; while [ $i -le %d ]; do %s; i=$((i+1)); done", loopbackFirst, loopbackLast, cmd)
}

// loopbackPlist is the LaunchDaemon that adds the aliases at boot.
func loopbackPlist() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + loopbackLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>` + loopbackAliasLoop("/sbin/ifconfig lo0 "+loopbackAlias) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`
}
