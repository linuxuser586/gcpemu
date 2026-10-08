package transcode

import (
	"strings"
	"testing"
)

func TestPathTemplate(t *testing.T) {
	cases := []struct {
		tmpl, path string
		ok         bool
		want       string // "field=value;..."
	}{
		{"/v1/{parent=projects/*/locations/*}/repositories", "/v1/projects/p/locations/us/repositories", true, "parent=projects/p/locations/us"},
		{"/v1/{name=projects/*/locations/*/repositories/*}", "/v1/projects/p/locations/us/repositories/r", true, "name=projects/p/locations/us/repositories/r"},
		{"/v1/{name=projects/*/locations/*/repositories/*}", "/v1/projects/p/locations/us/repositories/r/x", false, ""},
		{"/v1/{resource=projects/*/locations/*/repositories/*}:getIamPolicy", "/v1/projects/p/locations/l/repositories/r:getIamPolicy", true, "resource=projects/p/locations/l/repositories/r"},
		{"/v1/{name=projects/*/locations/*/repositories/*/dockerImages/*}", "/v1/projects/p/locations/l/repositories/r/dockerImages/a%252Fb@sha256:00", true, "name=projects/p/locations/l/repositories/r/dockerImages/a%2Fb@sha256:00"},
		{"/v1/{name=projects/*/locations/*/repositories/*/packages/*}", "/v1/projects/p/locations/l/repositories/r/packages/a%2Fb", true, "name=projects/p/locations/l/repositories/r/packages/a%2Fb"},
		{"/v1/{name=projects/*/locations/*/repositories/*/files/**}", "/v1/projects/p/locations/l/repositories/r/files/a/b/c", true, "name=projects/p/locations/l/repositories/r/files/a/b/c"},
		{"/v1/{repository.name=projects/*/locations/*/repositories/*}", "/v1/projects/p/locations/l/repositories/r", true, "repository.name=projects/p/locations/l/repositories/r"},
	}
	for _, c := range cases {
		tp, err := parseTemplate(c.tmpl)
		if err != nil {
			t.Fatal(err)
		}
		vars, ok := tp.match(c.path)
		if ok != c.ok {
			t.Errorf("%s ~ %s: ok = %v", c.tmpl, c.path, ok)
			continue
		}
		var got []string
		for _, v := range vars {
			got = append(got, v[0]+"="+v[1])
		}
		if ok && strings.Join(got, ";") != c.want {
			t.Errorf("%s ~ %s: vars = %v, want %s", c.tmpl, c.path, got, c.want)
		}
	}
}
