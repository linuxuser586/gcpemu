package gke

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Cluster add-ons that follow cluster settings. reconcileAddons installs
// what a setting enables, on create, on update and after the control plane
// is recreated, and removes what a disabled setting installed, keeping
// CRDs, user objects and synced Secrets:
//   - networkConfig.gatewayApiConfig: the Gateway API CRDs (v1.5.1, which
//     GKE supports from 1.35.2-gke.1842000) of the chosen channel; removed
//     only while no Gateway API object exists;
//   - secretManagerConfig: the Secrets Store CSI driver registered as GKE's
//     secrets-store-gke.csi.k8s.io; its `gke` provider is the node agent
//     (provider.go);
//   - secretSyncConfig: the SecretSync CRD; the controller runs in the
//     emulator (secretsync.go) while the setting is enabled.

//go:embed addons
var addonFS embed.FS

const (
	addonLabel       = "gcpemu.dev/addon"
	gatewayAPIVer    = "v1.5.1"
	defaultRotation  = 2 * time.Minute
	minAddonRotation = time.Minute
)

// manifest decodes an embedded multi-document YAML file (gzipped if it
// ends in .gz).
func manifest(name string) ([]map[string]any, error) {
	b, err := addonFS.ReadFile("addons/" + name)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(name, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		if b, err = io.ReadAll(zr); err != nil {
			return nil, err
		}
	}
	return decodeManifest(b)
}

func decodeManifest(b []byte) ([]map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var out []map[string]any
	for {
		var obj map[string]any
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if obj != nil {
			out = append(out, obj)
		}
	}
}

// kindPaths maps the kinds the add-ons use to their API paths.
var kindPaths = map[string]struct {
	group, resource string
	namespaced      bool
}{
	"CustomResourceDefinition":         {"apis/apiextensions.k8s.io/v1", "customresourcedefinitions", false},
	"ValidatingAdmissionPolicy":        {"apis/admissionregistration.k8s.io/v1", "validatingadmissionpolicies", false},
	"ValidatingAdmissionPolicyBinding": {"apis/admissionregistration.k8s.io/v1", "validatingadmissionpolicybindings", false},
	"ServiceAccount":                   {"api/v1", "serviceaccounts", true},
	"ClusterRole":                      {"apis/rbac.authorization.k8s.io/v1", "clusterroles", false},
	"ClusterRoleBinding":               {"apis/rbac.authorization.k8s.io/v1", "clusterrolebindings", false},
	"CSIDriver":                        {"apis/storage.k8s.io/v1", "csidrivers", false},
	"DaemonSet":                        {"apis/apps/v1", "daemonsets", true},
}

// objectPath returns the collection path of obj and its name.
func objectPath(obj map[string]any) (string, string, error) {
	kind, _ := obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	kp, ok := kindPaths[kind]
	if !ok || name == "" {
		return "", "", fmt.Errorf("add-on manifest: unsupported object %s %q", kind, name)
	}
	if kp.namespaced {
		ns, _ := meta["namespace"].(string)
		if ns == "" {
			ns = "default"
		}
		return "/" + kp.group + "/namespaces/" + ns + "/" + kp.resource, name, nil
	}
	return "/" + kp.group + "/" + kp.resource, name, nil
}

