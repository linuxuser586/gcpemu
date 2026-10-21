package cli

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/config"
)

// `gcpemu dns` adds (or removes) Resolver routes: rules in the host's
// resolver that send queries for a suffix to the Instance's dns Service
// endpoint (#608, ADR 0026).

// defaultResolverSuffixes are the Host attachment's names (ADR 0014).
var defaultResolverSuffixes = []string{"svc.cluster.local", "gke.internal"}

// resolverHostNow describes the host; with read, what is on its resolver
// now too. Tests replace it.
var resolverHostNow = func(read bool) (resolverHost, error) { return readResolverHost(goruntime.GOOS, "", read) }

var (
	suffixRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	instanceRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func (o *rootOpts) dnsCmd() *cobra.Command {
	root := &cobra.Command{Use: "dns", Short: "Manage the Resolver routes that send host DNS queries for a suffix to the Instance's dns Service endpoint"}
	sub := func(install bool) *cobra.Command {
		var yes, dryRun bool
		var suffixes []string
		c := &cobra.Command{
			Use:   "install",
			Short: "Send host DNS queries for svc.cluster.local, gke.internal and each --suffix to the Instance's DNS (asks for confirmation; uses sudo, or UAC on Windows)",
			Long: "Adds a Resolver route for each suffix, so host clients resolve names under it through\n" +
				"the Instance's dns Service endpoint: emulated Cloud DNS zones and Host attachment names.\n" +
				"svc.cluster.local and gke.internal are always included; running it again adds to the\n" +
				"routes the Instance has and moves them all to its current dns port.\n\n" +
				"  Linux:   routes on a dummy link gcpemu-dnsN through systemd-resolved; they last until reboot\n" +
				"  macOS:   one /etc/resolver/<suffix> file per suffix\n" +
				"  Windows: one NRPT rule per suffix; NRPT can't name a port, so start the Instance with --port dns=53",
		}
		if !install {
			c.Use = "uninstall"
			c.Short = "Remove the Instance's Resolver routes, or only each --suffix (asks for confirmation; uses sudo, or UAC on Windows)"
			c.Long = c.Short + "\n\nOnly what `gcpemu dns install` added for this Instance is removed."
		}
		c.RunE = func(cmd *cobra.Command, args []string) error {
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			if !instanceRE.MatchString(cfg.Instance) {
				return fmt.Errorf("instance name %q: Resolver routes need one of letters, digits, '.', '_' and '-'", cfg.Instance)
			}
			list, err := normalizeSuffixes(suffixes)
			if err != nil {
				return err
			}
			if install {
				list = append(slices.Clone(defaultResolverSuffixes), list...)
			}
			records, err := readResolverRecords()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !install && len(records[cfg.Instance].Suffixes) == 0 {
				fmt.Fprintf(out, "Instance %q has no Resolver routes.\n", cfg.Instance)
				return nil
			}
			port := dnsPort(cfg)
			if install && port == 0 {
				return fmt.Errorf("the dns Service endpoint of instance %q picks a free port (--port dns=0): start the Instance first, or give it a fixed port", cfg.Instance)
			}
			// uninstall works from the record alone: its plan checks
			// what is there as it runs.
			h, err := resolverHostNow(install)
			if err != nil {
				return err
			}
			ch, err := resolverPlan(h, records, cfg.Instance, list, port, install)
			if err != nil {
				return err
			}
			if len(ch.plan) > 0 {
				if err := runPlan(cmd, ch.plan, yes, dryRun); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(out, "Nothing to change on the host.")
			}
			if dryRun {
				return nil
			}
			if len(ch.record.Suffixes) == 0 {
				delete(records, cfg.Instance)
			} else {
				records[cfg.Instance] = ch.record
			}
			if err := writeResolverRecords(records); err != nil {
				return err
			}
			printResolverRoutes(out, ch.record)
			return nil
		}
		c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation (sudo may still prompt for a password; Windows still shows its UAC dialog)")
		c.Flags().BoolVar(&dryRun, "dry-run", false, "only print the commands")
		c.Flags().StringArrayVar(&suffixes, "suffix", nil, "DNS suffix to route, such as a Cloud DNS zone's DNS name (repeatable)")
		return c
	}
	root.AddCommand(sub(true), sub(false))
	return root
}

// normalizeSuffixes lowercases suffixes without their outer dots and
// refuses any that isn't a DNS name, so none can reach a script as more.
func normalizeSuffixes(suffixes []string) ([]string, error) {
	var out []string
	for _, s := range suffixes {
		n := strings.Trim(strings.ToLower(s), ".")
		if !suffixRE.MatchString(n) {
			return nil, fmt.Errorf("--suffix %q is not a DNS name", s)
		}
		out = append(out, n)
	}
	return out, nil
}

// dnsPort is the Instance's dns port: the running Instance's, else the
// configured one.
func dnsPort(cfg *config.Config) int {
	if eps, err := readEndpoints(cfg); err == nil {
		if _, p, err := net.SplitHostPort(eps["dns"]); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				return n
			}
		}
	}
	return cfg.Port("dns")
}

