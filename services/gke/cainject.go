package gke

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/trust"
	"github.com/linuxuser586/gcpemu/internal/workload"
)

// CA injection into pods (FR-INT-007, Section 7.4).
//
// Container images do not trust the emulator's CA, so TLS to the Google
// frontend would fail. Every cluster gets a MutatingWebhookConfiguration
// whose webhook is served by the emulator through the frontend
// (https://<services gateway>:<frontend port>/container/_emu/hooks/...;
// the frontend's no-SNI certificate covers the host's IP addresses and
// the caBundle is the instance CA). For each pod created outside
// kube-system and not opted out with the label gcpemu.dev/inject=disabled
// (on the namespace or the pod) it:
//
//   - ensures a ConfigMap "gcpemu-ca-bundle" in the pod's namespace with
//     the host's system roots plus the emulator CA (a ConfigMap rather
//     than a hostPath volume keeps pods admissible under Pod Security
//     "baseline" and "restricted");
//   - mounts it at /etc/gcpemu/certs and, file-wise, over
//     /etc/ssl/certs/ca-certificates.crt (unless the container already
//     mounts something there);
//   - sets SSL_CERT_FILE (Go, OpenSSL), REQUESTS_CA_BUNDLE (Python
//     requests) and NODE_EXTRA_CA_CERTS (Node.js) to the bundle, and
//     GCE_METADATA_HOST=169.254.169.254 (see metadataHostEnv), unless the
//     container sets them.
//
// A pod labelled gcpemu.dev/googleapis=real (a builder such as BuildKit,
// whose build containers are started by the pod and never get the CA)
// also gets dnsPolicy None and a dnsConfig naming the node's googleapis
// resolver (googleAPIsDNSIP, nodeagent.go), which resolves *.googleapis.com
// as Cloud DNS does on GCP, to real Google, and every other name through
// the cluster's DNS. Its build containers inherit that resolv.conf, so they
// reach real Google APIs, such as the public bucket behind proxy.golang.org,
// while LOCATION-docker.pkg.dev stays emulated (ADR 0019). A pod that
// sets dnsPolicy None keeps its own DNS; a dnsConfig of its own is merged.
//
// The webhook's failurePolicy is Ignore: pods are admitted unchanged when
// the emulator is unreachable.

const (
	caInjectorName   = "gcpemu-ca-injector"
	caBundleName     = "gcpemu-ca-bundle"
	caBundleKey      = workload.CABundleKey
	caBundleDir      = workload.CABundleDir
	caBundlePath     = workload.CABundlePath
	systemBundlePath = workload.SystemBundlePath
	injectLabel      = "gcpemu.dev/inject"
	// googleAPIsLabel=googleAPIsReal gives a pod real Google APIs (see above).
	googleAPIsLabel = "gcpemu.dev/googleapis"
	googleAPIsReal  = "real"
	dnsPolicyNone   = "None"
	clusterDomain   = "cluster.local"
)

// caEnvVars are set to the bundle path in every container.
var caEnvVars = workload.CAEnvVars

// metadataHostEnv tells client libraries they run on GCE (see
// workload.MetadataHostEnv).
const metadataHostEnv = workload.MetadataHostEnv

// installCAInjector registers (or updates) the CA-injecting webhook in a
// running cluster. Errors are logged: injection is best effort.
func (s *Service) installCAInjector(ctx context.Context, d *deps, key string) {
	hooks, kc, ok := s.hookBase(ctx, d, key, "pods will not trust the emulator CA")
	if !ok {
		return
	}
	if err := applyObject(ctx, kc, "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations", caInjectorName,
		caInjectorConfig(hooks+"admit", s.env.CA.PEM())); err != nil {
		s.env.Log.Warn("gke: registering the CA injection webhook failed", "cluster", key, "err", err)
	}
}

