package iam

import (
	"testing"

	"github.com/linuxuser586/gcpemu/internal/project"
)

func TestGKEWorkloadMember(t *testing.T) {
	num := project.NumberString("p1")
	base := "://iam.googleapis.com/projects/" + num + "/locations/global/workloadIdentityPools/p1.svc.id.goog/"
	for _, c := range []struct {
		m             string
		pool, ns, ksa string
		ok            bool
	}{
		{"principal" + base + "subject/ns/app/sa/web", "p1.svc.id.goog", "app", "web", true},
		{"principalSet" + base + "namespace/app", "p1.svc.id.goog", "app", "", true},
		{"principal://iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p1.svc.id.goog/subject/ns/app/sa/web", "", "", "", false},
		{"principal://iam.googleapis.com/projects/" + num + "/locations/global/workloadIdentityPools/pool/subject/x", "", "", "", false},
		{"principalSet" + base + "subject/ns/app/sa/web", "", "", "", false},
	} {
		pool, ns, ksa, ok := gkeWorkloadMember(c.m)
		if pool != c.pool || ns != c.ns || ksa != c.ksa || ok != c.ok {
			t.Errorf("%s: %q %q %q %v", c.m, pool, ns, ksa, ok)
		}
	}
}
