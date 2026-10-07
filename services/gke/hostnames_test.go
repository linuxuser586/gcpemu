package gke_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestRealHostnames is FR-INT-007 end to end: a pod running unmodified Go
// client libraries (testdata/wiapp, a static binary in an image without CA
// certificates, pulled from emulated Artifact Registry) writes an object
// to storage.googleapis.com and publishes to pubsub.googleapis.com (gRPC)
// with Workload Identity credentials from the metadata server and no
// endpoint configuration. IAM is enforced, so the GSA's token must flow.
func TestRealHostnames(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	const proj = project // waitOp polls operations in project
	inst := emutest.Start(t, []string{"gke", "gcs", "pubsub"}, emutest.WithIAMMode("enforce"))
	cm := newClient(t, inst)
	ctx := context.Background()
	loc := "us-central1-a"
	par := "projects/" + proj + "/locations/" + loc

	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: par, Cluster: &containerpb.Cluster{
		Name:                   "hn",
		WorkloadIdentityConfig: &containerpb.WorkloadIdentityConfig{WorkloadPool: proj + ".svc.id.goog"},
		NodePools:              []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}

	// While the cluster comes up: image, repository, IAM and resources.
	image := buildWIApp(t, inst, proj)
	gsa := "wiapp-gsa@" + proj + ".iam.gserviceaccount.com"
	mustGW(t, inst, "POST", "/iam/v1/projects/"+proj+"/serviceAccounts", map[string]any{"accountId": "wiapp-gsa"}, nil)
	var p struct {
		ProjectNumber string `json:"projectNumber"`
	}
	mustGW(t, inst, "GET", "/cloudresourcemanager/v1/projects/"+proj, nil, &p)
	nodeSA := p.ProjectNumber + "-compute@developer.gserviceaccount.com"
	mustGW(t, inst, "POST", "/cloudresourcemanager/v1/projects/"+proj+":setIamPolicy", map[string]any{"policy": map[string]any{"bindings": []map[string]any{
		{"role": "roles/artifactregistry.reader", "members": []string{"serviceAccount:" + nodeSA}},
		{"role": "roles/storage.objectAdmin", "members": []string{"serviceAccount:" + gsa}},
		{"role": "roles/pubsub.publisher", "members": []string{"serviceAccount:" + gsa}},
	}}}, nil)
	mustGW(t, inst, "POST", "/iam/v1/projects/"+proj+"/serviceAccounts/"+gsa+":setIamPolicy", map[string]any{"policy": map[string]any{"bindings": []map[string]any{
		{"role": "roles/iam.workloadIdentityUser", "members": []string{"serviceAccount:" + proj + ".svc.id.goog[default/wiapp]"}},
	}}}, nil)
	mustGW(t, inst, "POST", "/storage/v1/b?project="+proj, map[string]any{"name": "hn-bucket"}, nil)
	mustGW(t, inst, "PUT", "/pubsub/v1/projects/"+proj+"/topics/hn-topic", map[string]any{}, nil)
	mustGW(t, inst, "PUT", "/pubsub/v1/projects/"+proj+"/subscriptions/hn-sub", map[string]any{"topic": "projects/" + proj + "/topics/hn-topic"}, nil)

	waitOp(t, cm, loc, op.Name, 3*time.Minute)
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: par + "/clusters/hn"})
	if err != nil {
		t.Fatal(err)
	}
	adm, pool := adminClient(t, inst, par+"/clusters/hn")
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}

	t.Run("NodeTrustsCA", func(t *testing.T) {
		out := nodeExec(t, inst, "cat", "/etc/ssl/certs/ca-certificates.crt")
		if !strings.Contains(out, strings.TrimSpace(string(inst.Env.CA.PEM()))) {
			t.Error("node system bundle lacks the emulator CA")
		}
	})

	t.Run("Injection", func(t *testing.T) {
		pod := func(ns string, labels map[string]string) map[string]any {
			return map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{"name": "dry", "namespace": ns, "labels": labels},
				"spec":     map[string]any{"containers": []map[string]any{{"name": "c", "image": testImage}}},
			}
		}
		injected := func(ns string, labels map[string]string) (bool, bool) {
			var out struct {
				Spec struct {
					Volumes    []struct{ Name string }
					Containers []struct {
						Env []struct{ Name, Value string }
					}
				}
			}
			if code, b := e.kube("POST", "/api/v1/namespaces/"+ns+"/pods?dryRun=All", "", pod(ns, labels), &out); code/100 != 2 {
				t.Logf("dry-run pod in %s: %d %s", ns, code, b) // e.g. default SA not created yet
				return false, false
			}
			vol := len(out.Spec.Volumes) > 0 && out.Spec.Volumes[len(out.Spec.Volumes)-1].Name == "gcpemu-ca-bundle"
			envOK := false
			for _, ev := range out.Spec.Containers[0].Env {
				envOK = envOK || (ev.Name == "SSL_CERT_FILE" && ev.Value == "/etc/gcpemu/certs/ca-certificates.crt")
			}
			if vol != envOK {
				t.Errorf("partial injection in %s: %+v", ns, out.Spec)
			}
			return vol, true
		}
		check := func(ns string, labels map[string]string) bool {
			var got, ok bool
			eventually(t, 30*time.Second, "dry-run pod in "+ns, func() bool { got, ok = injected(ns, labels); return ok })
			return got
		}
		e.mustKube("POST", "/api/v1/namespaces", map[string]any{"metadata": map[string]any{"name": "optout", "labels": map[string]string{"gcpemu.dev/inject": "disabled"}}}, nil)
		eventually(t, 30*time.Second, "webhook injecting into default", func() bool { got, _ := injected("default", nil); return got })
		if check("optout", nil) {
			t.Error("injected into a namespace labelled gcpemu.dev/inject=disabled")
		}
		if check("default", map[string]string{"gcpemu.dev/inject": "disabled"}) {
			t.Error("injected into a pod labelled gcpemu.dev/inject=disabled")
		}
		if check("kube-system", nil) {
			t.Error("injected into kube-system")
		}
	})

	t.Run("WorkloadReachesRealHostnames", func(t *testing.T) {
		e.mustKube("POST", "/api/v1/namespaces/default/serviceaccounts", map[string]any{
			"metadata": map[string]any{"name": "wiapp", "annotations": map[string]string{"iam.gke.io/gcp-service-account": gsa}},
		}, nil)
		e.mustKube("POST", "/api/v1/namespaces/default/pods", map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "wiapp", "namespace": "default"},
			"spec": map[string]any{
				"serviceAccountName": "wiapp",
				"restartPolicy":      "Never",
				"containers": []map[string]any{{
					"name": "app", "image": image,
					"env": []map[string]string{{"name": "PROJECT", "value": proj}, {"name": "BUCKET", "value": "hn-bucket"}, {"name": "TOPIC", "value": "hn-topic"}},
				}},
			},
		}, nil)
		var phase string
		deadline := time.Now().Add(3 * time.Minute)
		for phase != "Succeeded" && phase != "Failed" && time.Now().Before(deadline) {
			time.Sleep(time.Second)
			var pod struct{ Status struct{ Phase string } }
			e.mustKube("GET", "/api/v1/namespaces/default/pods/wiapp", nil, &pod)
			phase = pod.Status.Phase
		}
		_, logs := e.kube("GET", "/api/v1/namespaces/default/pods/wiapp/log?limitBytes=8192", "", nil, nil)
		if phase != "Succeeded" || !strings.Contains(logs, "WIAPP OK") {
			var pod map[string]any
			e.kube("GET", "/api/v1/namespaces/default/pods/wiapp", "", nil, &pod)
			t.Fatalf("pod phase %q\nlogs:\n%s\nstatus: %v", phase, logs, pod["status"])
		}
		// Verified through the emulator.
		resp, err := http.Get(inst.GatewayURL() + "/storage/v1/b/hn-bucket/o/from-pod.txt?alt=media")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "hello from a GKE pod" {
			t.Errorf("object = %d %q", resp.StatusCode, b)
		}
		var pulled struct {
			ReceivedMessages []struct {
				Message struct{ Data string }
			}
		}
		mustGW(t, inst, "POST", "/pubsub/v1/projects/"+proj+"/subscriptions/hn-sub:pull", map[string]any{"maxMessages": 10}, &pulled)
		if len(pulled.ReceivedMessages) != 1 || pulled.ReceivedMessages[0].Message.Data != base64.StdEncoding.EncodeToString([]byte("hello from a GKE pod")) {
			t.Errorf("pulled = %+v", pulled)
		}
		// The writes were made by the GSA (Workload Identity), not anonymously.
		var reqs struct {
			Requests []struct {
				Service, Method, Principal string
			}
		}
		mustGW(t, inst, "GET", "/_emu/v1/requests?service=pubsub", nil, &reqs)
		byGSA := false
		for _, r := range reqs.Requests {
			byGSA = byGSA || (strings.Contains(r.Method, "Publish") && r.Principal == "serviceAccount:"+gsa)
		}
		if !byGSA {
			t.Errorf("no Publish by %s in %+v", gsa, reqs.Requests)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		op, err := cm.DeleteCluster(ctx, &containerpb.DeleteClusterRequest{Name: par + "/clusters/hn"})
		if err != nil {
			t.Fatal(err)
		}
		waitOp(t, cm, loc, op.Name, 2*time.Minute)
	})
}

