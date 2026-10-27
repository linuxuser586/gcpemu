//go:build conformance

package conformance

import (
	"context"
	"errors"
	"slices"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	iam "google.golang.org/api/iam/v1"
)

func TestIAMServiceAccounts(t *testing.T)  { iamServiceAccounts.test(t) }
func TestIAMResourcePolicies(t *testing.T) { iamResourcePolicies.test(t) }

// iamService is an IAM client for c.
func iamService(t testing.TB, c *conn) *iam.Service {
	t.Helper()
	s, err := iam.NewService(c.ctx, c.HTTP("iam")...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// createServiceAccount creates the service account base, deleted when the
// test ends, and waits until it can be read: IAM is eventually
// consistent.
func createServiceAccount(t testing.TB, c *conn, s *iam.Service, base string) *iam.ServiceAccount {
	t.Helper()
	sa, err := s.Projects.ServiceAccounts.Create("projects/"+c.Project(), &iam.CreateServiceAccountRequest{
		AccountId:      c.Name(base),
		ServiceAccount: &iam.ServiceAccount{DisplayName: "gcpemu Conformance suite", Description: "made by the gcpemu Conformance suite"},
	}).Context(c.ctx).Do()
	if err != nil {
		t.Fatalf("create service account: %v", err)
	}
	c.Cleanup(func(ctx context.Context) { _, _ = s.Projects.ServiceAccounts.Delete(sa.Name).Context(ctx).Do() })
	c.Retry(func() error {
		_, err := s.Projects.ServiceAccounts.Get(sa.Name).Context(c.ctx).Do()
		return err
	})
	return sa
}

var iamServiceAccounts = scenario{
	product: "iam",
	opts:    []Option{RunOnly("accounts")},
	body: func(t testing.TB, c *conn) {
		s := iamService(t, c)
		sa := createServiceAccount(t, c, s, "sa")
		c.Retry(func() error {
			as, err := s.Projects.ServiceAccounts.List("projects/" + c.Project()).PageSize(100).Context(c.ctx).Do()
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(as.Accounts, func(x *iam.ServiceAccount) bool { return x.Name == sa.Name }) {
				return errors.New("the new service account is not listed")
			}
			return nil
		})
		if _, err := s.Projects.ServiceAccounts.Patch(sa.Name, &iam.PatchServiceAccountRequest{
			ServiceAccount: &iam.ServiceAccount{DisplayName: "patched"},
			UpdateMask:     "displayName",
		}).Context(c.ctx).Do(); err != nil {
			t.Fatalf("patch: %v", err)
		}
		missing := "projects/" + c.Project() + "/serviceAccounts/" + c.Name("missing") + "@" + c.Project() + ".iam.gserviceaccount.com"
		if _, err := s.Projects.ServiceAccounts.Get(missing).Context(c.ctx).Do(); err == nil {
			t.Error("get of a missing service account succeeded")
		}
		if _, err := s.Projects.ServiceAccounts.Create("projects/"+c.Project(), &iam.CreateServiceAccountRequest{AccountId: c.Name("sa")}).Context(c.ctx).Do(); err == nil {
			t.Error("creating a service account whose name is taken succeeded")
		}
		if _, err := s.Projects.ServiceAccounts.Delete(sa.Name).Context(c.ctx).Do(); err != nil {
			t.Fatalf("delete: %v", err)
		}
	},
}

var iamResourcePolicies = scenario{
	product: "iam",
	opts:    []Option{Unordered("bindings", "role"), Unordered("bindings[].members", "")},
	body: func(t testing.TB, c *conn) {
		sa := createServiceAccount(t, c, iamService(t, c), "policy")
		member := "serviceAccount:" + sa.Email

		bucket := createBucket(t, c, storageClient(t, c), "policy")
		h := bucket.IAM()
		p, err := h.Policy(c.ctx)
		if err != nil {
			t.Fatalf("get bucket policy: %v", err)
		}
		p.Add(member, "roles/storage.objectViewer")
		// A new service account may not be usable in a policy at once.
		c.Retry(func() error { return h.SetPolicy(c.ctx, p) })
		if _, err := h.Policy(c.ctx); err != nil {
			t.Fatalf("get bucket policy again: %v", err)
		}

		topics := topicAdmin(t, c)
		topic := createTopic(t, c, topics, "policy")
		tp, err := topics.GetIamPolicy(c.ctx, &iampb.GetIamPolicyRequest{Resource: topic.Name})
		if err != nil {
			t.Fatalf("get topic policy: %v", err)
		}
		tp.Bindings = append(tp.Bindings, &iampb.Binding{Role: "roles/pubsub.subscriber", Members: []string{member}})
		c.Retry(func() error {
			_, err := topics.SetIamPolicy(c.ctx, &iampb.SetIamPolicyRequest{Resource: topic.Name, Policy: tp})
			return err
		})
		if _, err := topics.GetIamPolicy(c.ctx, &iampb.GetIamPolicyRequest{Resource: topic.Name}); err != nil {
			t.Fatalf("get topic policy again: %v", err)
		}
	},
}
