// Package testlog keeps the emulator logs of gcpemu's tests in the dir
// GCPEMU_TEST_LOG_DIR names, so a CI job can upload those of a failed
// run (#1147).
package testlog

import (
	"os"
	"regexp"
)

// unsafeChars are what a file name made from a test name may not hold.
var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Create creates a new file in GCPEMU_TEST_LOG_DIR, creating the dir,
// named after the test name returns. It returns nil when the variable is
// unset, without calling name: a fake testing.TB may have no Name.
func Create(name func() string) (*os.File, error) {
	dir := os.Getenv("GCPEMU_TEST_LOG_DIR")
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, unsafeChars.ReplaceAllString(name(), "_")+"-*.log")
}
