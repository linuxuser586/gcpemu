//go:build compat

// Command buildgo probes the Go Cloud Build client (#393): it creates a
// Build that uploads an artifact to gs://BUCKET/artifacts/, waits on the
// Operation and prints "build OK <status> <numArtifacts>
// <artifactManifest>", or "build ERR <error>". It dials GCPEMU_GATEWAY,
// which serves gRPC, without TLS.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	project, bucket := os.Args[1], os.Args[2]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	b, err := build(ctx, project, bucket)
	if err != nil {
		fmt.Println("build ERR", strings.Join(strings.Fields(err.Error()), " "))
		return
	}
	fmt.Println("build OK", b.GetStatus(), b.GetResults().GetNumArtifacts(), b.GetResults().GetArtifactManifest())
}

func build(ctx context.Context, project, bucket string) (*cloudbuildpb.Build, error) {
	c, err := cloudbuild.NewClient(ctx,
		option.WithEndpoint(os.Getenv("GCPEMU_GATEWAY")),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	op, err := c.CreateBuild(ctx, &cloudbuildpb.CreateBuildRequest{
		ProjectId: project,
		Build: &cloudbuildpb.Build{
			Steps: []*cloudbuildpb.BuildStep{{
				Name:       "busybox:1.37",
				Entrypoint: "sh",
				Args:       []string{"-c", "echo from go > out.txt"},
			}},
			Artifacts: &cloudbuildpb.Artifacts{Objects: &cloudbuildpb.Artifacts_ArtifactObjects{
				Location: "gs://" + bucket + "/artifacts/",
				Paths:    []string{"out.txt"},
			}},
		},
	})
	if err != nil {
		return nil, err
	}
	return op.Wait(ctx)
}
