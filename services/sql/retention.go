package sql

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// Transaction log retention (#365, ADR 0022). A Cloud SQL instance keeps
// its WAL archive to transactionLogRetentionDays, measured on PostgreSQL's
// clock: the anchor is the newest Base backup that ended at or before
// now − N days. The anchor and every newer Base backup are kept, older
// ones are dropped, and so is every archived WAL segment before the oldest
// kept Base backup's start segment. Timeline history files are always kept.

// baseBackup is one Base backup in a WAL archive.
type baseBackup struct {
	// End is PostgreSQL's clock (Unix microseconds) once the backup was
	// complete; it names the backup's directory.
	End int64
	// Start is the WAL segment replay starts from, as the backup's
	// manifest records it; empty when it is unknown.
	Start string
}

// archivePlan is what transaction log retention keeps and drops.
type archivePlan struct {
	Keep     []baseBackup // oldest first
	DropBase []baseBackup
	DropWAL  []string
}

// planRetention applies a retention of days to a WAL archive's Base
// backups and archived WAL file names as of now.
func planRetention(bases []baseBackup, wal []string, now time.Time, days int64) archivePlan {
	bases = append([]baseBackup(nil), bases...)
	sort.Slice(bases, func(i, j int) bool { return bases[i].End < bases[j].End })
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).UnixMicro()
	anchor := -1
	for i, b := range bases {
		if b.End <= cutoff {
			anchor = i
		}
	}
	if anchor < 0 {
		return archivePlan{Keep: bases}
	}
	p := archivePlan{Keep: bases[anchor:], DropBase: bases[:anchor]}
	_, p.DropWAL = walFrom(p.Keep[0].Start, wal)
	return p
}

// walFrom splits archived WAL file names into those a replay from the
// segment start needs (every segment at or after its position, on any
// timeline, and every file that is not a segment, such as timeline
// history) and the segments before it. An unknown start keeps everything.
func walFrom(start string, wal []string) (keep, drop []string) {
	from, ok := segmentPos(start)
	for _, f := range wal {
		if pos, seg := segmentPos(f); ok && seg && pos < from {
			drop = append(drop, f)
		} else {
			keep = append(keep, f)
		}
	}
	return keep, drop
}

// segmentPos returns the position (log and segment number, without the
// timeline) of a WAL segment file name, or of a .partial or .backup file
// named after one. Timeline history files are not segments.
func segmentPos(name string) (string, bool) {
	if len(name) < 24 || (len(name) > 24 && name[24] != '.') {
		return "", false
	}
	for _, c := range name[:24] {
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return "", false
		}
	}
	return name[8:24], true
}

// walRange matches one entry of a backup_manifest's WAL-Ranges.
var walRange = regexp.MustCompile(`"Timeline":\s*(\d+),\s*"Start-LSN":\s*"([0-9A-Fa-f]+)/([0-9A-Fa-f]+)"`)

// manifestStart returns the WAL segment a Base backup starts from, given
// the WAL-Ranges section of its backup_manifest and the cluster's WAL
// segment size in bytes: the segment of the start LSN on the oldest
// timeline the backup spans. It returns "" when there is no range.
func manifestStart(ranges string, segSize uint64) string {
	var tli, lsn uint64
	found := false
	for _, m := range walRange.FindAllStringSubmatch(ranges, -1) {
		t, err1 := strconv.ParseUint(m[1], 10, 32)
		hi, err2 := strconv.ParseUint(m[2], 16, 32)
		lo, err3 := strconv.ParseUint(m[3], 16, 32)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		if !found || t < tli {
			tli, lsn, found = t, hi<<32|lo, true
		}
	}
	if !found || segSize == 0 {
		return ""
	}
	seg, perLog := lsn/segSize, uint64(1<<32)/segSize
	return fmt.Sprintf("%08X%08X%08X", tli, seg/perLog, seg%perLog)
}
