package secrets_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/project"
)

const testProject = "test-proj"

var (
	parent = "projects/" + testProject
	// numbered is the parent as the API reports it, with the project number.
	numbered = "projects/" + project.NumberString(testProject)
)

// env is a started emulator with a Secret Manager gRPC client.
type env struct {
	t    *testing.T
	inst *emutest.Instance
	sm   *secretmanager.Client
	ctx  context.Context
}

func start(t *testing.T, svcs []string, opts ...emutest.Option) *env {
	t.Helper()
	inst := emutest.Start(t, append([]string{"secrets"}, svcs...), opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	sm, err := secretmanager.NewClient(ctx,
		option.WithEndpoint(inst.Endpoint("gateway")),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sm.Close() })
	return &env{t: t, inst: inst, sm: sm, ctx: ctx}
}

// rest performs a REST call against the gateway and decodes the JSON reply.
func (e *env) rest(method, path string, body, out any) int {
	e.t.Helper()
	return e.restHost(method, "", path, body, out)
}

// restHost is rest with a Host header, for host routing.
func (e *env) restHost(method, host, path string, body, out any) int {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(e.ctx, method, e.inst.GatewayURL()+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return resp.StatusCode
}

func (e *env) advance(d time.Duration) {
	e.t.Helper()
	if err := e.inst.AdvanceClock(e.ctx, d); err != nil {
		e.t.Fatal(err)
	}
}

// wantCode fails unless err carries code.
func wantCode(t *testing.T, what string, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("%s: got %v, want %s", what, err, code)
	}
}

// must returns v, panicking on err (which fails the test).
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
