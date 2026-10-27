//go:build conformance

package conformance

import (
	"context"
	"testing"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func TestArtifactRegistryRepos(t *testing.T) { artifactRegistryRepos.test(t) }

// arLocation is where the suite's repositories are.
const arLocation = "us-central1"

var artifactRegistryRepos = scenario{
	product: "artifactregistry",
	opts: []Option{
		RunOnly("repositories"),
		Ignore("done", "an Operation may or may not have finished when it is first returned; the test gets the repository after waiting"),
		Ignore("response", "an Operation carries its result only once done; the test gets the repository after waiting"),
	},
	body: func(t testing.TB, c *conn) {
		cl, err := artifactregistry.NewClient(c.ctx, c.GRPC()...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cl.Close() })
		parent := "projects/" + c.Project() + "/locations/" + arLocation
		create := func() (*artifactregistrypb.Repository, error) {
			op, err := cl.CreateRepository(c.ctx, &artifactregistrypb.CreateRepositoryRequest{
				Parent:       parent,
				RepositoryId: c.Name("repo"),
				Repository: &artifactregistrypb.Repository{
					Format:      artifactregistrypb.Repository_DOCKER,
					Description: "gcpemu Conformance suite",
					Labels:      c.Labels(),
				},
			})
			if err != nil {
				return nil, err
			}
			return op.Wait(c.ctx)
		}
		repo, err := create()
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		c.Cleanup(func(ctx context.Context) {
			if op, err := cl.DeleteRepository(ctx, &artifactregistrypb.DeleteRepositoryRequest{Name: repo.Name}); err == nil {
				_ = op.Wait(ctx)
			}
		})
		if _, err := cl.GetRepository(c.ctx, &artifactregistrypb.GetRepositoryRequest{Name: repo.Name}); err != nil {
			t.Fatalf("get: %v", err)
		}
		c.Retry(func() error {
			it := cl.ListRepositories(c.ctx, &artifactregistrypb.ListRepositoriesRequest{Parent: parent, PageSize: 1000})
			return listed(it.Next, "the new repository", func(x *artifactregistrypb.Repository) bool { return x.Name == repo.Name })
		})
		if _, err := cl.UpdateRepository(c.ctx, &artifactregistrypb.UpdateRepositoryRequest{
			Repository: &artifactregistrypb.Repository{Name: repo.Name, Description: "updated"},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		}); err != nil {
			t.Fatalf("update: %v", err)
		}
		if _, err := cl.GetRepository(c.ctx, &artifactregistrypb.GetRepositoryRequest{Name: repo.Name + "-missing"}); err == nil {
			t.Error("get of a missing repository succeeded")
		}
		if _, err := create(); err == nil {
			t.Error("creating a repository whose name is taken succeeded")
		}
		op, err := cl.DeleteRepository(c.ctx, &artifactregistrypb.DeleteRepositoryRequest{Name: repo.Name})
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := op.Wait(c.ctx); err != nil {
			t.Fatalf("delete: %v", err)
		}
	},
}
