package runtime

import "testing"

func TestSecondToLast(t *testing.T) {
	for in, want := range map[string]string{
		"10.90.0.0/24":  "10.90.0.254",
		"172.24.0.0/16": "172.24.255.254",
		"10.0.0.8/29":   "10.0.0.14",
		"10.0.0.0/30":   "",
		"fd00::/64":     "",
		"not-a-network": "",
	} {
		if got := secondToLast(in); got != want {
			t.Errorf("secondToLast(%s) = %q, want %q", in, got, want)
		}
	}
}