// printResolverRoutes lists rec's routes, for `dns install` and `status`.
func printResolverRoutes(out io.Writer, rec resolverRecord) {
	if len(rec.Suffixes) == 0 {
		return
	}
	fmt.Fprintf(out, "resolver routes to 127.0.0.1:%d:\n", rec.Port)
	for _, s := range rec.Suffixes {
		fmt.Fprintf(out, "  %s\n", s)
	}
}

const resolverRecordsName = "resolver-routes.json"

// resolverRecordsPath is the file of every Instance's install record: in
// the user config dir, not a data dir, so Instances without one have
// records too. Under sudo it is the invoking user's, which they own.
func resolverRecordsPath() (path string, owner string, err error) {
	if home, name := nssUser(elevated()); name != "" {
		if goruntime.GOOS == "darwin" {
			return filepath.Join(home, "Library", "Application Support", "gcpemu", resolverRecordsName), name, nil
		}
		return filepath.Join(home, ".config", "gcpemu", resolverRecordsName), name, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "gcpemu", resolverRecordsName), "", nil
}

// readResolverRecords returns every Instance's record by name.
func readResolverRecords() (map[string]resolverRecord, error) {
	recs := map[string]resolverRecord{}
	path, _, err := resolverRecordsPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return recs, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &recs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return recs, nil
}