// hookBase returns the URL under which the frontend serves the cluster's
// admission webhooks, ending in "/", and a client of the cluster, or false
// when there is no frontend (logged at debug with lacking, what pods then
// lack) or no cluster.
func (s *Service) hookBase(ctx context.Context, d *deps, key, lacking string) (string, *kubeClient, bool) {
	if s.env.CA == nil {
		return "", nil, false
	}
	fe, err := d.np.Addr(ctx, frontend.Endpoint)
	if err != nil {
		s.env.Log.Debug("gke: no Google front end; "+lacking, "err", err)
		return "", nil, false
	}
	rec, err := s.loadKey(key)
	if err != nil {
		return "", nil, false
	}
	kc, err := s.kube(key)
	if err != nil {
		return "", nil, false
	}
	return "https://" + fe + "/container/_emu/hooks/" + rec.cluster().Id + "/" + rec.Int.Secret + "/", kc, true
}

// caInjectorConfig is the MutatingWebhookConfiguration.
func caInjectorConfig(url string, caPEM []byte) map[string]any {
	notDisabled := map[string]any{"key": injectLabel, "operator": "NotIn", "values": []string{"disabled"}}
	return map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "MutatingWebhookConfiguration",
		"metadata": map[string]any{
			"name":   caInjectorName,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "gcpemu"},
		},
		"webhooks": []map[string]any{{
			"name":                    "ca-injector.gcpemu.dev",
			"admissionReviewVersions": []string{"v1"},
			"sideEffects":             "NoneOnDryRun",
			"failurePolicy":           "Ignore",
			"timeoutSeconds":          10,
			"reinvocationPolicy":      "IfNeeded",
			"clientConfig": map[string]any{
				"url":      url,
				"caBundle": base64.StdEncoding.EncodeToString(caPEM),
			},
			"rules": []map[string]any{{
				"apiGroups": []string{""}, "apiVersions": []string{"v1"},
				"operations": []string{"CREATE"}, "resources": []string{"pods"}, "scope": "Namespaced",
			}},
			"namespaceSelector": map[string]any{"matchExpressions": []map[string]any{
				{"key": "kubernetes.io/metadata.name", "operator": "NotIn", "values": []string{"kube-system"}},
				notDisabled,
			}},
			"objectSelector": map[string]any{"matchExpressions": []map[string]any{notDisabled}},
		}},
	}
}

// applyObject creates the named object under collection or replaces it.
func applyObject(ctx context.Context, kc *kubeClient, collection, name string, obj map[string]any) error {
	var cur struct {
		Metadata objectMeta `json:"metadata"`
	}
	err := kc.get(ctx, collection+"/"+name, &cur)
	if isKubeNotFound(err) {
		return kc.do(ctx, http.MethodPost, collection, "", obj, nil)
	}
	if err != nil {
		return err
	}
	meta := obj["metadata"].(map[string]any)
	meta["resourceVersion"] = cur.Metadata.ResourceVersion
	return kc.do(ctx, http.MethodPut, collection+"/"+name, "", obj, nil)
}

// ---- admission ----

type admissionReview struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Request    *admissionRequest  `json:"request,omitempty"`
	Response   *admissionResponse `json:"response,omitempty"`
}

type admissionRequest struct {
	UID      string `json:"uid"`
	Resource struct {
		Group    string `json:"group"`
		Resource string `json:"resource"`
	} `json:"resource"`
	Namespace string `json:"namespace"`
	Operation string `json:"operation"`
	UserInfo  struct {
		Username string   `json:"username"`
		Groups   []string `json:"groups"`
	} `json:"userInfo"`
	DryRun *bool           `json:"dryRun,omitempty"`
	Object json.RawMessage `json:"object"`
}

type admissionResponse struct {
	UID       string           `json:"uid"`
	Allowed   bool             `json:"allowed"`
	Status    *admissionStatus `json:"status,omitempty"`
	PatchType string           `json:"patchType,omitempty"`
	Patch     string           `json:"patch,omitempty"`
	Warnings  []string         `json:"warnings,omitempty"`
}

// admissionStatus is the Status a refusing webhook returns.
type admissionStatus struct {
	Code    int    `json:"code"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message"`
}

// admitPod is the subset of a Pod the injector reads.
type admitPod struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		DNSPolicy string     `json:"dnsPolicy"`
		DNSConfig *dnsConfig `json:"dnsConfig"`
		Volumes   []struct {
			Name string `json:"name"`
		} `json:"volumes"`
		Containers     []admitContainer `json:"containers"`
		InitContainers []admitContainer `json:"initContainers"`
	} `json:"spec"`
}

