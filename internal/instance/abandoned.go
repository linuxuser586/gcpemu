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

// Abandoned is an Abandoned Instance ID: one that still tags containers,
// networks or volumes but that no data dir holds, so no future Instance
// reclaims them (#192).
type Abandoned struct {
	ID string
	// Unverified: none of its objects records its data dir (LabelDir), so
	// only the dirs under StateHome were searched for one holding the ID;
	// a --data-dir Instance elsewhere may still own it.
	Unverified bool
	// Dirs are the data dirs its objects record.
	Dirs       []string
	Containers []runtime.Container
	Networks   []runtime.Network
	Volumes    []runtime.Volume
}

// FindAbandoned returns the Abandoned Instance IDs on the runtime, sorted.
// An ID is never Abandoned while any of its containers runs, while a dir
// its objects record holds it in its instance-id file, or while a dir
// under stateHome does. A dir that looks missing may be on an unmounted
// disk or inside a container, so callers confirm before pruning.
func FindAbandoned(ctx context.Context, cl *runtime.Client, stateHome string) ([]Abandoned, error) {
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
	byID := map[string]*Abandoned{}
	running := map[string]bool{}
	labelled := map[string]bool{}
	get := func(l map[string]string) *Abandoned {
		id := l[runtime.LabelInstance]
		a := byID[id]
		if a == nil {
			a = &Abandoned{ID: id}
			byID[id] = a
		}
		if d := l[runtime.LabelDir]; d != "" {
			labelled[id] = true
			if !slices.Contains(a.Dirs, d) {
				a.Dirs = append(a.Dirs, d)
			}
		}
		return a
	}
	for _, c := range cs {
		a := get(c.Labels)
		a.Containers = append(a.Containers, c)
		running[a.ID] = running[a.ID] || c.Running
	}
	for _, n := range nets {
		a := get(n.Labels)
		a.Networks = append(a.Networks, n)
	}
	for _, v := range vols {
		a := get(v.Labels)
		a.Volumes = append(a.Volumes, v)
	}

	held := map[string]bool{}
	// Glob fails only on a malformed pattern.
	idFiles, _ := filepath.Glob(filepath.Join(stateHome, "*", IDFile))
	for _, f := range idFiles {
		held[readID(filepath.Dir(f))] = true
	}
	var out []Abandoned
	for id, a := range byID {
		if id == "" || running[id] || held[id] || slices.ContainsFunc(a.Dirs, func(d string) bool { return readID(d) == id }) {
			continue
		}
		a.Unverified = !labelled[id]
		sort.Strings(a.Dirs)
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
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
func (a Abandoned) Prune(ctx context.Context, cl *runtime.Client) error {
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
