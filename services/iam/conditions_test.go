package iam

import (
	"testing"
	"time"

	iamv1 "google.golang.org/api/iam/v1"
)

func TestEvalCondition(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	obj := "//storage.googleapis.com/projects/_/buckets/b/objects/logs/a.txt"
	cases := []struct {
		expr string
		want bool
	}{
		{`request.time < timestamp("2027-01-01T00:00:00Z")`, true},
		{`request.time >= timestamp("2027-01-01T00:00:00Z")`, false},
		{`resource.name.startsWith("projects/_/buckets/b/objects/logs/")`, true},
		{`resource.name.startsWith("projects/_/buckets/other")`, false},
		{`resource.type == "storage.googleapis.com/Object"`, true},
		{`resource.service == "storage.googleapis.com"`, true},
		{`resource.type == "storage.googleapis.com/Bucket" || resource.name.endsWith(".txt")`, true},
		{`!(resource.name.contains("logs")) && true`, false},
		{`resource.name == 'projects/_/buckets/b/objects/logs/a.txt'`, true},
		{`request.time > 5`, false}, // parse error → deny
	}
	for _, tc := range cases {
		if got := evalCondition(&iamv1.Expr{Expression: tc.expr}, obj, now); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestValidateCondition(t *testing.T) {
	for _, expr := range []string{`request.foo == "x"`, `resource.name.startsWith(`, `"unterminated`, ``} {
		if err := validateCondition(&iamv1.Expr{Title: "t", Expression: expr}); err == nil {
			t.Errorf("%q: expected error", expr)
		}
	}
	if err := validateCondition(&iamv1.Expr{Expression: "true"}); err == nil {
		t.Error("missing title should fail")
	}
}

func TestResourceType(t *testing.T) {
	cases := map[string]string{
		"//storage.googleapis.com/projects/_/buckets/b":                           "storage.googleapis.com/Bucket",
		"//storage.googleapis.com/projects/_/buckets/b/objects/a/b/c":             "storage.googleapis.com/Object",
		"//pubsub.googleapis.com/projects/p/topics/t":                             "pubsub.googleapis.com/Topic",
		"//artifactregistry.googleapis.com/projects/p/locations/l/repositories/r": "artifactregistry.googleapis.com/Repository",
		"//cloudresourcemanager.googleapis.com/projects/p":                        "cloudresourcemanager.googleapis.com/Project",
	}
	for res, want := range cases {
		host, path := splitResource(res)
		if got := resourceType(host, path); got != want {
			t.Errorf("%s: got %s, want %s", res, got, want)
		}
	}
}

func TestMappingExpressions(t *testing.T) {
	env := condEnv{vars: map[string]any{"assertion": map[string]any{"sub": "abc", "repo": "o/r", "nested": map[string]any{"x": "y"}}}}
	for expr, want := range map[string]any{
		"assertion.sub":                                   "abc",
		`"repo:" + assertion.repo`:                        "repo:o/r",
		"assertion.nested.x":                              "y",
		`assertion.repo.startsWith("o/")`:                 true,
		`assertion.sub == "abc" && assertion.repo != "x"`: true,
	} {
		n, err := parseCond(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		got, err := n.eval(env)
		if err != nil || got != want {
			t.Errorf("%s = %v (%v), want %v", expr, got, err, want)
		}
	}
}
