package listfilter_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/listfilter"
)

type res struct {
	name   string
	labels map[string]string
}

var testFields = map[string]listfilter.Field[res]{
	"name":          {Get: func(r res) string { return r.name }},
	"exact":         {Exact: true, Get: func(r res) string { return r.name }},
	"label.":        {Exact: true, Key: func(r res, k string) string { return r.labels[k] }},
	"label.special": {Get: func(res) string { return "special" }},
}

var testRes = []res{
	{name: "POSTGRES_17", labels: map[string]string{"env": "dev"}},
	{name: "POSTGRES_1", labels: map[string]string{"env": "prod", "special": "x"}},
	{name: "MYSQL_8_0"},
}

func invalid(format string, args ...any) error { return fmt.Errorf(format, args...) }

// matching compiles filter over testFields and returns the names of the
// testRes it matches.
func matching(t *testing.T, filter string) []string {
	t.Helper()
	m, err := listfilter.Compile(filter, testFields, invalid)
	if err != nil {
		t.Fatalf("Compile(%q): %v", filter, err)
	}
	var got []string
	for _, r := range testRes {
		if m(r) {
			got = append(got, r.name)
		}
	}
	return got
}

func TestCompile(t *testing.T) {
	for _, c := range []struct {
		filter string
		want   []string
	}{
		// : is contains by default ...
		{`name:POSTGRES_1`, []string{"POSTGRES_17", "POSTGRES_1"}},
		// ... and equality on an Exact field.
		{`exact:POSTGRES_1`, []string{"POSTGRES_1"}},
		{`exact:"POSTGRES_17"`, []string{"POSTGRES_17"}},
		{`exact=POSTGRES_1`, []string{"POSTGRES_1"}},
		{`exact!=POSTGRES_1`, []string{"POSTGRES_17", "MYSQL_8_0"}},
		// A prefix field takes the rest of the name as its key; a
		// resource without the key reads as empty.
		{`label.env=dev`, []string{"POSTGRES_17"}},
		{`label.env:dev`, []string{"POSTGRES_17"}},
		{`label.env:de`, nil},
		{`label.env!=dev`, []string{"POSTGRES_1", "MYSQL_8_0"}},
		{`NOT label.env=dev`, []string{"POSTGRES_1", "MYSQL_8_0"}},
		{`label.missing=dev`, nil},
		// An exact name is looked up before a prefix field.
		{`label.special=special`, []string{"POSTGRES_17", "POSTGRES_1", "MYSQL_8_0"}},
	} {
		if got := matching(t, c.filter); !slices.Equal(got, c.want) {
			t.Errorf("filter %q = %v, want %v", c.filter, got, c.want)
		}
	}
}

func TestCompileRejected(t *testing.T) {
	for _, c := range []struct{ filter, msg string }{
		{`label.=x`, `unknown field "label."`},
		{`labelx=x`, `unknown field "labelx"`},
		{`exact:*`, `does not support ":*"`},
		{`label.env:*`, `does not support ":*"`},
	} {
		_, err := listfilter.Compile(c.filter, testFields, invalid)
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("Compile(%q) = %v, want an error containing %q", c.filter, err, c.msg)
		}
	}
}
