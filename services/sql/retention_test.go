package sql

import (
	"reflect"
	"testing"
	"time"
)

func TestRetentionDropsWhatIsOlderThanTheAnchor(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	day := func(n int) int64 { return now.AddDate(0, 0, -n).UnixMicro() }
	bases := []baseBackup{
		{End: day(10), Start: "000000010000000000000002"},
		{End: day(8), Start: "000000010000000000000005"},
		{End: day(3), Start: "000000010000000000000009"},
		{End: day(1), Start: "00000001000000000000000C"},
	}
	wal := []string{
		"000000010000000000000002", "000000010000000000000003", "000000010000000000000004",
		"000000010000000000000005", "000000010000000000000005.00000028.backup", "000000010000000000000006",
		"000000010000000000000009", "00000001000000000000000C",
	}
	p := planRetention(bases, wal, now, 7)
	if want := bases[1:]; !reflect.DeepEqual(p.Keep, want) {
		t.Errorf("kept %v, want %v", p.Keep, want)
	}
	if want := bases[:1]; !reflect.DeepEqual(p.DropBase, want) {
		t.Errorf("dropped base backups %v, want %v", p.DropBase, want)
	}
	if want := []string{"000000010000000000000002", "000000010000000000000003", "000000010000000000000004"}; !reflect.DeepEqual(p.DropWAL, want) {
		t.Errorf("dropped WAL %v, want %v", p.DropWAL, want)
	}
}

func TestRetentionWithoutAnAnchorDropsNothing(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	bases := []baseBackup{
		{End: now.AddDate(0, 0, -6).UnixMicro(), Start: "000000010000000000000002"},
		{End: now.Add(-time.Hour).UnixMicro(), Start: "000000010000000000000004"},
	}
	wal := []string{"000000010000000000000001", "000000010000000000000002", "000000010000000000000004"}
	p := planRetention(bases, wal, now, 7)
	if !reflect.DeepEqual(p.Keep, bases) || p.DropBase != nil || p.DropWAL != nil {
		t.Errorf("plan = %+v, want everything kept", p)
	}
}

func TestRetentionKeepsTimelineHistory(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	bases := []baseBackup{
		{End: now.AddDate(0, 0, -9).UnixMicro(), Start: "000000010000000000000002"},
		{End: now.AddDate(0, 0, -8).UnixMicro(), Start: "000000030000000000000007"},
	}
	wal := []string{"00000002.history", "000000010000000000000003", "00000003.history", "000000030000000000000007"}
	p := planRetention(bases, wal, now, 7)
	if want := []string{"000000010000000000000003"}; !reflect.DeepEqual(p.DropWAL, want) {
		t.Errorf("dropped WAL %v, want %v", p.DropWAL, want)
	}
}

// A Base backup taken on timeline 1 replays into timeline 2, which forked
// at segment 5: the segments of both timelines from its start are kept.
func TestRetentionAcrossTimelines(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	bases := []baseBackup{
		{End: now.AddDate(0, 0, -20).UnixMicro(), Start: "000000010000000000000001"},
		{End: now.AddDate(0, 0, -10).UnixMicro(), Start: "000000010000000000000004"},
		{End: now.AddDate(0, 0, -2).UnixMicro(), Start: "000000020000000000000008"},
	}
	wal := []string{
		"000000010000000000000001", "000000010000000000000003", "000000010000000000000004",
		"000000010000000000000005.partial", "00000002.history", "000000020000000000000003",
		"000000020000000000000005", "000000020000000000000008",
	}
	p := planRetention(bases, wal, now, 7)
	if want := bases[1:]; !reflect.DeepEqual(p.Keep, want) {
		t.Errorf("kept %v, want %v", p.Keep, want)
	}
	if want := []string{"000000010000000000000001", "000000010000000000000003", "000000020000000000000003"}; !reflect.DeepEqual(p.DropWAL, want) {
		t.Errorf("dropped WAL %v, want %v", p.DropWAL, want)
	}
}

// A restore bundle holds the WAL from its Base backup's start segment on,
// plus timeline history.
func TestBundleWAL(t *testing.T) {
	wal := []string{
		"00000002.history", "000000010000000000000003", "000000010000000000000004",
		"000000010000000000000004.00000028.backup", "000000020000000000000005",
	}
	keep, _ := walFrom("000000010000000000000004", wal)
	if want := []string{"00000002.history", "000000010000000000000004", "000000010000000000000004.00000028.backup", "000000020000000000000005"}; !reflect.DeepEqual(keep, want) {
		t.Errorf("bundle WAL %v, want %v", keep, want)
	}
	if keep, _ := walFrom("", wal); !reflect.DeepEqual(keep, wal) {
		t.Errorf("bundle WAL with an unknown start %v, want all of %v", keep, wal)
	}
}

// A Base backup's start segment comes from the WAL-Ranges of its
// backup_manifest; when the backup spans a timeline switch, from the
// oldest timeline's range.
func TestManifestStart(t *testing.T) {
	const mb16 = 16 << 20
	for _, c := range []struct{ ranges, want string }{
		{`"WAL-Ranges": [
{ "Timeline": 1, "Start-LSN": "0/5000028", "End-LSN": "0/5000100" }
],
"Manifest-Checksum": "abc"}`, "000000010000000000000005"},
		{`"WAL-Ranges": [
{ "Timeline": 3, "Start-LSN": "1/A4000000", "End-LSN": "1/A4000100" },
{ "Timeline": 2, "Start-LSN": "1/A3000060", "End-LSN": "1/A4000000" }
],`, "0000000200000001000000A3"},
		{`no manifest`, ""},
	} {
		if got := manifestStart(c.ranges, mb16); got != c.want {
			t.Errorf("manifestStart(%q) = %q, want %q", c.ranges, got, c.want)
		}
	}
}
