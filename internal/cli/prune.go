package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/instance"
	emuruntime "github.com/linuxuser586/gcpemu/internal/runtime"
)

// pruneCmd removes the containers, networks and volumes of Abandoned
// Instance IDs (#192). It is CLI only: pruning is host-level work, and one
// Instance must not delete another's objects.
func (o *rootOpts) pruneCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "prune",
		Short: "Remove containers, networks and volumes of gcpemu instances whose data dir is gone",
		Long: "Remove the containers, networks and volumes that still carry the ID of a gcpemu instance whose data dir " +
			"no longer holds it (an Abandoned Instance ID), e.g. after the data dir was deleted or a run crashed. " +
			"IDs with a running container are never touched. It lists everything first and asks before removing anything.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := emuruntime.Unusable(); err != nil {
				return fmt.Errorf("no container runtime: %w", err)
			}
			cl, err := emuruntime.Detect()
			if err != nil {
				return fmt.Errorf("no container runtime: %w", err)
			}
			ctx := cmd.Context()
			found, err := instance.FindAbandoned(ctx, cl, config.StateHome())
			if err != nil {
				return err
			}
			return runPrune(cmd.OutOrStdout(), cmd.InOrStdin(), yes, found, func(a instance.Abandoned) error {
				return a.Prune(ctx, cl)
			})
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "remove without asking")
	return c
}

// runPrune lists found, asks unless yes, and prunes each ID.
func runPrune(out io.Writer, in io.Reader, yes bool, found []instance.Abandoned, prune func(instance.Abandoned) error) error {
	if len(found) == 0 {
		fmt.Fprintln(out, "nothing to prune: no abandoned Instance IDs")
		return nil
	}
	for _, a := range found {
		fmt.Fprintf(out, "Instance ID %s%s\n", a.ID, unverifiedNote(a))
		if len(a.Dirs) > 0 {
			fmt.Fprintf(out, "  data dir:   %s (no longer holds this ID)\n", strings.Join(a.Dirs, ", "))
		}
		printNames(out, "containers: ", "", a.Containers, func(c emuruntime.Container) string { return c.Name })
		printNames(out, "networks:   ", "", a.Networks, func(n emuruntime.Network) string { return n.Name })
		printNames(out, "volumes:    ", " (their data is lost)", a.Volumes, func(v emuruntime.Volume) string { return v.Name })
	}
	if !yes {
		fmt.Fprint(out, "Remove all of the above? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("aborted: nothing removed (re-run with --yes to remove without asking)")
		}
	}
	var errs []error
	for _, a := range found {
		if err := prune(a); err != nil {
			errs = append(errs, fmt.Errorf("instance ID %s: %w", a.ID, err))
			continue
		}
		fmt.Fprintf(out, "removed Instance ID %s\n", a.ID)
	}
	return errors.Join(errs...)
}

// unverifiedNote explains an unverified Abandoned Instance ID.
func unverifiedNote(a instance.Abandoned) string {
	if !a.Unverified {
		return ""
	}
	return " (unverified: its objects predate the data dir label, and no data dir under " + config.StateHome() +
		" holds it; an instance started with --data-dir elsewhere may still own it)"
}

// printNames prints the names of objs on one line, if there are any.
func printNames[T any](out io.Writer, label, suffix string, objs []T, name func(T) string) {
	if len(objs) == 0 {
		return
	}
	names := make([]string, len(objs))
	for i, o := range objs {
		names[i] = name(o)
	}
	fmt.Fprintf(out, "  %s%s%s\n", label, strings.Join(names, ", "), suffix)
}