// applyAddon creates or replaces objs, labelled as add-on addon.
func applyAddon(ctx context.Context, kc *kubeClient, addon string, objs []map[string]any) error {
	for _, obj := range objs {
		coll, name, err := objectPath(obj)
		if err != nil {
			return err
		}
		meta := obj["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		labels[addonLabel] = addon
		meta["labels"] = labels
		if err := applyObject(ctx, kc, coll, name, obj); err != nil {
			return fmt.Errorf("%s %s: %w", obj["kind"], name, err)
		}
	}
	return nil
}

// deleteAddon deletes objs except those of the kinds in keep.
func deleteAddon(ctx context.Context, kc *kubeClient, objs []map[string]any, keep ...string) error {
	for _, obj := range objs {
		kind, _ := obj["kind"].(string)
		if contains(keep, kind) {
			continue
		}
		coll, name, err := objectPath(obj)
		if err != nil {
			return err
		}
		if err := kc.delete(ctx, coll+"/"+name); err != nil {
			return fmt.Errorf("%s %s: %w", kind, name, err)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// rotation returns whether an add-on's rotation is enabled and its
// interval (default 2 minutes).
func rotation(rc interface {
	GetEnabled() bool
	GetRotationInterval() *durationpb.Duration
}) (bool, time.Duration) {
	if d := rc.GetRotationInterval(); d != nil && d.AsDuration() > 0 {
		return rc.GetEnabled(), d.AsDuration()
	}
	return rc.GetEnabled(), defaultRotation
}

func secretManagerOn(c *containerpb.Cluster) bool {
	return c.GetSecretManagerConfig().GetEnabled()
}

func secretSyncOn(c *containerpb.Cluster) bool {
	return c.GetSecretSyncConfig().GetEnabled()
}

// checkAddons validates the add-on settings of c (FAILED_PRECONDITION or
// INVALID_ARGUMENT as GKE answers).
func (s *Service) checkAddons(c *containerpb.Cluster) error {
	switch ch := c.GetNetworkConfig().GetGatewayApiConfig().GetChannel(); ch {
	case containerpb.GatewayAPIConfig_CHANNEL_UNSPECIFIED, containerpb.GatewayAPIConfig_CHANNEL_DISABLED,
		containerpb.GatewayAPIConfig_CHANNEL_STANDARD, containerpb.GatewayAPIConfig_CHANNEL_EXPERIMENTAL:
	default:
		return apierr.InvalidArgument("Invalid Gateway API channel %v.", ch)
	}
	if !secretManagerOn(c) && !secretSyncOn(c) {
		return nil
	}
	if d := c.GetSecretManagerConfig().GetRotationConfig().GetRotationInterval(); d != nil && d.AsDuration() < minAddonRotation {
		return apierr.InvalidArgument("secretManagerConfig.rotationConfig.rotationInterval must be at least 60s.")
	}
	if d := c.GetSecretSyncConfig().GetRotationConfig().GetRotationInterval(); d != nil && d.AsDuration() < minAddonRotation {
		return apierr.InvalidArgument("secretSyncConfig.rotationConfig.rotationInterval must be at least 60s.")
	}
	if c.GetWorkloadIdentityConfig().GetWorkloadPool() == "" {
		return apierr.FailedPrecondition("The Secret Manager add-on and secret synchronization require Workload Identity Federation for GKE (workloadIdentityConfig.workloadPool).")
	}
	if _, ok := s.env.Lookup("secrets"); !ok {
		return apierr.FailedPrecondition("The Secret Manager add-on and secret synchronization need the emulator's Secret Manager Service (secrets), which is not running.")
	}
	return nil
}

// reconcileAddons brings the cluster's add-ons in line with its settings.
func (s *Service) reconcileAddons(ctx context.Context, key string) error {
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	c := rec.cluster()
	kc, err := s.kube(key)
	if err != nil {
		return err
	}
	return errors.Join(
		s.reconcileGatewayAPI(ctx, kc, c.GetNetworkConfig().GetGatewayApiConfig().GetChannel()),
		s.reconcileSecretManager(ctx, kc, c),
		s.reconcileSecretSync(ctx, kc, c),
	)
}

func (s *Service) reconcileGatewayAPI(ctx context.Context, kc *kubeClient, ch containerpb.GatewayAPIConfig_Channel) error {
	std, err := manifest("gateway-api-" + gatewayAPIVer + "-standard.yaml.gz")
	if err != nil {
		return err
	}
	exp, err := manifest("gateway-api-" + gatewayAPIVer + "-experimental.yaml.gz")
	if err != nil {
		return err
	}
	switch ch {
	case containerpb.GatewayAPIConfig_CHANNEL_STANDARD:
		// Moving from experimental drops the experimental-only CRDs.
		if err := s.removeGatewayCRDs(ctx, kc, without(exp, std)); err != nil {
			return err
		}
		return applyAddon(ctx, kc, "gateway-api", std)
	case containerpb.GatewayAPIConfig_CHANNEL_EXPERIMENTAL:
		return applyAddon(ctx, kc, "gateway-api", exp)
	}
	return s.removeGatewayCRDs(ctx, kc, exp)
}

// without returns the objects of a whose names are not in b.
func without(a, b []map[string]any) []map[string]any {
	names := map[string]bool{}
	for _, o := range b {
		_, n, _ := objectPath(o)
		names[n] = true
	}
	var out []map[string]any
	for _, o := range a {
		if _, n, _ := objectPath(o); !names[n] {
			out = append(out, o)
		}
	}
	return out
}

// removeGatewayCRDs deletes the add-on's Gateway API objects that exist,
// unless some Gateway API resource still does.
func (s *Service) removeGatewayCRDs(ctx context.Context, kc *kubeClient, objs []map[string]any) error {
	var installed []map[string]any
	for _, obj := range objs {
		coll, name, err := objectPath(obj)
		if err != nil {
			return err
		}
		var cur struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				Group string `json:"group"`
				Names struct {
					Plural string `json:"plural"`
				} `json:"names"`
				Versions []struct {
					Name   string `json:"name"`
					Served bool   `json:"served"`
				} `json:"versions"`
			} `json:"spec"`
		}
		err = kc.get(ctx, coll+"/"+name, &cur)
		if isKubeNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if cur.Metadata.Labels[addonLabel] != "gateway-api" {
			continue // installed by someone else
		}
		installed = append(installed, obj)
		if obj["kind"] != "CustomResourceDefinition" {
			continue
		}
		for _, v := range cur.Spec.Versions {
			if !v.Served {
				continue
			}
			var list struct {
				Items []map[string]any `json:"items"`
			}
			if err := kc.get(ctx, "/apis/"+cur.Spec.Group+"/"+v.Name+"/"+cur.Spec.Names.Plural+"?limit=1", &list); err == nil && len(list.Items) > 0 {
				s.env.Log.Info("gke: keeping the Gateway API CRDs while Gateway API objects exist", "crd", name)
				return nil
			}
		}
	}
	return deleteAddon(ctx, kc, installed)
}

func (s *Service) reconcileSecretManager(ctx context.Context, kc *kubeClient, c *containerpb.Cluster) error {
	cfg := c.GetSecretManagerConfig()
	on, every := rotation(cfg.GetRotationConfig())
	b, err := addonFS.ReadFile("addons/secret-manager.yaml")
	if err != nil {
		return err
	}
	b = bytes.ReplaceAll(b, []byte("ROTATION"), []byte(fmt.Sprint(on)))
	b = bytes.ReplaceAll(b, []byte("INTERVAL"), []byte(every.String()))
	objs, err := decodeManifest(b)
	if err != nil {
		return err
	}
	if secretManagerOn(c) {
		return applyAddon(ctx, kc, "secret-manager", objs)
	}
	return s.removeIfInstalled(ctx, kc, "secret-manager", objs, "CustomResourceDefinition")
}

func (s *Service) reconcileSecretSync(ctx context.Context, kc *kubeClient, c *containerpb.Cluster) error {
	if !secretSyncOn(c) {
		return nil // the controller stops by itself; the CRD and Secrets stay
	}
	objs, err := manifest("secret-sync.yaml")
	if err != nil {
		return err
	}
	return applyAddon(ctx, kc, "secret-sync", objs)
}

// removeIfInstalled deletes objs when the add-on was installed, i.e. its
// first object of a kind not kept carries the add-on label.
func (s *Service) removeIfInstalled(ctx context.Context, kc *kubeClient, addon string, objs []map[string]any, keep ...string) error {
	for _, obj := range objs {
		if contains(keep, obj["kind"].(string)) {
			continue
		}
		coll, name, err := objectPath(obj)
		if err != nil {
			return err
		}
		var cur struct {
			Metadata objectMeta `json:"metadata"`
		}
		err = kc.get(ctx, coll+"/"+name, &cur)
		if isKubeNotFound(err) || err == nil && cur.Metadata.Labels[addonLabel] != addon {
			return nil
		}
		if err != nil {
			return err
		}
		break
	}
	return deleteAddon(ctx, kc, objs, keep...)
}
