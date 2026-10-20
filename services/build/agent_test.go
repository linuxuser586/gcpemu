package build_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/escapeguard"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/project"
	"github.com/linuxuser586/gcpemu/internal/vmrelay"
)

// cloudSDK is the Cloud SDK image the gcloud steps run.
const cloudSDK = "gcr.io/google.com/cloudsdktool/google-cloud-cli:589.0.0-alpine"

// metadataEmail is a busybox step script printing the service account the
// metadata server answers as.
const metadataEmail = `wget -q -O- --header Metadata-Flavor:Google http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/email`

// run creates a Build and waits for it to succeed, failing with its log
// otherwise.
func (e *env) run(b *cloudbuildpb.Build) *cloudbuildpb.Build {
	e.t.Helper()
	op, created := e.create(b)
	done, err := op.Wait(e.ctx)
	if err != nil {
		e.t.Fatalf("Build failed: %v\n%s", err, e.log(created))
	}
	return done
}

// A Build without serviceAccount runs as the Compute Engine default
// service account, which its steps see at the metadata server.
func TestDefaultServiceAccount(t *testing.T) {
	e := start(t)
	b := e.run(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("email", metadataEmail)}})
	email := project.NumberString(testProject) + "-compute@developer.gserviceaccount.com"
	if got, want := b.GetServiceAccount(), "projects/"+testProject+"/serviceAccounts/"+email; got != want {
		t.Errorf("serviceAccount = %q, want %q", got, want)
	}
	if log := e.log(b); !strings.Contains(log, `Step #0 - "email": `+email) {
		t.Errorf("log = %q", log)
	}
}

// Unmodified gcloud and Go client-library steps reach the Instance by
// real hostname with no Endpoint override, as the Build's service
// account, and no request of theirs leaves the machine: with the VM
// relay forced off (a case that runs only when the runtime is not a VM
// runtime) and forced on.
func TestClientsReachTheInstance(t *testing.T) {
	for _, relay := range []string{"0", "1"} {
		t.Run("relay="+relay, func(t *testing.T) {
			t.Setenv(vmrelay.Env, relay)
			e := start(t)
			if relay == "0" && emutest.VMRuntime(t, e.inst) {
				t.Skip("the VM relay is forced off on a VM runtime, so the host cannot bind the services network's gateway")
			}
			sa := e.serviceAccount("builder")
			bucket := "clients-" + relay
			e.upload(bucket, "hello.txt", []byte("hello from gcs"))
			guard := e.guard()
			b := e.run(&cloudbuildpb.Build{
				ServiceAccount: "projects/" + testProject + "/serviceAccounts/" + sa,
				Source:         e.probeSource(),
				// As in the client compatibility suite, gcloud does not
				// look for updates at dl.google.com.
				Options: &cloudbuildpb.BuildOptions{Env: append(guard, "CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=1")},
				Steps: []*cloudbuildpb.BuildStep{
					{Name: cloudSDK, Id: "gcloud", Entrypoint: "sh", Args: []string{"-c", noProxyAgent +
						"gcloud auth list --format='value(account)' && gcloud storage ls gs://" + bucket}},
					{Name: busybox, Id: "go", Entrypoint: "sh", Args: []string{"-c", noProxyAgent +
						"./gcsprobe " + bucket + " hello.txt"}},
				},
			})
			log := e.log(b)
			for _, want := range []string{
				`Step #0 - "gcloud": ` + sa,
				`Step #0 - "gcloud": gs://` + bucket + `/hello.txt`,
				`Step #1 - "go": account: ` + sa,
				`Step #1 - "go": object: hello from gcs`,
			} {
				if !strings.Contains(log, want) {
					t.Errorf("log lacks %q:\n%s", want, log)
				}
			}
			if hosts := e.escaped(); len(hosts) > 0 {
				t.Errorf("requests left the machine for %v", hosts)
			}
			if relay == "1" {
				emutest.RequireRelayStream(t, e.inst, "the Build agent to the gateway", "gateway")
			}
		})
	}
}

