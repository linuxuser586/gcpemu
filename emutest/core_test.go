package emutest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
)

// TestGRPCReflection is Section 7.2: the gateway's gRPC server has server
// reflection enabled.
func TestGRPCReflection(t *testing.T) {
	inst := emutest.Start(t, []string{"pubsub"})
	conn, err := grpc.NewClient("passthrough:///"+inst.Endpoint("gateway"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&reflectionpb.ServerReflectionRequest{MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{}}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range resp.GetListServicesResponse().GetService() {
		names = append(names, s.Name)
	}
	for _, want := range []string{"google.pubsub.v1.Publisher", "google.pubsub.v1.Subscriber"} {
		if !slices.Contains(names, want) {
			t.Errorf("reflection lists %v, missing %s", names, want)
		}
	}
	// The file descriptor resolves too.
	if err := stream.Send(&reflectionpb.ServerReflectionRequest{MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: "google.pubsub.v1.Publisher"}}); err != nil {
		t.Fatal(err)
	}
	if r, err := stream.Recv(); err != nil || len(r.GetFileDescriptorResponse().GetFileDescriptorProto()) == 0 {
		t.Errorf("FileContainingSymbol: %v %v", r, err)
	}
}

func call(t *testing.T, method, url, body string) []byte {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s %s: %d %s", method, url, resp.StatusCode, b)
	}
	return b
}

// TestDataDirPermissions is NFR-SEC-003: keys, tokens, the CA, the state
// store and stored data are readable by the owner only.
func TestDataDirPermissions(t *testing.T) {
	dir := t.TempDir()
	inst := emutest.Start(t, []string{"iam", "gcs", "pubsub"}, func(c *config.Config) { c.Ephemeral, c.DataDir = false, dir })
	gw := inst.GatewayURL()
	call(t, "POST", gw+"/iam/v1/projects/perm-proj/serviceAccounts", `{"accountId":"perm-sa"}`)
	call(t, "POST", gw+"/iam/v1/projects/perm-proj/serviceAccounts/perm-sa@perm-proj.iam.gserviceaccount.com/keys", `{}`)
	gcs := "http://" + inst.Endpoint("gcs")
	call(t, "POST", gcs+"/storage/v1/b?project=perm-proj", `{"name":"perm-bucket"}`)
	call(t, "POST", gcs+"/upload/storage/v1/b/perm-bucket/o?uploadType=media&name=secret.txt", "secret")
	call(t, "PUT", gw+"/pubsub/v1/projects/perm-proj/topics/topic1", `{}`)
	call(t, "POST", gw+"/pubsub/v1/projects/perm-proj/topics/topic1:publish", `{"messages":[{"data":"c2VjcmV0"}]}`)

	var files int
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		files++
		rel, _ := filepath.Rel(dir, p)
		// The CA certificate (not its key) is public by nature.
		if rel == "ca.pem" || rel == "ca-bundle.pem" {
			return nil
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v, want no group/other access", rel, fi.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 4 {
		t.Fatalf("only %d files in %s", files, dir)
	}
}

// TestDeterministic is FR-CORE-022 / NFR-REL-002: with --deterministic,
// IDs, operation names and timestamps are the same run to run.
func TestDeterministic(t *testing.T) {
	run := func() []byte {
		inst := emutest.Start(t, []string{"compute", "pubsub"}, emutest.WithDeterministic())
		gw := inst.GatewayURL()
		var out bytes.Buffer
		op := call(t, "POST", gw+"/compute/v1/projects/det-proj/global/networks", `{"name":"net","autoCreateSubnetworks":false}`)
		out.Write(op)
		var o struct{ Name string }
		_ = json.Unmarshal(op, &o)
		out.Write(call(t, "POST", gw+"/compute/v1/projects/det-proj/global/operations/"+o.Name+"/wait", ""))
		out.Write(call(t, "GET", gw+"/compute/v1/projects/det-proj/global/networks/net", ""))
		call(t, "PUT", gw+"/pubsub/v1/projects/det-proj/topics/topic1", `{}`)
		out.Write(call(t, "POST", gw+"/pubsub/v1/projects/det-proj/topics/topic1:publish", `{"messages":[{"data":"eA=="}]}`))
		return out.Bytes()
	}
	a, b := run(), run()
	if !bytes.Equal(a, b) {
		t.Fatalf("runs differ:\n%s\n---\n%s", a, b)
	}
	if !bytes.Contains(a, []byte(`"id"`)) || !bytes.Contains(a, []byte("creationTimestamp")) {
		t.Fatalf("unexpected output:\n%s", a)
	}
}
