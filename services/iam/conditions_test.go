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
		{`request.time > 5`, false}, // type error → deny
		{`resource.name.matches("^projects/_/buckets/b/objects/logs/[a-z]+\\.txt$")`, true},
		{`resource.name.matches("^projects/p/")`, false},
		{`resource.name.extract("/buckets/{name}/") == "b"`, true},
		{`resource.name.extract("/objects/{name}") == "logs/a.txt"`, true},
		{`resource.name.extract("/topics/{name}") == ""`, true},
		{`resource.type in ["storage.googleapis.com/Bucket", "storage.googleapis.com/Object"]`, true},
		{`resource.matchTag("123/env", "prod")`, false},
		{`!resource.hasTagKey("123/env")`, true},
		{`api.getAttribute("iam.googleapis.com/modifiedGrantsByRole", []).size() == 0`, false}, // size not allowed
		{`api.getAttribute("iam.googleapis.com/modifiedGrantsByRole", []) == []`, true},
	}
	for _, tc := range cases {
		if got := evalCondition(&iamv1.Expr{Expression: tc.expr}, obj, now); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestEvalConditionTimestamp(t *testing.T) {
	// Monday 2026-06-01 08:30:15.250 UTC (10:30 in Berlin).
	now := time.Date(2026, 6, 1, 8, 30, 15, 250e6, time.UTC)
	cases := []struct {
		expr string
		want bool
	}{
		{`request.time.getHours() == 8`, true},
		{`request.time.getHours("Europe/Berlin") == 10`, true},
		{`request.time.getMinutes() == 30 && request.time.getSeconds() == 15`, true},
		{`request.time.getMilliseconds() == 250`, true},
		{`request.time.getDayOfWeek() == 1`, true}, // 0 = Sunday
		{`request.time.getDayOfWeek() >= 1 && request.time.getDayOfWeek() <= 5`, true},
		{`request.time.getDayOfMonth() == 0 && request.time.getDate() == 1`, true},
		{`request.time.getDayOfYear() == 151`, true},
		{`request.time.getMonth() == 5 && request.time.getFullYear() == 2026`, true},
		{`request.time.getHours("America/Los_Angeles") >= 9`, false},
		{`request.time - duration("3600s") < timestamp("2026-06-01T08:00:00Z")`, true},
		{`request.time + duration("1h") > timestamp("2026-06-01T09:00:00Z")`, true},
		{`request.time < timestamp("not a time")`, false}, // evaluation error → deny
	}
	for _, tc := range cases {
		if got := evalCondition(&iamv1.Expr{Expression: tc.expr}, "//cloudresourcemanager.googleapis.com/projects/p", now); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestValidateCondition(t *testing.T) {
	for _, expr := range []string{
		`request.foo == "x"`, `resource.name.startsWith(`, `"unterminated`, ``,
		`resource.name`,                     // not boolean
		`resource.name.size() > 0`,          // size is not an IAM function
		`[1].exists(x, x == 1)`,             // macros are not allowed
		`string(request.time) == ""`,        // conversions are not allowed
		`resource.name.lowerAscii() == "x"`, // extensions are not allowed
		`resource.matchTag("k") == true`,    // wrong arity
	} {
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
	vars := map[string]any{"assertion": map[string]any{"sub": "abc", "repo": "o/r", "nested": map[string]any{"x": "y"}, "groups": []any{"admins"}}}
	for expr, want := range map[string]any{
		"assertion.sub":                                   "abc",
		`"repo:" + assertion.repo`:                        "repo:o/r",
		"assertion.nested.x":                              "y",
		`assertion.repo.startsWith("o/")`:                 true,
		`assertion.sub == "abc" && assertion.repo != "x"`: true,
		`"admins" in assertion.groups`:                    true,
		`assertion.repo.extract("{owner}/")`:              "o",
		`has(assertion.missing) ? "x" : assertion.sub`:    "abc",
	} {
		got, err := evalMapping(expr, vars)
		if err != nil || got != want {
			t.Errorf("%s = %v (%v), want %v", expr, got, err, want)
		}
	}
}
