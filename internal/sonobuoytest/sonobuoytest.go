// Package sonobuoytest runs Kubernetes conformance, Sonobuoy's quick mode,
// for gcpemu's own tests: the Autopilot acceptance (e2e) on Autopilot
// clusters and TestKubeConformance (compat) on Standard clusters.
//
// Only the e2e plugin runs, on Standard and Autopilot clusters alike: the systemd-logs
// plugin's DaemonSet is privileged and mounts host paths, which Autopilot
// refuses, as GKE does, and on a Standard cluster it only gathers node
// journals, asserting nothing about conformance (#754).
package sonobuoytest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Version is the pinned Sonobuoy release.
const Version = "0.57.5"

// SHA256 is Version's archive checksums (sonobuoy_<version>_checksums.txt)
// by GOOS/GOARCH.
var SHA256 = map[string]string{
	"linux/amd64":   "7c5c2250e5103c98f4dad2bcab86baf954e319c57c419c113f28f5060e62f129",
	"linux/arm64":   "ec482b5d1ec237f4c94b6fd7be5a69f95678a4331001b3edcb6ad7835ec40695",
	"darwin/amd64":  "4202d3a960a812e68b555829eefbe0cc25de8b2e5cb2fc8e5cc38dfc83515a9d",
	"darwin/arm64":  "d889c4a4485f3899f8d860506ed4da5380372f23f91cd0113e52f036d842e978",
	"windows/amd64": "041500d35af9720cb8e0ec3584b32243e6555c1e7413caadf9ddb8c336354f63",
}

// URL returns the Version archive's URL for platform ("linux/amd64").
func URL(platform string) string {
	return "https://github.com/vmware-tanzu/sonobuoy/releases/download/v" + Version +
		"/sonobuoy_" + Version + "_" + strings.ReplaceAll(platform, "/", "_") + ".tar.gz"
}

// wait is how long `sonobuoy run` waits for the e2e plugin. Its process
// gets a few minutes more, so that Sonobuoy, not the timeout, ends a slow
// run and says where the plugin got to.
const wait = 20 * time.Minute

// Quick is a quick-mode run against one cluster.
type Quick struct {
	// Sonobuoy runs sonobuoy with args against the cluster, within timeout.
	Sonobuoy func(timeout time.Duration, args ...string) (string, error)
	// Keep, if set, is the file a failed run's results tarball is copied to.
	Keep string
	// Diagnose, if set, explains a run that failed or left no results (the
	// sonobuoy pods' logs, say).
	Diagnose func() string
	// Logf logs the results.
	Logf func(format string, args ...any)
}

// Run runs quick mode on a cluster at version ("1.36.5-gke.100"), keeping
// the results tarball in dir, and returns an error unless the e2e plugin
// passed with at least one test passed.
func (q Quick) Run(dir, version string) error {
	// The conformance image's version, not the one Sonobuoy would read off
	// the server, which carries k3s's "+k3s1".
	kube := "v" + strings.SplitN(version, "-", 2)[0]
	start := time.Now()
	out, runErr := q.Sonobuoy(wait+5*time.Minute, "run", "--mode", "quick", "--plugin", "e2e",
		"--kubernetes-version", kube, "--wait="+strconv.Itoa(int(wait/time.Minute)))
	q.Logf("sonobuoy run on %s (%v)", kube, time.Since(start).Round(time.Second))
	if runErr != nil {
		runErr = fmt.Errorf("sonobuoy run: %v\n%s", runErr, tail(out, 40))
	}
	file := "results.tar.gz"
	if q.Keep != "" {
		file = filepath.Base(q.Keep)
	}
	if out, err := q.Sonobuoy(2*time.Minute, "retrieve", dir, "--filename", file); err != nil {
		// A run that never finished has no results to retrieve.
		status, _ := q.Sonobuoy(time.Minute, "status")
		return errors.Join(runErr, fmt.Errorf("sonobuoy retrieve: %v\n%s\nsonobuoy status:\n%s%s", err, tail(out, 40), status, q.diagnosis()))
	}
	if runErr != nil {
		runErr = fmt.Errorf("%w%s", runErr, q.diagnosis())
	}
	tarball := filepath.Join(dir, file)
	report, err := q.Sonobuoy(time.Minute, "results", tarball, "--plugin", "e2e")
	if err == nil {
		q.Logf("sonobuoy results:\n%s", report)
		err = judgeReport(report)
	} else {
		err = fmt.Errorf("sonobuoy results: %v\n%s", err, tail(report, 40))
	}
	if err != nil {
		err = fmt.Errorf("Kubernetes conformance on %s: %w", kube, err)
	}
	err = errors.Join(runErr, err)
	if q.Keep != "" {
		_ = os.Remove(q.Keep) // a tarball from an earlier run
	}
	if err == nil {
		return nil
	}
	detailed, _ := q.Sonobuoy(time.Minute, "results", tarball, "--plugin", "e2e", "--mode", "detailed")
	q.Logf("sonobuoy results --mode detailed (skipped tests left out):\n%s", withoutSkipped(detailed))
	if q.Keep != "" {
		if kerr := keep(tarball, q.Keep); kerr != nil {
			return errors.Join(err, fmt.Errorf("keeping the results tarball: %w", kerr))
		}
		q.Logf("results tarball: %s", q.Keep)
	}
	return err
}

// diagnosis is Diagnose's explanation on a line of its own, if any.
func (q Quick) diagnosis() string {
	if q.Diagnose == nil {
		return ""
	}
	return "\n" + q.Diagnose()
}

// keep copies the tarball to dst.
func keep(tarball, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(tarball)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// withoutSkipped drops the skipped tests from `sonobuoy results --mode
// detailed`, one JSON object a line: quick mode skips all but one.
func withoutSkipped(detailed string) string {
	var kept []string
	for _, l := range strings.Split(detailed, "\n") {
		if !strings.Contains(l, `"status":"skipped"`) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// judgeReport passes the e2e plugin's report (`sonobuoy results --plugin
// e2e`) when its status is passed and at least one test passed.
func judgeReport(report string) error {
	fields := map[string]string{}
	for _, l := range strings.Split(report, "\n") {
		if k, v, ok := strings.Cut(l, ": "); ok {
			if _, seen := fields[k]; !seen { // the first block is the plugin's
				fields[k] = strings.TrimSpace(v)
			}
		}
	}
	status, ok := fields["Status"]
	if !ok {
		return fmt.Errorf("no e2e plugin status: the plugin did not finish")
	}
	if status != "passed" {
		return fmt.Errorf("e2e plugin status %q, want passed", status)
	}
	passed, err := strconv.Atoi(fields["Passed"])
	if err != nil {
		return fmt.Errorf("e2e plugin report has no passed count")
	}
	if passed == 0 {
		return fmt.Errorf("e2e plugin passed with no test run")
	}
	return nil
}
