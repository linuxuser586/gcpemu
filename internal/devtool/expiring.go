package main

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// expiringExceptions lists the scan exceptions in files (each scan
// exception file's contents, by name) that stop holding before now+within,
// soonest first, as Markdown lines for the weekly scan's rolling issue
// (ADR 0090). An entry the checks already reject is listed too when its
// date is readable.
func expiringExceptions(files map[string][]byte, now time.Time, within time.Duration) ([]string, error) {
	type expiring struct {
		date time.Time
		line string
	}
	var all []expiring
	for file, b := range files {
		var entries []exception
		if file == trivyIgnoreFile {
			entries, _ = parseTrivyIgnore(b)
		} else {
			var err error
			if entries, err = decodeExceptions(b); err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
		}
		for _, e := range entries {
			d, err := time.Parse(time.DateOnly, e.Expires)
			if err != nil || !d.Before(now.Add(within)) {
				continue
			}
			all = append(all, expiring{d, fmt.Sprintf("`%s` (%s) stops holding on %s: %s", e.ID, file, e.Expires, e.Issue)})
		}
	}
	slices.SortFunc(all, func(a, b expiring) int {
		return cmp.Or(a.date.Compare(b.date), cmp.Compare(a.line, b.line))
	})
	out := make([]string, len(all))
	for i, e := range all {
		out[i] = e.line
	}
	return out, nil
}
