package main

import (
	"slices"
	"testing"
	"testing/fstest"
)

func TestADRDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  []string
	}{
		{"none", nil, nil},
		{"unique", []string{"0001-a.md", "0002-b.md", "0010-c.md"}, nil},
		{
			"pair",
			[]string{"0001-a.md", "0002-b.md", "0002-c.md", "0003-d.md"},
			[]string{"docs/adr: 0002 is used by 0002-b.md and 0002-c.md"},
		},
		{
			"three, and a second clash",
			[]string{"0004-x.md", "0004-y.md", "0004-z.md", "0007-p.md", "0007-q.md"},
			[]string{
				"docs/adr: 0004 is used by 0004-x.md, 0004-y.md and 0004-z.md",
				"docs/adr: 0007 is used by 0007-p.md and 0007-q.md",
			},
		},
		{
			"only numbered markdown files count",
			[]string{"0005-a.md", "0005-notes.txt", "README.md", "005-short.md", "0005b-x.md"},
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for _, f := range tc.files {
				fsys[f] = &fstest.MapFile{}
			}
			got, err := adrDuplicates(fsys)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("adrDuplicates(%v) =\n%q\nwant\n%q", tc.files, got, tc.want)
			}
		})
	}
}
