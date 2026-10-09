package gke

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/locations"
)

// Secret Manager reads for the `gke` CSI provider and secret
// synchronization. Both act as a workload's Kubernetes service account
// with the Workload Identity rules of the metadata server (wi.go): the
// annotated Google service account when the KSA may impersonate it,
// otherwise the KSA's federated identity.

// spcSecret is one entry of a SecretProviderClass's `secrets` parameter.
type spcSecret struct {
	ResourceName string `yaml:"resourceName" json:"resourceName"`
	Path         string `yaml:"path" json:"path"`
	FileName     string `yaml:"fileName" json:"fileName"` // the open-source provider's key
}

// parseSPCSecrets parses the `secrets` parameter.
func parseSPCSecrets(param string) ([]spcSecret, error) {
	var out []spcSecret
	if err := yaml.Unmarshal([]byte(param), &out); err != nil {
		return nil, fmt.Errorf("invalid secrets parameter: %w", err)
	}
	for i := range out {
		if out[i].Path == "" {
			out[i].Path = out[i].FileName
		}
		p := out[i].Path
		if p == "" || path.IsAbs(p) || strings.Contains(p, "..") {
			return nil, fmt.Errorf("secret %q: path %q must be a relative file name", out[i].ResourceName, p)
		}
	}
	return out, nil
}

// workloadPrincipal returns the Principal the KSA ns/ksa acts as.
func (s *Service) workloadPrincipal(ctx context.Context, key string, c *containerpb.Cluster, ns, ksa string) (emu.Principal, error) {
	pool := c.GetWorkloadIdentityConfig().GetWorkloadPool()
	if pool == "" {
		return "", apierr.FailedPrecondition("Workload Identity is not enabled on the cluster.")
	}
	kc, err := s.kube(key)
	if err != nil {
		return "", err
	}
	var sa struct {
		Metadata objectMeta `json:"metadata"`
	}
	if err := kc.get(ctx, "/api/v1/namespaces/"+ns+"/serviceaccounts/"+ksa, &sa); err != nil {
		return "", fmt.Errorf("service account %s/%s: %w", ns, ksa, err)
	}
	member := "serviceAccount:" + pool + "[" + ns + "/" + ksa + "]"
	gsa := strings.TrimSpace(sa.Metadata.Annotations[annoGSA])
	if gsa == "" {
		return emu.Principal(member), nil
	}
	if !s.wiAllowed(ctx, member, gsa) {
		return "", apierr.PermissionDenied("Permission 'iam.serviceAccounts.getAccessToken' denied on %s for %s; grant roles/iam.workloadIdentityUser.", gsa, member)
	}
	return emu.Principal("serviceAccount:" + gsa), nil
}

// secretFile is a fetched secret and the version it came from.
type secretFile struct {
	Path     string `json:"path"`
	Contents []byte `json:"contents"`
	Resource string `json:"resource"`
	Version  string `json:"version"`
}

// readSecrets reads secrets as the KSA ns/ksa. A regional secret must be
// in the cluster's region, as on GKE.
func (s *Service) readSecrets(ctx context.Context, key string, c *containerpb.Cluster, ns, ksa string, secrets []spcSecret) ([]secretFile, error) {
	svc, ok := s.env.Lookup("secrets")
	acc, ok2 := svc.(emu.SecretAccessor)
	if !ok || !ok2 {
		return nil, apierr.FailedPrecondition("the emulator's Secret Manager Service (secrets) is not running")
	}
	p, err := s.workloadPrincipal(ctx, key, c, ns, ksa)
	if err != nil {
		return nil, err
	}
	region, _ := locations.RegionOf(c.Location)
	ctx = emu.WithPrincipal(ctx, p)
	var out []secretFile
	for _, sec := range secrets {
		parts := strings.Split(sec.ResourceName, "/")
		if len(parts) == 8 && parts[2] == "locations" && parts[3] != region {
			return nil, apierr.InvalidArgument("Secret %s is in %s; regional secrets must be in the cluster's region %s.", sec.ResourceName, parts[3], region)
		}
		data, version, err := acc.AccessSecretVersion(ctx, sec.ResourceName)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sec.ResourceName, err)
		}
		out = append(out, secretFile{Path: sec.Path, Contents: data, Resource: sec.ResourceName, Version: version})
	}
	return out, nil
}

// providerMount is the node agent's request for a CSI volume mount.
type providerMount struct {
	// Attributes is the driver's JSON-encoded volume attributes.
	Attributes string `json:"attributes"`
}

// serveProviderMount answers the `gke` provider on a node: the volume's
// secrets read as the mounting pod's service account.
func (s *Service) serveProviderMount(w http.ResponseWriter, r *http.Request, key string) {
	var req providerMount
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierr.Write(w, apierr.InvalidArgument("bad mount request: %v", err))
		return
	}
	var attrs map[string]string
	if err := json.Unmarshal([]byte(req.Attributes), &attrs); err != nil {
		apierr.Write(w, apierr.InvalidArgument("bad attributes: %v", err))
		return
	}
	rec, err := s.loadKey(key)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	c := rec.cluster()
	if !secretManagerOn(c) {
		apierr.Write(w, apierr.FailedPrecondition("The Secret Manager add-on is not enabled on cluster %s.", c.Name))
		return
	}
	secrets, err := parseSPCSecrets(attrs["secrets"])
	if err != nil {
		apierr.Write(w, apierr.InvalidArgument("%v", err))
		return
	}
	ns, ksa := attrs["csi.storage.k8s.io/pod.namespace"], attrs["csi.storage.k8s.io/serviceAccount.name"]
	if ns == "" || ksa == "" {
		apierr.Write(w, apierr.InvalidArgument("the mount carries no pod namespace or service account (CSIDriver podInfoOnMount)"))
		return
	}
	files, err := s.readSecrets(r.Context(), key, c, ns, ksa, secrets)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, map[string]any{"files": files})
}
