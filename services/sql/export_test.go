package sql

import "strings"

// AppliedFlags lists the allowed flags that are set on the server with
// ALTER SYSTEM for a major version (exported to the external tests).
func AppliedFlags(major int) []string {
	var out []string
	for _, name := range flagNames() {
		d, _ := lookupFlag(name)
		if d.Recorded || strings.HasPrefix(name, "cloudsql.") || (d.Since > 0 && major < d.Since) || (d.Until > 0 && major > d.Until) {
			continue
		}
		out = append(out, name)
	}
	return out
}
