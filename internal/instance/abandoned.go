package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// Tagged is an Instance ID with the containers, networks and volumes it
// tags on the runtime.
type Tagged struct {
	ID string
	// Dirs are the data dirs its objects record.
	Dirs       []string
	Containers []runtime.Container
	Networks   []runtime.Network
	Volumes    []runtime.Volume
}

// Abandoned is an Abandoned Instance ID: one that still tags containers,
// networks or volumes but that no data dir holds, or that only Test data
// dirs whose test has ended hold, so no future Instance reclaims them
// (#192, #914).
type Abandoned struct {
	Tagged
	// Unverified: none of its objects records its data dir (LabelDir), so
	// only the dirs under StateHome were searched for one holding the ID;
	// a --data-dir Instance elsewhere may still own it.
	Unverified bool
	// EndedTestDirs are the Test data dirs whose test has ended that hold
	// the ID; Reclaim removes them too. Its containers may still run.
	EndedTestDirs []string
}

// EndedTest reports whether only Test data dirs whose test has ended hold
// the ID.
func (a Abandoned) EndedTest() bool { return len(a.EndedTestDirs) > 0 }

// FindAbandoned returns the Abandoned Instance IDs on the runtime, sorted.
// An ID that a dir its objects record, or a dir under stateHome, holds in
// its instance-id file is Abandoned only when every such dir is a Test
// data dir whose owner lock is free: its test has ended, even if its
// containers still run (ADR 0057). Any other ID is never Abandoned while
// any of its containers runs. A dir that looks missing may be on an
// unmounted disk or inside a container, so callers confirm before
// pruning.
func FindAbandoned(ctx context.Context, cl *runtime.Client, stateHome string) ([]Abandoned, error) {
	tagged, err := FindTagged(ctx, cl)
	if err != nil {
		return nil, err
	}
	holders := map[string][]string{}
	// Glob fails only on a malformed pattern.
	idFiles, _ := filepath.Glob(filepath.Join(stateHome, "*", IDFile))
	for _, f := range idFiles {
		dir := filepath.Dir(f)
		id := readID(dir)
		holders[id] = append(holders[id], dir)
	}
	var out []Abandoned
	for _, a := range tagged {
		held := holders[a.ID]
		for _, d := range a.Dirs {
			if readID(d) == a.ID && !slices.Contains(held, d) {
				held = append(held, d)
			}
		}
		if len(held) > 0 {
			if slices.ContainsFunc(held, func(d string) bool { return !testEnded(d) }) {
				continue
			}
			sort.Strings(held)
			out = append(out, Abandoned{Tagged: a, EndedTestDirs: held})
			continue
		}
		if slices.ContainsFunc(a.Containers, func(c runtime.Container) bool { return c.Running }) {
			continue
		}
		out = append(out, Abandoned{Tagged: a, Unverified: len(a.Dirs) == 0})
	}
	return out, nil
}

// FindTagged returns every Instance ID that tags containers, networks or
// volumes on the runtime, sorted. Unlike FindAbandoned it keeps the IDs
// that are still live, so the caller decides which to Prune.
func FindTagged(ctx context.Context, cl *runtime.Client) ([]Tagged, error) {
	sel := map[string]string{runtime.LabelInstance: ""}
	cs, err := cl.ListContainers(ctx, sel)
	if err != nil {
		return nil, err
	}
	nets, err := cl.ListNetworks(ctx, sel)
	if err != nil {
		return nil, err
	}
	vols, err := cl.Volumes(ctx, sel)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Tagged{}
	get := func(l map[string]string) *Tagged {
		id := l[runtime.LabelInstance]
		a := byID[id]
		if a == nil {
			a = &Tagged{ID: id}
			byID[id] = a
		}
		if d := l[runtime.LabelDir]; d != "" && !slices.Contains(a.Dirs, d) {
			a.Dirs = append(a.Dirs, d)
		}
		return a
	}
	for _, c := range cs {
		a := get(c.Labels)
		a.Containers = append(a.Containers, c)
	}
	for _, n := range nets {
		a := get(n.Labels)
		a.Networks = append(a.Networks, n)
	}
	for _, v := range vols {
		a := get(v.Labels)
		a.Volumes = append(a.Volumes, v)
	}
	var out []Tagged
	for id, a := range byID {
		if id == "" {
			continue
		}
		sort.Strings(a.Dirs)
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// taggedByID returns what the Instance ID id tags on the runtime.
func taggedByID(ctx context.Context, cl *runtime.Client, id string) (Tagged, error) {
	tagged, err := FindTagged(ctx, cl)
	if err != nil {
		return Tagged{}, err
	}
	for _, a := range tagged {
		if a.ID == id {
			return a, nil
		}
	}
	return Tagged{ID: id}, nil
}

// readID returns the Instance ID that dir holds, or "".
func readID(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, IDFile))
	if err != nil {
		return ""
	}
	return string(b)
}

// Prune removes the ID's containers, then its networks, then its volumes,
// in the order an Instance cleans up after itself.
func (a Tagged) Prune(ctx context.Context, cl *runtime.Client) error {
	var errs []error
	for _, c := range a.Containers {
		if err := cl.RemoveContainer(ctx, c.ID, true); err != nil {
			errs = append(errs, fmt.Errorf("remove container %s: %w", c.Name, err))
		}
	}
	for _, n := range a.Networks {
		if err := cl.RemoveNetwork(ctx, n.ID); err != nil {
			errs = append(errs, fmt.Errorf("remove network %s: %w", n.Name, err))
		}
	}
	for _, v := range a.Volumes {
		if err := cl.RemoveVolume(ctx, v.Name); err != nil {
			errs = append(errs, fmt.Errorf("remove volume %s: %w", v.Name, err))
		}
	}
	return errors.Join(errs...)
}
