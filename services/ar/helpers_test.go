package ar_test

import (
	"context"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/name"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/linuxuser586/gcpemu/emutest"
)

const (
	testProject = "test-proj"
	testLoc     = "us-central1"
)

// env is a started emulator with an AR gRPC client.
type env struct {
	t    *testing.T
	inst *emutest.Instance
	c    *artifactregistry.Client
	ctx  context.Context
}

func start(t *testing.T, opts ...emutest.Option) *env {
	t.Helper()
	inst := emutest.Start(t, []string{"ar"}, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	c, err := artifactregistry.NewClient(ctx,
		option.WithEndpoint(inst.Endpoint("gateway")),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &env{t: t, inst: inst, c: c, ctx: ctx}
}

// createRepo creates a DOCKER repository and waits for the operation.
func (e *env) createRepo(loc, id string) *artifactregistrypb.Repository {
	e.t.Helper()
	op, err := e.c.CreateRepository(e.ctx, &artifactregistrypb.CreateRepositoryRequest{
		Parent:       "projects/" + testProject + "/locations/" + loc,
		RepositoryId: id,
		Repository:   &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER, Description: "test"},
	})
	if err != nil {
		e.t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := op.Wait(e.ctx)
	if err != nil {
		e.t.Fatalf("CreateRepository wait: %v", err)
	}
	return repo
}

// registry returns the registry host:port.
func (e *env) registry() string { return e.inst.Endpoint("ar") }

// ref parses an image reference on the emulator registry.
func (e *env) ref(s string) name.Reference {
	e.t.Helper()
	r, err := name.ParseReference(e.registry()+"/"+s, name.Insecure)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}
