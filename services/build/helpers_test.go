package build_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

const testProject = "build-proj"

// busybox is the image the test Builds' steps run.
const busybox = "busybox:1.37"

// env is a started emulator with a Cloud Build gRPC client.
type env struct {
	t    *testing.T
	inst *emutest.Instance
	cb   *cloudbuild.Client
	ctx  context.Context
}

func start(t *testing.T, opts ...emutest.Option) *env {
	t.Helper()
	return startWith(t, nil, opts...)
}

// startWith is start with the Services svcs running too.
func startWith(t *testing.T, svcs []string, opts ...emutest.Option) *env {
	t.Helper()
	emutest.RequireRuntime(t)
	inst := emutest.Start(t, append([]string{"build", "gcs"}, svcs...), opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	cb, err := cloudbuild.NewClient(ctx,
		option.WithEndpoint(inst.Endpoint("gateway")),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cb.Close() })
	return &env{t: t, inst: inst, cb: cb, ctx: ctx}
}

// withConcurrency sets build.concurrency.
func withConcurrency(n int) emutest.Option {
	return func(c *config.Config) { c.Build.Concurrency = n }
}

// step is a busybox step running a shell command.
func step(id, script string, waitFor ...string) *cloudbuildpb.BuildStep {
	return &cloudbuildpb.BuildStep{Name: busybox, Id: id, Entrypoint: "sh", Args: []string{"-c", script}, WaitFor: waitFor}
}

// create starts a Build of steps in the global location.
func (e *env) create(b *cloudbuildpb.Build) (*cloudbuild.CreateBuildOperation, *cloudbuildpb.Build) {
	e.t.Helper()
	op, err := e.cb.CreateBuild(e.ctx, &cloudbuildpb.CreateBuildRequest{ProjectId: testProject, Build: b})
	if err != nil {
		e.t.Fatalf("CreateBuild: %v", err)
	}
	md, err := op.Metadata()
	if err != nil || md.GetBuild().GetId() == "" {
		e.t.Fatalf("metadata = %v, %v", md, err)
	}
	return op, md.GetBuild()
}

func (e *env) get(id string) *cloudbuildpb.Build {
	e.t.Helper()
	b, err := e.cb.GetBuild(e.ctx, &cloudbuildpb.GetBuildRequest{ProjectId: testProject, Id: id})
	if err != nil {
		e.t.Fatalf("GetBuild %s: %v", id, err)
	}
	return b
}

// await polls until the Build has a status in want, and returns it.
func (e *env) await(id string, want ...cloudbuildpb.Build_Status) *cloudbuildpb.Build {
	e.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		b := e.get(id)
		for _, w := range want {
			if b.GetStatus() == w {
				return b
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("Build %s is %v, want one of %v", id, b.GetStatus(), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitStep polls until step i of the Build is WORKING.
func (e *env) awaitStep(id string, i int) {
	e.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for e.get(id).GetSteps()[i].GetStatus() != cloudbuildpb.Build_WORKING {
		if time.Now().After(deadline) {
			e.t.Fatalf("step %d of Build %s never ran", i, id)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// buildContainers returns the Instance's containers and volumes that
// belong to Builds.
func (e *env) buildObjects() (containers, volumes []string) {
	e.t.Helper()
	rt, err := e.inst.Env.Containers.Runtime(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	sel := map[string]string{runtime.LabelInstance: rt.InstanceID, runtime.LabelService: "build"}
	cs, err := rt.ListContainers(e.ctx, sel)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range cs {
		containers = append(containers, c.Name)
	}
	vs, err := rt.ListVolumes(e.ctx, sel)
	if err != nil {
		e.t.Fatal(err)
	}
	return containers, vs
}

// noBuildObjects fails unless no container or volume of a Build is left.
func (e *env) noBuildObjects() {
	e.t.Helper()
	if cs, vs := e.buildObjects(); len(cs)+len(vs) > 0 {
		e.t.Errorf("left behind: containers %v, volumes %v", cs, vs)
	}
}

// rest performs a REST call against the gateway and decodes the JSON reply.
func (e *env) rest(method, path string, body, out any) int {
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
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return resp.StatusCode
}

// log returns a Build's log.
func (e *env) log(b *cloudbuildpb.Build) string {
	e.t.Helper()
	resp, err := http.Get(e.inst.GatewayURL() + "/_emu/v1/build/" + b.GetName() + "/log")
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("log of %s: %d %s", b.GetName(), resp.StatusCode, out)
	}
	return strings.TrimSpace(string(out))
}

// object reads a Cloud Storage object through the gateway, as gcloud
// storage cat does, from byte from on, returning the status and body.
func (e *env) object(bucket, name string, from int) (int, string) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.ctx, http.MethodGet, e.inst.GatewayURL()+"/"+bucket+"/"+name, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if from > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// storedLog reads a Build's log object from its logsBucket.
func (e *env) storedLog(b *cloudbuildpb.Build) string {
	e.t.Helper()
	bucket, prefix, _ := strings.Cut(strings.TrimPrefix(b.GetLogsBucket(), "gs://"), "/")
	name := "log-" + b.GetId() + ".txt"
	if prefix != "" {
		name = prefix + "/" + name
	}
	code, body := e.object(bucket, name, 0)
	if code != http.StatusOK {
		e.t.Fatalf("log object %s/%s: %d %s", bucket, name, code, body)
	}
	return body
}
