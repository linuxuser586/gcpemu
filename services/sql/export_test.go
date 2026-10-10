package sql

import (
	"context"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

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

// MarkFailed removes an instance's container and marks it FAILED, as a
// failed recovery leaves it (exported to the external tests).
func MarkFailed(ctx context.Context, svc emu.Service, project, name string) error {
	s := svc.(*Service)
	mu := s.lock(project, name)
	defer mu.Unlock()
	if err := s.stopContainer(ctx, project, name); err != nil {
		return err
	}
	return s.setState(project, name, "FAILED")
}
