package iam

import (
	"slices"
	"testing"
)

func TestCatalogueLoads(t *testing.T) {
	c, err := loadCatalogue(rolesYAML)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.universe) < 300 {
		t.Errorf("universe has only %d permissions", len(c.universe))
	}
	for _, name := range []string{
		"roles/owner", "roles/editor", "roles/viewer", "roles/storage.admin", "roles/storage.objectAdmin",
		"roles/pubsub.publisher", "roles/dns.admin", "roles/artifactregistry.reader", "roles/cloudsql.client",
		"roles/container.developer", "roles/compute.networkAdmin", "roles/compute.loadBalancerAdmin",
		"roles/iam.serviceAccountUser", "roles/iam.workloadIdentityUser", "roles/iam.serviceAccountTokenCreator",
	} {
		r, ok := c.roles[name]
		if !ok {
			t.Errorf("missing role %s", name)
			continue
		}
		if len(r.Permissions) == 0 {
			t.Errorf("%s has no permissions", name)
		}
	}
}

func TestRoleHas(t *testing.T) {
	c := builtin()
	cases := []struct {
		role, perm string
		want       bool
	}{
		{"roles/owner", "storage.buckets.setIamPolicy", true},
		{"roles/owner", "unknown.thing.do", true},
		{"roles/editor", "storage.objects.create", true},
		{"roles/editor", "storage.buckets.setIamPolicy", false},
		{"roles/editor", "iam.serviceAccounts.getAccessToken", false},
		{"roles/editor", "iam.serviceAccounts.actAs", true},
		{"roles/viewer", "storage.buckets.list", true},
		{"roles/viewer", "storage.objects.create", false},
		{"roles/viewer", "iam.serviceAccounts.getAccessToken", false},
		{"roles/storage.objectAdmin", "storage.objects.delete", true},
		{"roles/storage.objectAdmin", "storage.buckets.delete", false},
		{"roles/storage.admin", "storage.newthing.get", true},
		{"roles/pubsub.editor", "pubsub.topics.setIamPolicy", false},
		{"roles/pubsub.editor", "pubsub.topics.create", true},
		{"roles/container.viewer", "container.pods.list", true},
		{"roles/container.viewer", "container.secrets.get", false},
		{"roles/iam.securityAdmin", "storage.buckets.setIamPolicy", true},
		{"roles/iam.serviceAccountTokenCreator", "iam.serviceAccounts.signBlob", true},
		{"roles/monitoring.viewer", "monitoring.metricDescriptors.list", true},
		{"roles/monitoring.viewer", "monitoring.alertPolicies.get", true},
		{"roles/monitoring.viewer", "monitoring.timeSeries.create", false},
		{"roles/monitoring.editor", "monitoring.dashboards.update", true},
		{"roles/monitoring.alertPolicyEditor", "monitoring.alertPolicies.delete", true},
		{"roles/monitoring.alertPolicyEditor", "monitoring.dashboards.delete", false},
		{"roles/monitoring.dashboardViewer", "monitoring.dashboards.update", false},
	}
	for _, tc := range cases {
		if got := c.roles[tc.role].has(tc.perm); got != tc.want {
			t.Errorf("%s has %s = %v, want %v", tc.role, tc.perm, got, tc.want)
		}
	}
}

// TestMetricWriterPermissions: the role a GKE node service account gets to
// write metrics grants exactly what GCP lists for it (#364).
func TestMetricWriterPermissions(t *testing.T) {
	want := []string{
		"monitoring.metricDescriptors.create", "monitoring.metricDescriptors.get", "monitoring.metricDescriptors.list",
		"monitoring.monitoredResourceDescriptors.get", "monitoring.monitoredResourceDescriptors.list",
		"monitoring.timeSeries.create",
	}
	if got := builtin().roles["roles/monitoring.metricWriter"].Permissions; !slices.Equal(got, want) {
		t.Errorf("metricWriter permissions = %v, want %v", got, want)
	}
}

// TestSmallestRole: the IAM audit view's advice (FR-UI-014) is the
// predefined role with the fewest permissions that grants one, preferring
// any to a basic or bucket-only legacy role.
func TestSmallestRole(t *testing.T) {
	c := builtin()
	for perm, want := range map[string]string{
		"pubsub.topics.publish":                 "roles/pubsub.publisher",
		"secretmanager.versions.access":         "roles/secretmanager.secretAccessor",
		"storage.objects.get":                   "roles/storage.objectViewer",
		"iam.serviceAccounts.actAs":             "roles/iam.serviceAccountUser",
		"resourcemanager.projects.getIamPolicy": "roles/browser",
		"resourcemanager.projects.delete":       "roles/owner",
		"unknown.thing.do":                      "roles/editor",
	} {
		if got := c.smallestRole(perm); got == nil || got.Name != want {
			t.Errorf("smallestRole(%s) = %v, want %s", perm, got, want)
		}
	}
}
