package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

const waitUsage = "usage: gcpemu wait --for KIND=PROJECT/LOCATION/NAME [--for ...] [--timeout 120s]"

// waitPoll is how often `gcpemu wait` asks the Instance.
const waitPoll = 250 * time.Millisecond

// waitResult is the body of /_emu/v1/conditions.
type waitResult struct {
	Met        bool
	Conditions []struct {
		For    string
		State  emu.WaitState
		Reason string
	}
	Error string
}

func (o *rootOpts) waitCmd() *cobra.Command {
	var fors []string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait --for KIND=TARGET [--for ...]",
		Short: "Block until every Wait condition holds",
		Long: `Block until every Wait condition holds (FR-CI-007). A Wait condition is
KIND=PROJECT/LOCATION/NAME:

  gke/cluster  the GKE cluster's status is RUNNING (LOCATION: zone or region)
  lb/health    every backend group of the backend service has a HEALTHY
               endpoint (LOCATION: global or a region)

A resource that does not exist yet keeps wait waiting. wait fails straight
away when a Wait condition can no longer hold (e.g. a cluster in ERROR), and
otherwise once --timeout passes, naming each Wait condition not met.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(fors) == 0 {
				return waitUsageError(errors.New("at least one --for is required"))
			}
			for _, f := range fors {
				if _, _, err := emu.ParseWaitCondition(f); err != nil {
					return waitUsageError(err)
				}
			}
			cfg, err := o.load(cmd.Flags())
			if err != nil {
				return err
			}
			c, err := clientFor(cfg)
			if err != nil {
				return fmt.Errorf("instance %q is not running", cfg.Instance)
			}
			path := "/_emu/v1/conditions?" + url.Values{"for": fors}.Encode()
			deadline := time.Now().Add(timeout)
			unmet := []string{"  (no answer from the instance yet)"}
			for {
				res, err := pollWait(c, path, deadline)
				if errors.Is(err, context.DeadlineExceeded) {
					return fmt.Errorf("timed out after %v waiting for:\n%s", timeout, strings.Join(unmet, "\n"))
				}
				if errors.Is(err, errNoAnswer) {
					return fmt.Errorf("instance %q is not running: %w", cfg.Instance, err)
				}
				if err != nil {
					return err
				}
				if res.Error != "" {
					return waitUsageError(errors.New(res.Error))
				}
				if res.Met {
					return nil
				}
				unmet = unmet[:0]
				failed := false
				for _, r := range res.Conditions {
					if r.State != emu.WaitMet {
						unmet = append(unmet, "  "+r.For+": "+r.Reason)
					}
					failed = failed || r.State == emu.WaitFailed
				}
				if failed {
					return fmt.Errorf("a Wait condition can no longer hold:\n%s", strings.Join(unmet, "\n"))
				}
				left := time.Until(deadline)
				if left <= 0 {
					return fmt.Errorf("timed out after %v waiting for:\n%s", timeout, strings.Join(unmet, "\n"))
				}
				time.Sleep(min(waitPoll, left))
			}
		},
	}
	cmd.Flags().StringArrayVar(&fors, "for", nil, "a Wait condition, KIND=PROJECT/LOCATION/NAME (repeatable)")
	cmd.Flags().DurationVar(&timeout, "timeout", 120*time.Second, "how long to wait before failing")
	return cmd
}

// errNoAnswer is a poll that reached no Instance.
var errNoAnswer = errors.New("no answer")

// pollWait asks the Instance once, giving up at deadline (an error that
// matches context.DeadlineExceeded). A 400, a malformed Wait condition,
// comes back as a result with Error set.
func pollWait(c *adminClient, path string, deadline time.Time) (*waitResult, error) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	req, err := c.newRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", errNoAnswer, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var res waitResult
	if err := json.Unmarshal(body, &res); err != nil || resp.StatusCode != http.StatusOK && !(resp.StatusCode == http.StatusBadRequest && res.Error != "") {
		return nil, fmt.Errorf("asking for the Wait conditions: %d %s", resp.StatusCode, body)
	}
	return &res, nil
}

func waitUsageError(err error) error { return fmt.Errorf("%w\n%s", err, waitUsage) }
