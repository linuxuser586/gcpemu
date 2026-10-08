package ar

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNameParsing(t *testing.T) {
	r, img, d, err := parseDockerImageName("projects/p/locations/us/repositories/r/dockerImages/a%2Fb@sha256:" + strings.Repeat("a", 64))
	if err != nil || r != (repoRef{"p", "us", "r"}) || img != "a/b" || !strings.HasPrefix(d, "sha256:") {
		t.Fatalf("docker image = %v %q %q %v", r, img, d, err)
	}
	if _, _, _, err := parsePackageSub("projects/p/locations/us/repositories/r/packages/app/tags/v1", "tags"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"projects/p", "projects/p/locations/us/repositories/r/packages/A", "projects/p/locations/us/repositories/r/dockerImages/app"} {
		if _, _, err := parsePackageName(bad); err == nil {
			if _, _, _, err := parseDockerImageName(bad); err == nil {
				t.Errorf("%s parsed", bad)
			}
		}
	}
	if escapeImage("a/b/c") != "a%2Fb%2Fc" || packageName(repoRef{"p", "l", "r"}, "a/b") != "projects/p/locations/l/repositories/r/packages/a%2Fb" {
		t.Fatal("escape")
	}
	if !validImage("team/my-app_1.0") || validImage("Upper") || validImage("a//b") {
		t.Fatal("validImage")
	}
}

func TestPaginate(t *testing.T) {
	items := []string{"a", "b", "c", "d"}
	cases := []struct {
		q, want, next string
	}{
		{"", "a,b,c,d", ""},
		{"n=2", "a,b", "b"},
		{"n=2&last=b", "c,d", ""},
		{"last=c", "d", ""},
		{"n=0", "", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/v2/x/tags/list?"+c.q, nil)
		got, next, err := paginate(items, r)
		if err != nil || strings.Join(got, ",") != c.want || next != c.next {
			t.Errorf("%s: %v %q %v", c.q, got, next, err)
		}
	}
}

func TestAccepts(t *testing.T) {
	cases := []struct {
		accept []string
		mt     string
		want   bool
	}{
		{nil, mtOCIIndex, true},
		{[]string{"*/*"}, mtOCIIndex, true},
		{[]string{mtDockerManifest + ", " + mtDockerList}, mtOCIIndex, false},
		{[]string{mtDockerManifest, mtOCIIndex}, mtOCIIndex, true},
		{[]string{"application/json"}, mtDockerManifest, true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		for _, a := range c.accept {
			r.Header.Add("Accept", a)
		}
		if got := accepts(r, c.mt); got != c.want {
			t.Errorf("accept %v for %s = %v", c.accept, c.mt, got)
		}
	}
}

func TestHostLocation(t *testing.T) {
	if l, ok := hostLocation("us-central1-docker.pkg.dev:443"); !ok || l != "us-central1" {
		t.Fatal(l, ok)
	}
	if _, ok := hostLocation("localhost:5000"); ok {
		t.Fatal("localhost matched")
	}
}
