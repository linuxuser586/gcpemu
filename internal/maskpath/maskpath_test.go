package maskpath_test

import (
	"testing"

	"github.com/linuxuser586/gcpemu/internal/maskpath"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want maskpath.Path
	}{
		{"labels", maskpath.Path{Field: "labels"}},
		{"versionAliases", maskpath.Path{Field: "versionAliases"}},
		{"labels.env", maskpath.Path{Field: "labels", Key: "env", HasKey: true}},
		{"labels.myKey", maskpath.Path{Field: "labels", Key: "myKey", HasKey: true}},
		{"labels.my_key", maskpath.Path{Field: "labels", Key: "my_key", HasKey: true}},
		{"labels.`my-key`", maskpath.Path{Field: "labels", Key: "my-key", HasKey: true}},
		{"cleanupPolicies.`a.b.c`", maskpath.Path{Field: "cleanupPolicies", Key: "a.b.c", HasKey: true}},
	} {
		got, ok := maskpath.Parse(tc.in)
		if !ok || got != tc.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v, true", tc.in, got, ok, tc.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"", ".env", "labels.", "labels.``", "labels.`env", "labels.env`", "labels.`env`.x", "labels.env.x", "labels.`a`b`"} {
		if got, ok := maskpath.Parse(in); ok {
			t.Errorf("Parse(%q) = %+v, true; want false", in, got)
		}
	}
}

// A rejected path still reports its field segment, so a Service can say
// the field is unknown before it says the key is malformed.
func TestParseRejectedKeepsField(t *testing.T) {
	if got, _ := maskpath.Parse("rotation.x.y"); got.Field != "rotation" {
		t.Fatalf("Field = %q, want rotation", got.Field)
	}
}

func TestCase(t *testing.T) {
	for _, tc := range []struct{ camel, snake string }{
		{"labels", "labels"},
		{"versionAliases", "version_aliases"},
		{"customerManagedEncryption", "customer_managed_encryption"},
		{"expireTime", "expire_time"},
	} {
		if got := maskpath.SnakeCase(tc.camel); got != tc.snake {
			t.Errorf("SnakeCase(%q) = %q, want %q", tc.camel, got, tc.snake)
		}
		if got := maskpath.SnakeCase(tc.snake); got != tc.snake {
			t.Errorf("SnakeCase(%q) = %q, want it unchanged", tc.snake, got)
		}
		if got := maskpath.LowerCamel(tc.snake); got != tc.camel {
			t.Errorf("LowerCamel(%q) = %q, want %q", tc.snake, got, tc.camel)
		}
		if got := maskpath.LowerCamel(tc.camel); got != tc.camel {
			t.Errorf("LowerCamel(%q) = %q, want it unchanged", tc.camel, got)
		}
	}
}