// mustGW is gw that fails the test on a non-2xx status.
func mustGW(t *testing.T, inst *emutest.Instance, method, path string, body, out any) {
	t.Helper()
	if code := gw(t, inst, method, path, body, out); code/100 != 2 {
		t.Fatalf("%s %s: %d", method, path, code)
	}
}

// buildWIApp compiles testdata/wiapp statically, wraps it as the only
// layer of an image without a base (no CA certificates), pushes it to
// emulated Artifact Registry and returns its LOCATION-docker.pkg.dev
// reference.
func buildWIApp(t *testing.T, inst *emutest.Instance, proj string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "app")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", bin, "./testdata/wiapp")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+goruntime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build wiapp: %v\n%s", err, out)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	_ = tw.WriteHeader(&tar.Header{Name: "app", Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(data)
	_ = tw.Close()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(tb.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", goruntime.GOARCH
	cf.Config = v1.Config{Entrypoint: []string{"/app"}}
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}

	mustGW(t, inst, "POST", "/artifactregistry/v1/projects/"+proj+"/locations/us-central1/repositories?repositoryId=apps",
		map[string]any{"format": "DOCKER"}, nil)
	keysSvc, _ := inst.Env.Lookup("iam")
	tok, _, err := keysSvc.(emu.ServiceAccountKeys).AccessToken(context.Background(), emu.Principal(inst.Config.DefaultPrincipal))
	if err != nil {
		t.Fatal(err)
	}
	image := "us-central1-docker.pkg.dev/" + proj + "/apps/wiapp:v1"
	ref, err := name.ParseReference(inst.Endpoint("ar")+"/"+image, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img, remote.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: tok})); err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}
	return image
}

// nodeExec runs a command in the instance's (only) GKE node container.
func nodeExec(t *testing.T, inst *emutest.Instance, cmd ...string) string {
	t.Helper()
	ctx := context.Background()
	rt, err := inst.Env.Containers.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := rt.ListContainers(ctx, map[string]string{runtime.LabelInstance: rt.InstanceID, runtime.LabelService: "gke", runtime.LabelRole: "node"})
	if err != nil || len(cs) == 0 {
		t.Fatalf("node containers: %v %v", cs, err)
	}
	res, err := rt.Exec(ctx, cs[0].ID, cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(res.Stdout)
}
