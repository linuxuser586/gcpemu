package pubsub

import (
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestFilterMatch(t *testing.T) {
	attrs := map[string]string{"type": "order", "region": "us-east1", "empty": "", "my key": "v"}
	tests := []struct {
		filter string
		want   bool
	}{
		{``, true},
		{`attributes.type = "order"`, true},
		{`attributes.type = "refund"`, false},
		{`attributes.type != "refund"`, true},
		{`attributes.missing != "x"`, true},
		{`attributes.type != "order"`, false},
		{`attributes:type`, true},
		{`attributes:missing`, false},
		{`attributes:empty`, true},
		{`NOT attributes:missing`, true},
		{`-attributes:type`, false},
		{`hasPrefix(attributes.region, "us-")`, true},
		{`hasPrefix(attributes.region, "eu-")`, false},
		{`hasPrefix(attributes.missing, "")`, false},
		{`attributes.type = "order" AND attributes.region = "us-east1"`, true},
		{`attributes.type = "order" AND attributes.region = "eu"`, false},
		{`attributes.type = "x" OR attributes.region = "us-east1"`, true},
		{`attributes.type = "x" OR attributes.region = "y"`, false},
		{`(attributes.type = "x" OR attributes:region) AND NOT attributes:missing`, true},
		{`NOT (attributes.type = "order" AND attributes:region)`, false},
		{`attributes."my key" = "v"`, true},
		{`attributes.type = 'order'`, true},
		{`attributes.type = "or\"der"`, false},
	}
	for _, tc := range tests {
		f, err := compileFilter(tc.filter)
		if err != nil {
			t.Errorf("compile %q: %v", tc.filter, err)
			continue
		}
		if got := filterMatches(f, attrs); got != tc.want {
			t.Errorf("%q = %v, want %v", tc.filter, got, tc.want)
		}
	}
}

func TestFilterErrors(t *testing.T) {
	bad := []string{
		`attributes.type = order`,
		`attributes.type == "x"`,
		`attributes.a = "1" AND attributes.b = "2" OR attributes.c = "3"`,
		`(attributes:a`,
		`attributes:a)`,
		`hasPrefix(attributes.a)`,
		`hasPrefix(data, "x")`,
		`attributes`,
		`attributes.a = "unterminated`,
		`foo.bar = "x"`,
		`attributes.a ! "x"`,
		`attributes.a:"x"`,
		string(make([]byte, 300)) + `attributes:a`,
	}
	for _, f := range bad {
		if _, err := compileFilter(f); err == nil {
			t.Errorf("compile %q: expected error", f)
		}
	}
}

func TestRetryBackoff(t *testing.T) {
	rp := &pubsubpb.Subscription{RetryPolicy: &pubsubpb.RetryPolicy{
		MinimumBackoff: durationpb.New(time.Second), MaximumBackoff: durationpb.New(5 * time.Second),
	}}
	for attempts, want := range map[int32]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 5 * time.Second, 10: 5 * time.Second} {
		if got := retryBackoff(rp, attempts, false); got != want {
			t.Errorf("attempt %d: backoff %v, want %v", attempts, got, want)
		}
	}
	if got := retryBackoff(&pubsubpb.Subscription{}, 3, false); got != 0 {
		t.Errorf("pull without policy: %v, want 0", got)
	}
	if got := retryBackoff(&pubsubpb.Subscription{}, 1, true); got != 100*time.Millisecond {
		t.Errorf("push without policy: %v", got)
	}
}