// dnsConfig is a pod's spec.dnsConfig.
type dnsConfig struct {
	Nameservers []string    `json:"nameservers,omitempty"`
	Options     []dnsOption `json:"options,omitempty"`
	Searches    []string    `json:"searches,omitempty"`
}

type dnsOption struct {
	Name  string  `json:"name"`
	Value *string `json:"value,omitempty"`
}

type admitContainer struct {
	Env []struct {
		Name string `json:"name"`
	} `json:"env"`
	VolumeMounts []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
	} `json:"volumeMounts"`
}

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// servePodAdmission answers the CA injection webhook.
func (s *Service) servePodAdmission(w http.ResponseWriter, r *http.Request, key string) {
	var ar admissionReview
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil || ar.Request == nil {
		http.Error(w, "bad AdmissionReview", http.StatusBadRequest)
		return
	}
	req := ar.Request
	resp := &admissionResponse{UID: req.UID, Allowed: true}
	var pod admitPod
	if req.Operation == "CREATE" && json.Unmarshal(req.Object, &pod) == nil {
		if ops := injectPatch(&pod, req.Namespace); len(ops) > 0 {
			dry := req.DryRun != nil && *req.DryRun
			if dry || s.ensureCABundle(r.Context(), key, req.Namespace) {
				b, _ := json.Marshal(ops)
				resp.PatchType = "JSONPatch"
				resp.Patch = base64.StdEncoding.EncodeToString(b)
			}
		}
	}
	writeJSON(w, admissionReview{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview", Response: resp})
}

// injectPatch returns the JSON patch adding the CA bundle and, for a pod
// labelled for real Google APIs, their DNS to pod in namespace ns, or nil
// when the pod opted out or has both already.
func injectPatch(pod *admitPod, ns string) []patchOp {
	if pod.Metadata.Labels[injectLabel] == "disabled" {
		return nil
	}
	ops := caPatch(pod)
	if pod.Metadata.Labels[googleAPIsLabel] == googleAPIsReal && pod.Spec.DNSPolicy != dnsPolicyNone {
		ops = append(ops,
			patchOp{Op: "add", Path: "/spec/dnsPolicy", Value: dnsPolicyNone},
			patchOp{Op: "add", Path: "/spec/dnsConfig", Value: googleAPIsDNSConfig(ns, pod.Spec.DNSConfig)})
	}
	return ops
}

// googleAPIsDNSConfig is the dnsConfig of a pod in ns labelled for real
// Google APIs: the googleapis resolver and the cluster's search path, with
// the pod's own dnsConfig merged in as kubelet merges it under
// ClusterFirst (its nameservers and searches after, its options winning).
func googleAPIsDNSConfig(ns string, own *dnsConfig) dnsConfig {
	five := "5"
	c := dnsConfig{
		Nameservers: []string{googleAPIsDNSIP},
		Searches:    []string{ns + ".svc." + clusterDomain, "svc." + clusterDomain, clusterDomain},
		Options:     []dnsOption{{Name: "ndots", Value: &five}},
	}
	if own == nil {
		return c
	}
	c.Nameservers = append(c.Nameservers, own.Nameservers...)
	c.Searches = append(c.Searches, own.Searches...)
	for _, o := range own.Options {
		if o.Name == "ndots" {
			c.Options = c.Options[1:]
		}
	}
	c.Options = append(c.Options, own.Options...)
	return c
}

// caPatch returns the JSON patch adding the CA bundle to pod, or nil when
// it is already injected.
func caPatch(pod *admitPod) []patchOp {
	for _, v := range pod.Spec.Volumes {
		if v.Name == caBundleName {
			return nil
		}
	}
	var ops []patchOp
	vol := map[string]any{
		"name": caBundleName,
		"configMap": map[string]any{
			"name": caBundleName, "optional": true,
			"items": []map[string]any{{"key": caBundleKey, "path": caBundleKey}},
		},
	}
	if pod.Spec.Volumes == nil {
		ops = append(ops, patchOp{Op: "add", Path: "/spec/volumes", Value: []any{vol}})
	} else {
		ops = append(ops, patchOp{Op: "add", Path: "/spec/volumes/-", Value: vol})
	}
	for i, c := range pod.Spec.InitContainers {
		ops = append(ops, containerPatch("/spec/initContainers/"+strconv.Itoa(i), c)...)
	}
	for i, c := range pod.Spec.Containers {
		ops = append(ops, containerPatch("/spec/containers/"+strconv.Itoa(i), c)...)
	}
	return ops
}

// containerPatch mounts the bundle and sets the CA environment variables.
func containerPatch(base string, c admitContainer) []patchOp {
	taken := map[string]bool{}
	for _, m := range c.VolumeMounts {
		taken[strings.TrimSuffix(m.MountPath, "/")] = true
	}
	ownDir := !taken[caBundleDir] && !taken["/etc/gcpemu"]
	var mounts []any
	if ownDir {
		mounts = append(mounts, map[string]any{"name": caBundleName, "mountPath": caBundleDir, "readOnly": true})
	}
	if !taken[systemBundlePath] && !taken["/etc/ssl/certs"] && !taken["/etc/ssl"] {
		mounts = append(mounts, map[string]any{"name": caBundleName, "mountPath": systemBundlePath, "subPath": caBundleKey, "readOnly": true})
	}
	var ops []patchOp
	if len(mounts) > 0 {
		if c.VolumeMounts == nil {
			ops = append(ops, patchOp{Op: "add", Path: base + "/volumeMounts", Value: mounts})
		} else {
			for _, m := range mounts {
				ops = append(ops, patchOp{Op: "add", Path: base + "/volumeMounts/-", Value: m})
			}
		}
	}
	set := map[string]bool{}
	for _, e := range c.Env {
		set[e.Name] = true
	}
	var env []any
	add := func(n, v string) {
		if !set[n] {
			env = append(env, map[string]any{"name": n, "value": v})
		}
	}
	if ownDir { // otherwise the bundle path is not ours
		for _, n := range caEnvVars {
			add(n, caBundlePath)
		}
	}
	add(metadataHostEnv, metadataIP)
	if len(env) > 0 {
		if c.Env == nil {
			ops = append(ops, patchOp{Op: "add", Path: base + "/env", Value: env})
		} else {
			for _, e := range env {
				ops = append(ops, patchOp{Op: "add", Path: base + "/env/-", Value: e})
			}
		}
	}
	return ops
}

// bundleCache memoizes the bundle for the current CA.
var bundleCache struct {
	sync.Mutex
	ca, bundle []byte
}

func (s *Service) caBundle() []byte {
	ca := s.env.CA.PEM()
	bundleCache.Lock()
	defer bundleCache.Unlock()
	if !bytes.Equal(bundleCache.ca, ca) {
		bundleCache.ca, bundleCache.bundle = ca, trust.Bundle(ca)
	}
	return bundleCache.bundle
}

// ensureCABundle creates or refreshes the namespace's bundle ConfigMap and
// reports whether it is in place.
func (s *Service) ensureCABundle(ctx context.Context, key, ns string) bool {
	kc, err := s.kube(key)
	if err != nil || ns == "" {
		return false
	}
	want := string(s.caBundle())
	path := "/api/v1/namespaces/" + ns + "/configmaps"
	var cm struct {
		Data map[string]string `json:"data"`
	}
	err = kc.get(ctx, path+"/"+caBundleName, &cm)
	switch {
	case isKubeNotFound(err):
		err = kc.do(ctx, http.MethodPost, path, "", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": caBundleName, "labels": map[string]string{"app.kubernetes.io/managed-by": "gcpemu"}},
			"data":     map[string]string{caBundleKey: want},
		}, nil)
		var ke *kubeError
		if err != nil && errors.As(err, &ke) && ke.Status == http.StatusConflict {
			err = nil // created concurrently
		}
	case err == nil && cm.Data[caBundleKey] != want:
		err = kc.mergePatch(ctx, path+"/"+caBundleName, map[string]any{"data": map[string]string{caBundleKey: want}})
	}
	if err != nil {
		s.env.Log.Warn("gke: CA bundle ConfigMap", "namespace", ns, "err", err)
		return false
	}
	return true
}
