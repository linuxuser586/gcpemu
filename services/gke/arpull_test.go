package gke_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// TestNodePullsAsNodeSA is FR-INT-006 / FR-AR-005: nodes pull Artifact
// Registry images as their node pool's service account, so in enforce mode
// a pull fails until that account may read the repository.
func TestNodePullsAsNodeSA(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	inst := emutest.Start(t, []string{"gke"}, emutest.WithIAMMode("enforce"))
	cm := newClient(t, inst)
	ctx := context.Background()

	const repo = "/artifactregistry/v1/projects/" + project + "/locations/us-central1/repositories"
	if code := gw(t, inst, "POST", repo+"?repositoryId=apps", map[string]any{"format": "DOCKER"}, nil); code != 200 {
		t.Fatalf("create repository: %d", code)
	}
	iam, _ := inst.Env.Lookup("iam")
	tok, _, err := iam.(emu.ServiceAccountKeys).AccessToken(ctx, emu.Principal(inst.Env.Config.DefaultPrincipal))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(inst.Endpoint("ar")+"/"+project+"/apps/app:v1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := random.Image(512, 1)
	if err := remote.Write(ref, img, remote.WithAuth(&authn.Basic{Username: "oauth2accesstoken", Password: tok})); err != nil {
		t.Fatalf("push: %v", err)
	}

	nodeSA := "gke-nodes@" + project + ".iam.gserviceaccount.com"
	gw(t, inst, "POST", "/iam/v1/projects/"+project+"/serviceAccounts", map[string]any{"accountId": "gke-nodes"}, nil)
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "pull", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1, Config: &containerpb.NodeConfig{ServiceAccount: nodeSA}}}}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name, 3*time.Minute)
	c, _ := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: parent + "/clusters/pull"})
	adm, pool := adminClient(t, inst, parent+"/clusters/pull")
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}

	image := "us-central1-docker.pkg.dev/" + project + "/apps/app:v1"
	pod := func(name string) {
		e.mustKube("POST", "/api/v1/namespaces/default/pods", map[string]any{
			"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": name},
			"spec": map[string]any{"containers": []map[string]any{{"name": "c", "image": image, "imagePullPolicy": "Always"}}},
		}, nil)
	}
	// pulled reports "Pulled" or "Failed" once the kubelet has tried.
	pulled := func(name string) string {
		var ev struct {
			Items []struct{ Reason, Message string }
		}
		e.mustKube("GET", "/api/v1/namespaces/default/events?fieldSelector=involvedObject.name%3D"+name, nil, &ev)
		for _, it := range ev.Items {
			if it.Reason == "Pulled" {
				return "Pulled"
			}
			if it.Reason == "Failed" && strings.Contains(it.Message, "pull") {
				return "Failed: " + it.Message
			}
		}
		return ""
	}
	var got string
	pod("denied")
	eventually(t, 90*time.Second, "a pull attempt without permission", func() bool { got = pulled("denied"); return got != "" })
	if !strings.HasPrefix(got, "Failed") {
		t.Fatalf("pull without artifactregistry.reader: %s", got)
	}

	pol := map[string]any{"policy": map[string]any{"bindings": []map[string]any{
		{"role": "roles/artifactregistry.reader", "members": []string{"serviceAccount:" + nodeSA}}}}}
	if code := gw(t, inst, "POST", repo+"/apps:setIamPolicy", pol, nil); code != 200 {
		t.Fatalf("setIamPolicy: %d", code)
	}
	pod("allowed")
	eventually(t, 90*time.Second, "a pull attempt with permission", func() bool { got = pulled("allowed"); return got != "" })
	if got != "Pulled" {
		t.Fatalf("pull with artifactregistry.reader: %s", got)
	}
}
