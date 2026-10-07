package iam

import "testing"

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
	}
	for _, tc := range cases {
		if got := c.roles[tc.role].has(tc.perm); got != tc.want {
			t.Errorf("%s has %s = %v, want %v", tc.role, tc.perm, got, tc.want)
		}
	}
}
