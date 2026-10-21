package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// readResolverHost describes goos's host and, with read, reads what is on
// its resolver now; etc stands in for /etc on macOS (tests).
func readResolverHost(goos, etc string, read bool) (resolverHost, error) {
	h := resolverHost{goos: goos, elevated: elevated(), etc: etc}
	if !read {
		return h, nil
	}
	switch goos {
	case "linux":
		domains, err := exec.Command("resolvectl", "domain").Output()
		if err != nil {
			return h, nil
		}
		servers, err := exec.Command("resolvectl", "dns").Output()
		if err != nil {
			return h, nil
		}
		h.resolved = true
		h.links = map[string]resolvedLink{}
		for name, d := range parseResolvectl(string(domains)) {
			l := h.links[name]
			l.domains = d
			h.links[name] = l
		}
		for name, d := range parseResolvectl(string(servers)) {
			l := h.links[name]
			l.dns = d
			h.links[name] = l
		}
	case "darwin":
		h.files = map[string]string{}
		dir := h.resolverDir()
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return h, err
		}
		for _, e := range entries {
			if e.Type().IsRegular() {
				b, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					return h, err
				}
				h.files[e.Name()] = string(b)
			}
		}
	case "windows":
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"ConvertTo-Json -Compress -InputObject @(Get-DnsClientNrptRule | ForEach-Object { "+
				"[pscustomobject]@{ Name = $_.Name; Namespace = @($_.Namespace) -join ' '; Comment = [string]$_.Comment } })").Output()
		if err != nil {
			return h, fmt.Errorf("reading the NRPT rules (Get-DnsClientNrptRule): %w", err)
		}
		if h.nrpt, err = parseNRPT(out); err != nil {
			return h, fmt.Errorf("reading the NRPT rules: %w", err)
		}
		out, err = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"ConvertTo-Json -Compress -InputObject @(Get-DnsClientNrptPolicy | ForEach-Object { [string]$_.Namespace })").Output()
		if err != nil {
			return h, fmt.Errorf("reading the NRPT policy (Get-DnsClientNrptPolicy): %w", err)
		}
		if err := json.Unmarshal(out, &h.policy); err != nil {
			return h, fmt.Errorf("reading the NRPT policy: %w", err)
		}
	}
	return h, nil
}

// resolverDir is macOS's /etc/resolver.
func (h resolverHost) resolverDir() string {
	etc := h.etc
	if etc == "" {
		etc = "/etc"
	}
	return etc + "/resolver"
}

// parseResolvectl reads `resolvectl domain` or `resolvectl dns` output:
// each link's values by its name, "Global" for the global ones.
func parseResolvectl(out string) map[string][]string {
	links := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		head, values, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name := strings.TrimSpace(head)
		if i, j := strings.Index(head, "("), strings.LastIndex(head, ")"); strings.HasPrefix(name, "Link ") && i >= 0 && j > i {
			name = head[i+1 : j]
		} else if name != "Global" {
			continue
		}
		links[name] = strings.Fields(values)
	}
	return links
}

// parseNRPT reads the rules readResolverHost lists: a JSON array of
// rules whose namespaces are joined by spaces.
func parseNRPT(out []byte) ([]nrptRule, error) {
	var raw []struct{ Name, Namespace, Comment string }
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	rules := make([]nrptRule, len(raw))
	for i, r := range raw {
		rules[i] = nrptRule{Name: r.Name, Namespace: strings.Fields(r.Namespace), Comment: r.Comment}
	}
	return rules, nil
}
