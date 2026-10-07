package iam

import "testing"

func TestIsServiceAgent(t *testing.T) {
	for email, want := range map[string]bool{
		"service-123@gs-project-accounts.iam.gserviceaccount.com":   true,
		"service-123@gcp-sa-pubsub.iam.gserviceaccount.com":         true,
		"service-42@container-engine-robot.iam.gserviceaccount.com": true,
		"app@proj.iam.gserviceaccount.com":                          false,
		"service-x@proj.iam.gserviceaccount.com":                    false,
		"service-@proj.iam.gserviceaccount.com":                     false,
		"123-compute@developer.gserviceaccount.com":                 false,
	} {
		if got := isServiceAgent(email); got != want {
			t.Errorf("isServiceAgent(%q) = %v, want %v", email, got, want)
		}
	}
}