// Under enforce, a Build's serviceAccount needs actAs from the caller,
// and its steps act with that account's grants: one that lacks
// storage.objects.get is denied the object. The legacy Cloud Build
// service account exists in every project and reads it as a Cloud Build
// Service Account.
func TestServiceAccountUnderEnforce(t *testing.T) {
	e := start(t, emutest.WithIAMMode(config.IAMEnforce))
	e.ctx = e.as(e.inst.Config.DefaultPrincipal)
	sa := "projects/" + testProject + "/serviceAccounts/" + e.serviceAccount("noread")
	bucket := "enforced"
	e.upload(bucket, "hello.txt", []byte("hello from gcs"))
	src := e.probeSource()

	dev := "user:builder@example.com"
	e.grant("roles/cloudbuild.builds.editor", dev)
	_, err := e.cb.CreateBuild(e.as(dev), &cloudbuildpb.CreateBuildRequest{ProjectId: testProject, Build: &cloudbuildpb.Build{
		ServiceAccount: sa, Steps: []*cloudbuildpb.BuildStep{step("", "true")},
	}})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "iam.serviceAccounts.actAs") {
		t.Fatalf("CreateBuild without actAs: %v", err)
	}

	probe := &cloudbuildpb.BuildStep{Name: busybox, Id: "go", Entrypoint: "./gcsprobe", Args: []string{bucket, "hello.txt"}}
	op, created := e.create(&cloudbuildpb.Build{ServiceAccount: sa, Source: src, Steps: []*cloudbuildpb.BuildStep{probe}})
	if _, err := op.Wait(e.ctx); err == nil {
		t.Fatal("the probe read the object without storage.objects.get")
	}
	if log := e.log(created); !strings.Contains(log, "storage.objects.get") || !strings.Contains(log, "403") {
		t.Errorf("log = %s", log)
	}

	legacy := project.NumberString(testProject) + "@cloudbuild.gserviceaccount.com"
	b := e.run(&cloudbuildpb.Build{ServiceAccount: "projects/" + testProject + "/serviceAccounts/" + legacy, Source: src, Steps: []*cloudbuildpb.BuildStep{probe}})
	log := e.log(b)
	for _, want := range []string{"account: " + legacy, "object: hello from gcs"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

// as returns e's context calling as principal.
func (e *env) as(principal string) context.Context {
	e.t.Helper()
	svc, _ := e.inst.Env.Lookup("iam")
	tok, _, err := svc.(emu.ServiceAccountKeys).AccessToken(context.Background(), emu.Principal(principal))
	if err != nil {
		e.t.Fatal(err)
	}
	return metadata.NewOutgoingContext(e.ctx, metadata.Pairs("authorization", "Bearer "+tok))
}

// token returns the bearer token of e's context.
func (e *env) token() string {
	md, _ := metadata.FromOutgoingContext(e.ctx)
	if v := md.Get("authorization"); len(v) > 0 {
		return v[0]
	}
	return ""
}

// send performs a REST call as e's context's caller and fails unless it
// succeeds.
func (e *env) send(method, path, body string) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.inst.GatewayURL()+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := e.token(); tok != "" {
		req.Header.Set("Authorization", tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("%s %s: %d", method, path, resp.StatusCode)
	}
}

// serviceAccount creates the service account id in the test project and
// returns its email.
func (e *env) serviceAccount(id string) string {
	e.t.Helper()
	e.send(http.MethodPost, "/iam/v1/projects/"+testProject+"/serviceAccounts", `{"accountId":"`+id+`"}`)
	return id + "@" + testProject + ".iam.gserviceaccount.com"
}

// grant binds role to member on the test project, keeping the default
// principal its owner.
func (e *env) grant(role, member string) {
	e.t.Helper()
	e.send(http.MethodPost, "/cloudresourcemanager/v1/projects/"+testProject+":setIamPolicy", `{"policy":{"bindings":[
		{"role":"roles/owner","members":["`+e.inst.Config.DefaultPrincipal+`"]},
		{"role":"`+role+`","members":["`+member+`"]}]}}`)
}

// noProxyAgent exempts the Build agent from the Escape guard's proxy; $$
// escapes the shell's variables from substitution.
const noProxyAgent = `export NO_PROXY="$$NO_PROXY,$$GCE_METADATA_HOST" no_proxy="$$no_proxy,$$GCE_METADATA_HOST"; `

var guards sync.Map // *testing.T → *escapeguard.Guard

// guard starts an Escape guard that the Build's steps reach at the Host
// address of the Build network, and returns the variables that send their
// requests to it: every host but those the Instance serves.
func (e *env) guard() []string {
	e.t.Helper()
	e.run(&cloudbuildpb.Build{Steps: []*cloudbuildpb.BuildStep{step("", "true")}}) // creates the Build network
	g := escapeguard.New(e.t)
	guards.Store(e.t, g)
	np, err := e.inst.Env.Containers.Netplane(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	rt := np.Runtime()
	n, err := rt.InspectNetwork(e.ctx, rt.Name("build"))
	if err != nil {
		e.t.Fatal(err)
	}
	addr, err := np.HostAddress(e.ctx, netplane.Net{Name: n.Name, Subnet: n.Subnet, Gateway: n.Gateway})
	if err != nil {
		e.t.Fatal(err)
	}
	var port int
	var env []string
	for _, kv := range g.Env() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "NO_PROXY") {
			v += ",metadata.google.internal,metadata," + strings.Join(e.inst.FrontendHosts(), ",")
		} else {
			_, p, _ := net.SplitHostPort(strings.TrimPrefix(v, "http://"))
			port, _ = strconv.Atoi(p)
			v = "http://" + net.JoinHostPort(addr, p)
		}
		env = append(env, k+"="+v)
	}
	if err := np.Offer(e.ctx, addr, port, false); err != nil {
		e.t.Fatal(err)
	}
	return env
}

// escaped returns the hosts the Escape guard refused.
func (e *env) escaped() []string {
	g, _ := guards.Load(e.t)
	return g.(*escapeguard.Guard).Take()
}

var (
	probeOnce sync.Once
	probeBin  []byte
	probeErr  error
)

// probeSource is a storageSource holding gcsprobe built for the runtime.
func (e *env) probeSource() *cloudbuildpb.Source {
	e.t.Helper()
	probeOnce.Do(func() {
		rt, err := e.inst.Env.Containers.Runtime(e.ctx)
		if err != nil {
			probeErr = err
			return
		}
		out := filepath.Join(os.TempDir(), "gcpemu-gcsprobe-"+rt.Info.Arch)
		cmd := exec.Command("go", "build", "-o", out, "./testdata/gcsprobe")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+rt.Info.Arch)
		if b, err := cmd.CombinedOutput(); err != nil {
			probeErr = err
			e.t.Logf("%s", b)
			return
		}
		probeBin, probeErr = os.ReadFile(out)
	})
	if probeErr != nil {
		e.t.Fatalf("building gcsprobe: %v", probeErr)
	}
	bucket := testProject + "_cloudbuild"
	e.upload(bucket, "source/probe.tgz", tgzExec(e.t, "gcsprobe", probeBin))
	return &cloudbuildpb.Source{Source: &cloudbuildpb.Source_StorageSource{StorageSource: &cloudbuildpb.StorageSource{Bucket: bucket, Object: "source/probe.tgz"}}}
}

// tgzExec is a gzipped tarball of one executable file.
func tgzExec(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
