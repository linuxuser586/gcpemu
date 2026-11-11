package main

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// runADR fails when two ADRs share a number, which ADRs drafted on
// parallel branches can (#482).
func runADR([]string) error {
	dups, err := adrDuplicates(os.DirFS("docs/adr"))
	if err != nil {
		return err
	}
	for _, d := range dups {
		fmt.Fprintln(os.Stderr, d)
	}
	if len(dups) > 0 {
		return errReported
	}
	return nil
}

// adrDuplicates returns a line for each number that more than one ADR in
// fsys (docs/adr) uses, naming them in order.
func adrDuplicates(fsys fs.FS) ([]string, error) {
	// fs.Glob sorts, so ADRs sharing a number are next to each other.
	names, err := fs.Glob(fsys, "[0-9][0-9][0-9][0-9]-*.md")
	if err != nil {
		return nil, err
	}
	var out []string
	for i := 0; i < len(names); {
		num, _, _ := strings.Cut(names[i], "-")
		j := i + 1
		for j < len(names) && strings.HasPrefix(names[j], num+"-") {
			j++
		}
		if run := names[i:j]; len(run) > 1 {
			out = append(out, fmt.Sprintf("docs/adr: %s is used by %s and %s",
				num, strings.Join(run[:len(run)-1], ", "), run[len(run)-1]))
		}
		i = j
	}
	return out, nil
}
