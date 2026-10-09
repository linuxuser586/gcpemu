package gke_test

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/linuxuser586/gcpemu/emutest"
	projectid "github.com/linuxuser586/gcpemu/internal/project"
)

// The cluster settings gatewayApiConfig, secretManagerConfig and
// secretSyncConfig install their components: a pod mounts a Secret
// Manager secret through secrets-store-gke.csi.k8s.io as its KSA (bound
// with GKE's principal:// member), a SecretSync writes it to a Kubernetes
// Secret, and disabling the settings removes the components. The CSI
// driver images are pulled through Artifact Registry (GCPEMU_NET_TESTS=1).
func TestClusterAddons(t *testing.T) {
	emutest.RequireRuntime(t)
	if os.Getenv("GCPEMU_NET_TESTS") != "1" {
		t.Skip("set GCPEMU_NET_TESTS=1 to pull the Secrets Store CSI driver images")
	}
	t.Parallel()
	const proj = project
	inst := emutest.Start(t, []string{"gke", "secrets"}, emutest.WithIAMMode("enforce"))
	cm := newClient(t, inst)
	ctx := context.Background()
	loc := "us-central1-a"
	par := "projects/" + proj + "/locations/" + loc
	on := true

	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: par, Cluster: &containerpb.Cluster{
		Name:                   "addons",
		WorkloadIdentityConfig: &containerpb.WorkloadIdentityConfig{WorkloadPool: proj + ".svc.id.goog"},
		NetworkConfig:          &containerpb.NetworkConfig{GatewayApiConfig: &containerpb.GatewayAPIConfig{Channel: containerpb.GatewayAPIConfig_CHANNEL_STANDARD}},
		SecretManagerConfig:    &containerpb.SecretManagerConfig{Enabled: &on},
		SecretSyncConfig: &containerpb.SecretSyncConfig{Enabled: &on, RotationConfig: &containerpb.SecretSyncConfig_SyncRotationConfig{
			Enabled: &on, RotationInterval: durationpb.New(time.Minute)}},
		NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}

	// The secret, readable by the app/web KSA through GKE's principal://
	// member; the node pulls the driver images through AR.
	var p struct {
		ProjectNumber string `json:"projectNumber"`
	}
	mustGW(t, inst, "GET", "/cloudresourcemanager/v1/projects/"+proj, nil, &p)
	mustGW(t, inst, "POST", "/cloudresourcemanager/v1/projects/"+proj+":setIamPolicy", map[string]any{"policy": map[string]any{"bindings": []map[string]any{
		{"role": "roles/owner", "members": []string{inst.Config.DefaultPrincipal}},
		{"role": "roles/artifactregistry.reader", "members": []string{"serviceAccount:" + p.ProjectNumber + "-compute@developer.gserviceaccount.com"}},
	}}}, nil)
	sm := "/secretmanager/v1/projects/" + proj
	mustGW(t, inst, "POST", sm+"/secrets?secretId=app-config", map[string]any{"replication": map[string]any{"automatic": map[string]any{}}}, nil)
	mustGW(t, inst, "POST", sm+"/secrets/app-config:addVersion", map[string]any{"payload": map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("hello-v1"))}}, nil)
	member := "principal://iam.googleapis.com/projects/" + projectid.NumberString(proj) + "/locations/global/workloadIdentityPools/" + proj + ".svc.id.goog/subject/ns/app/sa/web"
	mustGW(t, inst, "POST", sm+"/secrets/app-config:setIamPolicy", map[string]any{"policy": map[string]any{"bindings": []map[string]any{
		{"role": "roles/secretmanager.secretAccessor", "members": []string{member}},
	}}}, nil)

	waitOp(t, cm, loc, op.Name, 4*time.Minute)
	c, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: par + "/clusters/addons"})
	if err != nil {
		t.Fatal(err)
	}
	adm, pool := adminClient(t, inst, par+"/clusters/addons")
	e := &env{t: t, inst: inst, cm: cm, c: c, ca: pool, adm: adm}
	crd := "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"
	for _, n := range []string{"gateways.gateway.networking.k8s.io", "httproutes.gateway.networking.k8s.io", "secretproviderclasses.secrets-store.csi.x-k8s.io", "secretsyncs.secret-sync.gke.io"} {
		e.mustKube("GET", crd+n, nil, nil)
	}
	if code, _ := e.kube("GET", crd+"tcproutes.gateway.networking.k8s.io", "", nil, nil); code != 404 {
		t.Errorf("experimental CRD on the standard channel: %d", code)
	}

	e.mustKube("POST", "/api/v1/namespaces", map[string]any{"metadata": map[string]any{"name": "app"}}, nil)
	e.mustKube("POST", "/api/v1/namespaces/app/serviceaccounts", map[string]any{"metadata": map[string]any{"name": "web"}}, nil)
	spc := "/apis/secrets-store.csi.x-k8s.io/v1/namespaces/app/secretproviderclasses"
	e.mustKube("POST", spc, map[string]any{
		"apiVersion": "secrets-store.csi.x-k8s.io/v1", "kind": "SecretProviderClass",
		"metadata": map[string]any{"name": "app-secrets"},
		"spec": map[string]any{"provider": "gke", "parameters": map[string]any{"secrets": `
- resourceName: "projects/` + proj + `/secrets/app-config/versions/latest"
  path: "config.txt"
`}},
	}, nil)

	t.Run("CSIMount", func(t *testing.T) {
		e.mustKube("POST", "/api/v1/namespaces/app/pods", map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "reader"},
			"spec": map[string]any{
				"serviceAccountName": "web", "restartPolicy": "Never",
				"containers": []map[string]any{{
					"name": "c", "image": testImage, "imagePullPolicy": "IfNotPresent",
					"command":      []string{"sh", "-c", "echo SECRET=$(cat /mnt/secrets/config.txt)"},
					"volumeMounts": []map[string]any{{"name": "s", "mountPath": "/mnt/secrets", "readOnly": true}},
				}},
				"volumes": []map[string]any{{"name": "s", "csi": map[string]any{
					"driver": "secrets-store-gke.csi.k8s.io", "readOnly": true,
					"volumeAttributes": map[string]string{"secretProviderClass": "app-secrets"},
				}}},
			},
		}, nil)
		deadline := time.Now().Add(5 * time.Minute)
		for !strings.Contains(e.podLogs("app", "reader"), "SECRET=hello-v1") {
			if time.Now().After(deadline) {
				_, ev := e.kube("GET", "/api/v1/namespaces/app/events", "", nil, nil)
				_, ds := e.kube("GET", "/api/v1/namespaces/kube-system/pods?labelSelector=app%3Dsecrets-store-gke", "", nil, nil)
				t.Logf("events: %s", ev)
				t.Logf("driver pods: %s", ds)
				var pods struct {
					Items []struct {
						Metadata struct{ Name string } `json:"metadata"`
					} `json:"items"`
				}
				e.kube("GET", "/api/v1/namespaces/kube-system/pods?labelSelector=app%3Dsecrets-store-gke", "", nil, &pods)
				for _, p := range pods.Items {
					_, l := e.kube("GET", "/api/v1/namespaces/kube-system/pods/"+p.Metadata.Name+"/log?container=secrets-store&tailLines=40", "", nil, nil)
					t.Logf("driver log %s: %s", p.Metadata.Name, l)
				}
				t.Fatal("timed out waiting for the pod to print the mounted secret")
			}
			time.Sleep(time.Second)
		}
	})

	t.Run("SecretSync", func(t *testing.T) {
		e.mustKube("POST", "/apis/secret-sync.gke.io/v1/namespaces/app/secretsyncs", map[string]any{
			"apiVersion": "secret-sync.gke.io/v1", "kind": "SecretSync",
			"metadata": map[string]any{"name": "synced"},
			"spec": map[string]any{"serviceAccountName": "web", "secretProviderClassName": "app-secrets",
				"secretObject": map[string]any{"type": "Opaque", "data": []map[string]string{{"sourcePath": "config.txt", "targetKey": "CONFIG"}}}},
		}, nil)
		var sec struct {
			Data map[string][]byte `json:"data"`
		}
		eventually(t, time.Minute, "the synced Secret", func() bool {
			code, _ := e.kube("GET", "/api/v1/namespaces/app/secrets/synced", "", nil, &sec)
			return code == 200 && string(sec.Data["CONFIG"]) == "hello-v1"
		})
		// A KSA without access fails the sync with a condition.
		e.mustKube("POST", "/api/v1/namespaces/app/serviceaccounts", map[string]any{"metadata": map[string]any{"name": "other"}}, nil)
		e.mustKube("POST", "/apis/secret-sync.gke.io/v1/namespaces/app/secretsyncs", map[string]any{
			"apiVersion": "secret-sync.gke.io/v1", "kind": "SecretSync",
			"metadata": map[string]any{"name": "denied"},
			"spec": map[string]any{"serviceAccountName": "other", "secretProviderClassName": "app-secrets",
				"secretObject": map[string]any{"type": "Opaque", "data": []map[string]string{{"sourcePath": "config.txt", "targetKey": "CONFIG"}}}},
		}, nil)
		eventually(t, time.Minute, "a SyncFailed condition", func() bool {
			_, b := e.kube("GET", "/apis/secret-sync.gke.io/v1/namespaces/app/secretsyncs/denied", "", nil, nil)
			return strings.Contains(b, "SyncFailed") && strings.Contains(b, "secretmanager.versions.access")
		})
	})

	t.Run("Disable", func(t *testing.T) {
		off := false
		for _, u := range []*containerpb.ClusterUpdate{
			{DesiredGatewayApiConfig: &containerpb.GatewayAPIConfig{Channel: containerpb.GatewayAPIConfig_CHANNEL_DISABLED}},
			{DesiredSecretManagerConfig: &containerpb.SecretManagerConfig{Enabled: &off}},
		} {
			op, err := cm.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: par + "/clusters/addons", Update: u})
			if err != nil {
				t.Fatal(err)
			}
			waitOp(t, cm, loc, op.Name, 2*time.Minute)
		}
		if code, _ := e.kube("GET", crd+"gateways.gateway.networking.k8s.io", "", nil, nil); code != 404 {
			t.Errorf("Gateway CRD after disabling: %d", code)
		}
		if code, _ := e.kube("GET", "/apis/apps/v1/namespaces/kube-system/daemonsets/secrets-store-gke", "", nil, nil); code != 404 {
			t.Errorf("CSI driver after disabling: %d", code)
		}
		e.mustKube("GET", crd+"secretproviderclasses.secrets-store.csi.x-k8s.io", nil, nil)
		got, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: par + "/clusters/addons"})
		if err != nil || got.GetSecretManagerConfig().GetEnabled() {
			t.Fatalf("cluster after disabling: %v %v", got.GetSecretManagerConfig(), err)
		}
	})
}
