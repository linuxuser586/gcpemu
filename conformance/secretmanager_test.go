//go:build conformance

package conformance

import (
	"context"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func TestSecretManagerSecrets(t *testing.T)  { secretManagerSecrets.test(t) }
func TestSecretManagerVersions(t *testing.T) { secretManagerVersions.test(t) }

// secretsClient is a Secret Manager client for c, closed when the test
// ends.
func secretsClient(t testing.TB, c *conn) *secretmanager.Client {
	t.Helper()
	cl, err := secretmanager.NewClient(c.ctx, c.GRPC()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

// createSecret creates the secret base, deleted when the test ends.
func createSecret(t testing.TB, c *conn, cl *secretmanager.Client, base string) *secretmanagerpb.Secret {
	t.Helper()
	s, err := cl.CreateSecret(c.ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   "projects/" + c.Project(),
		SecretId: c.Name(base),
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}},
			Labels:      c.Labels(),
		},
	})
	if err != nil {
		t.Fatalf("create secret: %v", err)
	}
	c.Cleanup(func(ctx context.Context) {
		_ = cl.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: s.Name})
	})
	return s
}

var secretManagerSecrets = scenario{
	product: "secretmanager",
	opts:    []Option{RunOnly("secrets")},
	body: func(t testing.TB, c *conn) {
		cl := secretsClient(t, c)
		s := createSecret(t, c, cl, "secret")
		if _, err := cl.GetSecret(c.ctx, &secretmanagerpb.GetSecretRequest{Name: s.Name}); err != nil {
			t.Fatalf("get: %v", err)
		}
		c.Retry(func() error {
			it := cl.ListSecrets(c.ctx, &secretmanagerpb.ListSecretsRequest{Parent: "projects/" + c.Project(), Filter: "name:" + c.Name("")})
			return listed(it.Next, "the new secret", func(x *secretmanagerpb.Secret) bool { return x.Name == s.Name })
		})
		labels := c.Labels()
		labels["color"] = "blue"
		if _, err := cl.UpdateSecret(c.ctx, &secretmanagerpb.UpdateSecretRequest{
			Secret:     &secretmanagerpb.Secret{Name: s.Name, Labels: labels},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
		}); err != nil {
			t.Fatalf("update: %v", err)
		}
		if _, err := cl.GetSecret(c.ctx, &secretmanagerpb.GetSecretRequest{Name: s.Name + "-missing"}); err == nil {
			t.Error("get of a missing secret succeeded")
		}
		if _, err := cl.CreateSecret(c.ctx, &secretmanagerpb.CreateSecretRequest{
			Parent: "projects/" + c.Project(), SecretId: c.Name("secret"),
			Secret: &secretmanagerpb.Secret{Replication: s.Replication},
		}); err == nil {
			t.Error("creating a secret whose name is taken succeeded")
		}
		if err := cl.DeleteSecret(c.ctx, &secretmanagerpb.DeleteSecretRequest{Name: s.Name}); err != nil {
			t.Fatalf("delete: %v", err)
		}
	},
}

var secretManagerVersions = scenario{
	product: "secretmanager",
	body: func(t testing.TB, c *conn) {
		cl := secretsClient(t, c)
		s := createSecret(t, c, cl, "versions")
		v, err := cl.AddSecretVersion(c.ctx, &secretmanagerpb.AddSecretVersionRequest{
			Parent:  s.Name,
			Payload: &secretmanagerpb.SecretPayload{Data: []byte("s3cret")},
		})
		if err != nil {
			t.Fatalf("add version: %v", err)
		}
		if _, err := cl.GetSecretVersion(c.ctx, &secretmanagerpb.GetSecretVersionRequest{Name: v.Name}); err != nil {
			t.Fatalf("get version: %v", err)
		}
		if _, err := cl.AccessSecretVersion(c.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: s.Name + "/versions/latest"}); err != nil {
			t.Fatalf("access: %v", err)
		}
		c.Retry(func() error {
			it := cl.ListSecretVersions(c.ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: s.Name})
			return listed(it.Next, "the new version", func(x *secretmanagerpb.SecretVersion) bool { return x.Name == v.Name })
		})
		if _, err := cl.DisableSecretVersion(c.ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: v.Name}); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if _, err := cl.AccessSecretVersion(c.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: v.Name}); err == nil {
			t.Error("accessing a disabled version succeeded")
		}
		if _, err := cl.GetSecretVersion(c.ctx, &secretmanagerpb.GetSecretVersionRequest{Name: s.Name + "/versions/99"}); err == nil {
			t.Error("get of a missing version succeeded")
		}
		if _, err := cl.DestroySecretVersion(c.ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: v.Name}); err != nil {
			t.Fatalf("destroy: %v", err)
		}
		if _, err := cl.AccessSecretVersion(c.ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: v.Name}); err == nil {
			t.Error("accessing a destroyed version succeeded")
		}
	},
}
