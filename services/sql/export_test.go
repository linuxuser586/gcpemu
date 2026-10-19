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

// SetLoopbackAliases replaces the loopback alias probe (exported to the
// external tests).
func SetLoopbackAliases(svc emu.Service, aliases bool) {
	svc.(*Service).aliases = func() bool { return aliases }
}

// BaseBackup is a Base backup in a WAL archive (exported to the external
// tests).
type BaseBackup = baseBackup

// WALArchive lists an instance's Base backups and archived WAL file names
// without pruning (exported to the external tests).
func WALArchive(ctx context.Context, svc emu.Service, project, name string) ([]BaseBackup, []string, error) {
	a, err := svc.(*Service).readArchive(ctx, project, name)
	return a.Bases, a.WAL, err
}

// ExecIn runs a command in an instance's container (exported to the
// external tests).
func ExecIn(ctx context.Context, svc emu.Service, project, name string, cmd ...string) (string, error) {
	return svc.(*Service).execIn(ctx, project, name, "", cmd...)
}

// SegmentBefore reports whether a WAL file name is a segment positioned
// before the segment start, on any timeline (exported to the external
// tests).
func SegmentBefore(name, start string) bool {
	_, drop := walFrom(start, []string{name})
	return len(drop) == 1
}