func writeResolverRecords(recs map[string]resolverRecord) error {
	path, owner, err := resolverRecordsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(recs, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if owner == "" {
		return nil
	}
	u, err := user.Lookup(owner)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	for _, p := range []string{filepath.Dir(path), path} {
		if err := os.Chown(p, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// resolverRecord is what `dns install` changed for one Instance, so
// `dns uninstall` reverts only that.
type resolverRecord struct {
	Suffixes []string `json:"suffixes"`
	Port     int      `json:"port"`
	// Link is the dummy link that holds the routes on Linux.
	Link string `json:"link,omitempty"`
}

// resolverHost is what is on the host's resolver now.
type resolverHost struct {
	goos     string
	elevated bool
	// Linux: whether systemd-resolved runs (and resolvectl is there), and
	// every link by name, "Global" for resolved's global settings.
	resolved bool
	links    map[string]resolvedLink
	// macOS: the contents of ETC/resolver/<suffix> by suffix, for those
	// that exist; etc is "/etc" unless set (tests).
	etc   string
	files map[string]string
	// Windows: the local NRPT rules, and the namespaces of the effective
	// NRPT policy, which has Group Policy's and VPN clients' rules too.
	nrpt   []nrptRule
	policy []string
}

// nrptRule is one rule of Windows' Name Resolution Policy Table.
type nrptRule struct {
	Name      string
	Namespace []string
	Comment   string
}

// resolvedLink is a link's systemd-resolved settings.
type resolvedLink struct {
	domains, dns []string
}

// resolverChange is a plan and the Instance's record once it has run; a
// record without suffixes means none is kept.
type resolverChange struct {
	plan   []trustStep
	record resolverRecord
}

// resolverEdit is a change to one Instance's routes.
type resolverEdit struct {
	instance string
	old      resolverRecord
	// routes are the Instance's routes once it is made, removed the ones
	// it removes, port the dns port they go to.
	routes, removed []string
	port            int
}

// resolverPlan returns the plan that adds (install) the Resolver routes
// for suffixes to 127.0.0.1:port for instance, to the ones it has, or
// removes suffixes (all of its routes when there are none). records
// holds every Instance's record by name. Nothing to do is an empty plan.
func resolverPlan(h resolverHost, records map[string]resolverRecord, instance string, suffixes []string, port int, install bool) (resolverChange, error) {
	e := resolverEdit{instance: instance, old: records[instance], port: port}
	if install {
		e.routes = slices.Clone(e.old.Suffixes)
		for _, s := range suffixes {
			if owner := suffixOwner(records, instance, s); owner != "" {
				return resolverChange{}, fmt.Errorf("instance %q has the Resolver route for %s; run `gcpemu --instance %s dns uninstall --suffix %s` first", owner, s, owner, s)
			}
			if !slices.Contains(e.routes, s) {
				e.routes = append(e.routes, s)
			}
		}
	} else {
		e.port = e.old.Port
		for _, s := range suffixes {
			if !slices.Contains(e.old.Suffixes, s) {
				return resolverChange{}, fmt.Errorf("instance %q has no Resolver route for %s", instance, s)
			}
		}
		for _, s := range e.old.Suffixes {
			if len(suffixes) == 0 || slices.Contains(suffixes, s) {
				e.removed = append(e.removed, s)
			} else {
				e.routes = append(e.routes, s)
			}
		}
		if len(e.removed) == 0 {
			return resolverChange{}, nil
		}
	}
	switch h.goos {
	case "linux":
		return linuxResolverPlan(h, records, e)
	case "darwin":
		return darwinResolverPlan(h, e)
	case "windows":
		return windowsResolverPlan(h, e)
	}
	return resolverChange{}, fmt.Errorf("Resolver routes are not supported on %s; send %s to 127.0.0.1:%d in your resolver", h.goos, strings.Join(e.routes, " "), port)
}

// linuxResolverPlan sets routes on a dummy link the Instance owns, so no
// other link's settings change; it lasts until reboot (ADR 0026).
func linuxResolverPlan(h resolverHost, records map[string]resolverRecord, e resolverEdit) (resolverChange, error) {
	if len(e.removed) > 0 {
		if len(e.routes) == 0 {
			return resolverChange{plan: shStep(h.elevated, "ip link del "+e.old.Link+" 2>/dev/null || true\n")}, nil
		}
		rec := e.old
		rec.Suffixes = e.routes
		// After a reboot the link is gone, and with it the routes.
		return resolverChange{
			plan:   shStep(h.elevated, fmt.Sprintf("if ip link show %[1]s >/dev/null 2>&1; then resolvectl domain %[1]s %[2]s; fi\n", e.old.Link, routingDomains(e.routes))),
			record: rec,
		}, nil
	}
	routes, port := e.routes, e.port
	if !h.resolved {
		return resolverChange{}, fmt.Errorf("systemd-resolved isn't running (or resolvectl is missing), so gcpemu can't add Resolver routes; send %s to 127.0.0.1:%d in your resolver", strings.Join(routes, " "), port)
	}
	n := resolverLinkNumber(h, records, e.instance)
	link := resolverLink(n)
	var foreign []string
	for _, s := range routes {
		for _, name := range sortedKeys(h.links) {
			if name == link {
				continue
			}
			for _, d := range h.links[name].domains {
				if strings.TrimPrefix(d, "~") == s {
					foreign = append(foreign, fmt.Sprintf("%s (link %s has the domain %s)", s, name, d))
				}
			}
		}
	}
	if err := refuseForeign(foreign); err != nil {
		return resolverChange{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ip link show %[1]s >/dev/null 2>&1 || ip link add %[1]s type dummy\n", link)
	// systemd-resolved uses a link's DNS servers only while it has a
	// routable address; this one is the link's own, in link-local space.
	fmt.Fprintf(&b, "ip addr replace 169.254.53.%d/32 dev %s\n", n+1, link)
	fmt.Fprintf(&b, "ip link set %s up\n", link)
	// resolved learns of a new link a moment after the kernel has it.
	fmt.Fprintf(&b, "i=0; until resolvectl status %s >/dev/null 2>&1 || [ $i -ge 50 ]; do i=$((i+1)); sleep 0.1; done\n", link)
	fmt.Fprintf(&b, "resolvectl dns %s 127.0.0.1:%d\n", link, port)
	fmt.Fprintf(&b, "resolvectl domain %s %s\n", link, routingDomains(routes))
	fmt.Fprintf(&b, "resolvectl default-route %s false\n", link)
	return resolverChange{plan: shStep(h.elevated, b.String()), record: resolverRecord{Suffixes: routes, Port: port, Link: link}}, nil
}

// resolverLink is the name of the dummy link N on Linux.
func resolverLink(n int) string { return fmt.Sprintf("gcpemu-dns%d", n) }

// resolverMarker marks what gcpemu wrote for instance.
func resolverMarker(instance string) string { return "written by gcpemu (" + instance + ")" }

// darwinMarker is the line that marks instance's /etc/resolver files.
func darwinMarker(instance string) string { return "# " + resolverMarker(instance) }

// darwinResolverPlan writes one /etc/resolver/<suffix> per route, marked
// with the Instance's name; it removes only files that still carry it.
func darwinResolverPlan(h resolverHost, e resolverEdit) (resolverChange, error) {
	marker := darwinMarker(e.instance)
	dir := h.resolverDir()
	var b strings.Builder
	if len(e.removed) > 0 {
		for _, s := range e.removed {
			fmt.Fprintf(&b, "if grep -qxF '%[1]s' %[2]s/%[3]s 2>/dev/null; then rm -f %[2]s/%[3]s; fi\n", marker, dir, s)
		}
		return resolverChange{plan: shStep(h.elevated, b.String()), record: e.remaining()}, nil
	}
	var foreign []string
	for _, s := range e.routes {
		if f, ok := h.files[s]; ok && !slices.Contains(strings.Split(f, "\n"), marker) {
			foreign = append(foreign, s+" ("+dir+"/"+s+")")
		}
	}
	if err := refuseForeign(foreign); err != nil {
		return resolverChange{}, err
	}
	b.WriteString("mkdir -p " + dir + "\n")
	for _, s := range e.routes {
		fmt.Fprintf(&b, "printf '%%s\\n' '%s' 'nameserver 127.0.0.1' 'port %d' > %s/%s\n", marker, e.port, dir, s)
	}
	return resolverChange{plan: shStep(h.elevated, b.String()), record: resolverRecord{Suffixes: e.routes, Port: e.port}}, nil
}

// windowsResolverPlan adds one NRPT rule per route, commented with the
// Instance's name, and removes only rules that carry that comment. NRPT
// can't name a port, so routes need dns on 53 (ADR 0026).
func windowsResolverPlan(h resolverHost, e resolverEdit) (resolverChange, error) {
	comment := resolverMarker(e.instance)
	var b strings.Builder
	if len(e.removed) > 0 {
		for _, s := range e.removed {
			fmt.Fprintf(&b, "Get-DnsClientNrptRule | Where-Object { $_.Comment -eq '%s' -and $_.Namespace -contains '.%s' } | Remove-DnsClientNrptRule -Force\n", comment, s)
		}
		return resolverChange{plan: []trustStep{powershellStep(h.elevated, b.String())}, record: e.remaining()}, nil
	}
	if e.port != 53 {
		return resolverChange{}, fmt.Errorf("Windows' NRPT can't name a port, so Resolver routes need the dns Service endpoint on port 53, not %d: start the Instance with --port dns=53", e.port)
	}
	var foreign []string
	for _, s := range e.routes {
		ours := false
		for _, r := range h.nrpt {
			if !slices.Contains(r.Namespace, "."+s) && !slices.Contains(r.Namespace, s) {
				continue
			}
			if r.Comment == comment {
				ours = true
			} else {
				foreign = append(foreign, fmt.Sprintf("%s (NRPT rule %s for %s)", s, r.Name, strings.Join(r.Namespace, " ")))
			}
		}
		local := ours
		for _, r := range h.nrpt {
			local = local || slices.Contains(r.Namespace, "."+s) || slices.Contains(r.Namespace, s)
		}
		for _, n := range h.policy {
			if !local && (n == "."+s || n == s) {
				foreign = append(foreign, fmt.Sprintf("%s (NRPT policy for %s, from Group Policy or a VPN client)", s, n))
			}
		}
		if !ours {
			fmt.Fprintf(&b, "Add-DnsClientNrptRule -Namespace '.%s' -NameServers '127.0.0.1' -Comment '%s'\n", s, comment)
		}
	}
	if err := refuseForeign(foreign); err != nil {
		return resolverChange{}, err
	}
	rec := resolverRecord{Suffixes: e.routes, Port: e.port}
	if b.Len() == 0 {
		return resolverChange{record: rec}, nil
	}
	return resolverChange{plan: []trustStep{powershellStep(h.elevated, b.String())}, record: rec}, nil
}

// remaining is the Instance's record once e's routes are removed.
func (e resolverEdit) remaining() resolverRecord {
	if len(e.routes) == 0 {
		return resolverRecord{}
	}
	rec := e.old
	rec.Suffixes = e.routes
	return rec
}

// shStep runs script with sh, under sudo unless root, stopping at the
// first command that fails.
func shStep(root bool, script string) []trustStep {
	return []trustStep{{args: sudoUnless(root, "sh", "-s"), stdin: []byte("set -e\n" + script), script: true}}
}

// powershellStep runs script in Windows PowerShell; unless elevated, in
// one started through UAC, which shows its dialog even with --yes.
func powershellStep(elevated bool, script string) trustStep {
	script = "$ErrorActionPreference = 'Stop'\n" + script
	u := utf16.Encode([]rune(script))
	raw := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(raw[2*i:], c)
	}
	enc := base64.StdEncoding.EncodeToString(raw)
	ps := []string{"powershell", "-NoProfile", "-NonInteractive"}
	if elevated {
		return trustStep{
			args:  append(ps, "-EncodedCommand", enc),
			shown: strings.Join(ps, " ") + " -EncodedCommand <the script below>",
			text:  []byte(script),
		}
	}
	return trustStep{
		args: append(ps, "-Command", "$p = Start-Process powershell -Verb RunAs -Wait -PassThru -ArgumentList "+
			"'-NoProfile','-NonInteractive','-EncodedCommand','"+enc+"'; exit $p.ExitCode"),
		shown: strings.Join(ps, " ") + " -Command Start-Process powershell -Verb RunAs ... -EncodedCommand <the script below> (asks through UAC)",
		text:  []byte(script),
	}
}

// refuseForeign is the error for configuration gcpemu didn't write that
// already covers suffixes, each described by one of foreign.
func refuseForeign(foreign []string) error {
	if len(foreign) == 0 {
		return nil
	}
	return fmt.Errorf("configuration gcpemu didn't write already covers %s; remove it first (nothing was changed)", strings.Join(foreign, ", "))
}

// suffixOwner returns the Instance other than instance whose record holds
// suffix, if any.
func suffixOwner(records map[string]resolverRecord, instance, suffix string) string {
	for _, name := range sortedKeys(records) {
		if name != instance && slices.Contains(records[name].Suffixes, suffix) {
			return name
		}
	}
	return ""
}

// resolverLinkNumber returns N of the gcpemu-dnsN link that holds
// instance's routes on Linux: the one its record names, else the lowest
// that no other Instance's record names and no link on the host has.
func resolverLinkNumber(h resolverHost, records map[string]resolverRecord, instance string) int {
	var n int
	if _, err := fmt.Sscanf(records[instance].Link, "gcpemu-dns%d", &n); err == nil {
		return n
	}
	for n = 0; ; n++ {
		link := resolverLink(n)
		_, held := h.links[link]
		for _, r := range records {
			held = held || r.Link == link
		}
		if !held {
			return n
		}
	}
}

// routingDomains is suffixes as quoted resolvectl routing-only domains.
func routingDomains(suffixes []string) string {
	q := make([]string, len(suffixes))
	for i, s := range suffixes {
		q[i] = "'~" + s + "'"
	}
	return strings.Join(q, " ")
}

// sudoUnless prefixes args with sudo unless the process is root.
func sudoUnless(root bool, args ...string) []string {
	if root {
		return args
	}
	return append([]string{"sudo"}, args...)
}

// checkResolverRoutes reports, for `gcpemu doctor`, whether each of rec's
// routes is still on the host and goes to port, the Instance's dns port
// now. It checks the configuration, not live lookups.
func checkResolverRoutes(out io.Writer, h resolverHost, instance string, rec resolverRecord, port int) {
	want := fmt.Sprintf("127.0.0.1:%d", port)
	stale := false
	for _, s := range rec.Suffixes {
		target, ok := h.routeTarget(instance, rec, s)
		switch {
		case !ok:
			fmt.Fprintf(out, "[WARN] Resolver route for %s is gone\n", s)
			stale = true
		case target != want:
			fmt.Fprintf(out, "[WARN] Resolver route for %s goes to %s, but the dns Service endpoint is on %s\n", s, target, want)
			stale = true
		default:
			fmt.Fprintf(out, "[ok  ] Resolver route for %s to %s\n", s, target)
		}
	}
	if stale {
		fix := "run `gcpemu dns install` again"
		if h.goos == "windows" && port != 53 {
			fix = "start the Instance with --port dns=53, then " + fix
		}
		fmt.Fprintf(out, "       fix: %s\n", fix)
	}
}

// routeTarget returns where instance's route for suffix sends queries,
// and whether it is on the host.
func (h resolverHost) routeTarget(instance string, rec resolverRecord, suffix string) (string, bool) {
	switch h.goos {
	case "linux":
		l, ok := h.links[rec.Link]
		if !ok || !slices.Contains(l.domains, "~"+suffix) || len(l.dns) == 0 {
			return "", false
		}
		return l.dns[0], true
	case "darwin":
		f, ok := h.files[suffix]
		lines := strings.Split(f, "\n")
		if !ok || !slices.Contains(lines, darwinMarker(instance)) {
			return "", false
		}
		port := "53"
		for _, l := range lines {
			if p, ok := strings.CutPrefix(l, "port "); ok {
				port = strings.TrimSpace(p)
			}
		}
		return "127.0.0.1:" + port, true
	case "windows":
		for _, r := range h.nrpt {
			if r.Comment == resolverMarker(instance) && slices.Contains(r.Namespace, "."+suffix) {
				return "127.0.0.1:53", true
			}
		}
	}
	return "", false
}
