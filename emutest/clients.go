package emutest

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// restAPIs maps each REST API name to the Service ID that serves it and
// the API gateway path its Go client library (google.golang.org/api/<name>)
// needs as its endpoint: the API's mount, plus what the library's default
// base path adds after the host.
var restAPIs = map[string]struct{ service, prefix string }{
	"storage":              {"gcs", "/storage/v1/"},
	"compute":              {"compute", "/compute/v1/"},
	"servicenetworking":    {"compute", "/servicenetworking/"},
	"dns":                  {"dns", "/dns/"},
	"sqladmin":             {"sql", "/sqladmin/"},
	"container":            {"gke", "/container/"},
	"connectgateway":       {"gke", "/connectgateway/"},
	"artifactregistry":     {"ar", "/artifactregistry/"},
	"secretmanager":        {"secrets", "/secretmanager/"},
	"certificatemanager":   {"certs", "/certificatemanager/"},
	"networksecurity":      {"certs", "/networksecurity/"},
	"pubsub":               {"pubsub", "/pubsub/"},
	"monitoring":           {"pubsub", "/monitoring/"},
	"cloudbuild":           {"build", "/cloudbuild/"},
	"iam":                  {"iam", "/iam/"},
	"iamcredentials":       {"iam", "/iamcredentials/"},
	"cloudresourcemanager": {"iam", "/cloudresourcemanager/"},
	"cloudbilling":         {"iam", "/cloudbilling/"},
	"serviceusage":         {"iam", "/serviceusage/"},
	"sts":                  {"iam", "/sts/"},
}

// GRPCOptions returns the client options that connect any gRPC client
// library (cloud.google.com/go/...) to the Instance: an Endpoint override
// for its API gateway, no authentication and an insecure transport.
func (i *Instance) GRPCOptions() []option.ClientOption {
	return []option.ClientOption{
		option.WithEndpoint(i.Endpoint("gateway")),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	}
}

// HTTPOptions returns the client options that connect the REST client
// library of the API named api (google.golang.org/api/<api>: "storage",
// "compute", "dns", "sqladmin", …) to the Instance: an Endpoint override
// for the API's path on the API gateway, and no authentication. It fails when the Instance does not
// serve api.
func (i *Instance) HTTPOptions(api string) ([]option.ClientOption, error) {
	a, ok := restAPIs[api]
	if !ok {
		return nil, fmt.Errorf("emutest: no REST API %q", api)
	}
	if err := i.requireService(api, a.service); err != nil {
		return nil, err
	}
	return []option.ClientOption{
		option.WithEndpoint(i.GatewayURL() + a.prefix),
		option.WithoutAuthentication(),
	}, nil
}

// requireService fails unless the Instance runs the Service serviceID,
// which serves what.
func (i *Instance) requireService(what, serviceID string) error {
	if slices.ContainsFunc(i.in.Services(), func(s emu.Service) bool { return s.Name() == serviceID }) {
		return nil
	}
	return fmt.Errorf("emutest: the Instance does not serve %s: start it with the %q service", what, serviceID)
}

// Storage returns a Cloud Storage client connected to the Instance, closed
// when the test ends. It reads through the JSON API, which the API
// gateway serves. The test fails if the Instance does not run gcs.
func (i *Instance) Storage(t testing.TB) *storage.Client {
	t.Helper()
	opts, err := i.HTTPOptions("storage")
	if err != nil {
		t.Fatal(err)
	}
	c, err := storage.NewClient(context.Background(), append(opts, storage.WithJSONReads())...)
	if err != nil {
		t.Fatalf("emutest: storage client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// PubSub returns a Pub/Sub client for project connected to the Instance,
// closed when the test ends. The test fails if the Instance does not run
// pubsub.
func (i *Instance) PubSub(t testing.TB, project string) *pubsub.Client {
	t.Helper()
	if err := i.requireService("pubsub", "pubsub"); err != nil {
		t.Fatal(err)
	}
	c, err := pubsub.NewClient(context.Background(), project, i.GRPCOptions()...)
	if err != nil {
		t.Fatalf("emutest: pubsub client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
