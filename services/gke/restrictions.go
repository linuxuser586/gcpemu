package gke

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Workload restrictions on Autopilot clusters (FR-GKE-016).
//
// An Autopilot cluster gets three webhooks served by the emulator through
// the frontend, as the CA injector is (cainject.go): GKE Warden's
// validating webhook on new pods outside kube-system, the same on writes
// to kube-system by anyone but the cluster's own components, and a
// mutating webhook setting new pods' requests. Refusals carry Warden's
// message and constraint names, and changed pods GKE's warning. A Standard
// cluster gets none of them.
//
// No Recording of an Autopilot cluster exists yet (ADR 0023), so the
// messages and request values are GKE's as documented and observed, not
// checked against one.

// hostPathAllowed is the one host path prefix Autopilot lets pods mount,
// read-only.
const hostPathAllowed = "/var/log/"

// autopilotCaps are the capabilities Autopilot lets containers add;
// NET_ADMIN joins them with workloadPolicyConfig.allowNetAdmin.
var autopilotCaps = []string{"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD",
	"NET_BIND_SERVICE", "NET_RAW", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT", "SYS_PTRACE"}

// allowedCaps lists the capabilities a cluster allows, as GKE's message
// does.
func allowedCaps(netAdmin bool) string {
	caps := slices.Clone(autopilotCaps)
	if netAdmin {
		caps = append(caps, "NET_ADMIN")
		slices.Sort(caps)
	}
	return strings.Join(caps, ",")
}

// restrictedPod is the subset of a Pod the Workload restrictions read.
type restrictedPod struct {
	Spec struct {
		HostNetwork bool `json:"hostNetwork"`
		HostPID     bool `json:"hostPID"`
		HostIPC     bool `json:"hostIPC"`
		Volumes     []struct {
			Name     string `json:"name"`
			HostPath *struct {
				Path string `json:"path"`
			} `json:"hostPath"`
		} `json:"volumes"`
		InitContainers []restrictedContainer `json:"initContainers"`
		Containers     []restrictedContainer `json:"containers"`
	} `json:"spec"`
}

type restrictedContainer struct {
	Name  string `json:"name"`
	Ports []struct {
		HostPort int32 `json:"hostPort"`
	} `json:"ports"`
	VolumeMounts []struct {
		Name     string `json:"name"`
		ReadOnly bool   `json:"readOnly"`
	} `json:"volumeMounts"`
	SecurityContext *struct {
		Privileged   *bool `json:"privileged"`
		Capabilities *struct {
			Add []string `json:"add"`
		} `json:"capabilities"`
	} `json:"securityContext"`
}

// restrictionViolations returns what Autopilot refuses in pod, keyed by GKE's
// constraint name, or nil.
func restrictionViolations(pod *restrictedPod, allowNetAdmin bool) map[string][]string {
	v := map[string][]string{}
	add := func(constraint, msg string) { v[constraint] = append(v[constraint], msg) }
	for _, ns := range []struct {
		name string
		on   bool
	}{{"hostNetwork", pod.Spec.HostNetwork}, {"hostPID", pod.Spec.HostPID}, {"hostIPC", pod.Spec.HostIPC}} {
		if ns.on {
			add("autogke-disallow-hostnamespaces", "enabling "+ns.name+" is not allowed in Autopilot.")
		}
	}
	hostPaths := map[string]string{}
	for _, vol := range pod.Spec.Volumes {
		if vol.HostPath != nil {
			hostPaths[vol.Name] = vol.HostPath.Path
		}
	}
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		if sc := c.SecurityContext; sc != nil && sc.Privileged != nil && *sc.Privileged {
			add("autogke-disallow-privilege", "container "+c.Name+" is privileged; not allowed in Autopilot")
		}
		if sc := c.SecurityContext; sc != nil && sc.Capabilities != nil {
			for _, cp := range sc.Capabilities.Add {
				if n := strings.TrimPrefix(cp, "CAP_"); !slices.Contains(autopilotCaps, n) && (n != "NET_ADMIN" || !allowNetAdmin) {
					add("autogke-default-linux-capabilities", fmt.Sprintf(
						"linux capability '%s' on container '%s' not allowed; Autopilot only allows the capabilities: '%s'.", cp, c.Name, allowedCaps(allowNetAdmin)))
				}
			}
		}
		for _, m := range c.VolumeMounts {
			hp, ok := hostPaths[m.Name]
			switch {
			case !ok:
			case !strings.HasPrefix(path.Clean(hp)+"/", hostPathAllowed):
				add("autogke-no-write-mode-hostpath", fmt.Sprintf(
					"hostPath volume %s used in container %s uses path %s which is not allowed in Autopilot. Allowed path prefixes for hostPath volumes are: [%s].", m.Name, c.Name, hp, hostPathAllowed))
			case !m.ReadOnly:
				add("autogke-no-write-mode-hostpath", fmt.Sprintf(
					"hostPath volume %s in container %s is used in write mode; disallowed in Autopilot. Allowed read-only hostPath paths: [%s].", m.Name, c.Name, hostPathAllowed))
			}
		}
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				add("autogke-no-host-port", fmt.Sprintf("container %s specifies host port %d; disallowed in Autopilot", c.Name, p.HostPort))
			}
		}
	}
	return v
}

// ---- resource requests ----

// resourcePod is the subset of a Pod the request mutation reads.
type resourcePod struct {
	Metadata struct {
		Name         string `json:"name"`
		GenerateName string `json:"generateName"`
	} `json:"metadata"`
	Spec struct {
		InitContainers []resourceContainer `json:"initContainers"`
		Containers     []resourceContainer `json:"containers"`
	} `json:"spec"`
}

type resourceContainer struct {
	Name          string             `json:"name"`
	RestartPolicy string             `json:"restartPolicy"`
	Resources     containerResources `json:"resources"`
}

// containerResources is a container's resources, every field of which a
// patch rewrites.
type containerResources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
	Claims   json.RawMessage   `json:"claims,omitempty"`
}

// autopilotDefaults are the requests Autopilot gives a container that
// sets none, in the order GKE's warning names them.
var autopilotDefaults = []struct{ name, value string }{
	{"cpu", "500m"}, {"ephemeral-storage", "1Gi"}, {"memory", "2Gi"},
}

// autopilotResources sets pod's requests as Autopilot does on a cluster
// with bursting and returns the JSON patch making the same changes and
// GKE's warning, or nil and "" when the pod already meets the rules.
func autopilotResources(pod *resourcePod, ns string) ([]patchOp, string) {
	changed := map[string]bool{} // "init/i" or "app/i"
	var parts []string
	for _, d := range autopilotDefaults {
		var names []string
		eachContainer(pod, func(key string, c *resourceContainer, sidecarOrApp bool) {
			if !sidecarOrApp {
				return
			}
			if _, ok := c.Resources.Requests[d.name]; ok {
				return
			}
			if c.Resources.Requests == nil {
				c.Resources.Requests = map[string]string{}
			}
			c.Resources.Requests[d.name] = d.value
			changed[key] = true
			names = append(names, c.Name)
		})
		if len(names) > 0 {
			parts = append(parts, fmt.Sprintf("defaulted unspecified '%s' resource for containers %v", d.name, names))
		}
	}
	defaulted := len(parts) > 0
	var defaults string
	if defaulted {
		defaults = strings.Join(parts, ", ")
		parts = []string{defaults}
	}
	if adjustRequests(pod) {
		changed["containers/0"] = true
		parts = append(parts, fmt.Sprintf("adjusted resources to meet requirements for containers [%s]", pod.Spec.Containers[0].Name))
	}
	if len(changed) == 0 {
		return nil, ""
	}
	var ops []patchOp
	eachContainer(pod, func(key string, c *resourceContainer, _ bool) {
		if changed[key] {
			ops = append(ops, patchOp{Op: "add", Path: "/spec/" + key + "/resources", Value: c.Resources})
		}
	})
	see := "http://g.co/gke/autopilot-resources"
	if defaulted {
		see = "http://g.co/gke/autopilot-defaults"
	}
	name := cmp.Or(pod.Metadata.Name, pod.Metadata.GenerateName)
	return ops, fmt.Sprintf("autopilot-default-resources-mutator:Autopilot updated Pod %s/%s: %s (see %s).", ns, name, strings.Join(parts, "; "), see)
}

// Autopilot's per-Pod minimums on clusters with bursting, and the range of
// memory per vCPU it keeps a Pod in.
const (
	autopilotMinCPUMilli  = 50
	autopilotMinMem       = 52 << 20
	autopilotMinMemPerCPU = 1 << 30
	autopilotMaxMemPerCPU = 13 << 29 // 6.5 GiB
)

// adjustRequests raises the Pod's CPU and memory requests to Autopilot's
// minimums and memory-per-vCPU range, counting them as the scheduler does
// (podRequest), and reports whether it did. Raising a resource adds the
// shortfall to the first container, and its limit with it if the limit
// would be lower.
func adjustRequests(pod *resourcePod) bool {
	if len(pod.Spec.Containers) == 0 {
		return false
	}
	adjusted := false
	raise := func(resource string, to int64) {
		if podRequest(pod, resource) >= to {
			return
		}
		// An init container may need more than the containers and
		// sidecars; they must reach to on their own.
		short := to - runningRequest(pod, resource)
		c := &pod.Spec.Containers[0]
		v := quantityOf(resource, c.Resources.Requests[resource]) + short
		c.Resources.Requests[resource] = formatQuantity(resource, v)
		if l, ok := c.Resources.Limits[resource]; ok && quantityOf(resource, l) < v {
			c.Resources.Limits[resource] = c.Resources.Requests[resource]
		}
		adjusted = true
	}
	if pod.Spec.Containers[0].Resources.Requests == nil {
		pod.Spec.Containers[0].Resources.Requests = map[string]string{}
	}
	raise("cpu", autopilotMinCPUMilli)
	raise("memory", autopilotMinMem)
	cpu, mem := podRequest(pod, "cpu"), podRequest(pod, "memory")
	raise("memory", ceilDiv(cpu*autopilotMinMemPerCPU, 1000))
	raise("cpu", ceilDiv(mem*1000, autopilotMaxMemPerCPU))
	return adjusted
}

// podRequest is the Pod's request for resource (CPU in millicores, memory
// in bytes) as the scheduler counts it: its containers and sidecars, or
// more if an init container, with the sidecars started before it, needs
// more.
func podRequest(pod *resourcePod, resource string) int64 {
	var sidecars, init int64
	for _, c := range pod.Spec.InitContainers {
		r := quantityOf(resource, c.Resources.Requests[resource])
		if c.RestartPolicy == "Always" {
			sidecars += r
			init = max(init, sidecars)
			continue
		}
		init = max(init, sidecars+r)
	}
	return max(init, runningRequest(pod, resource))
}

// runningRequest is the Pod's request for resource while its containers
// run: theirs and its sidecars'.
func runningRequest(pod *resourcePod, resource string) int64 {
	var r int64
	eachContainer(pod, func(_ string, c *resourceContainer, sidecarOrApp bool) {
		if sidecarOrApp {
			r += quantityOf(resource, c.Resources.Requests[resource])
		}
	})
	return r
}

// quantityOf reads a quantity of resource: CPU in millicores, anything
// else in units.
func quantityOf(resource, q string) int64 {
	if q == "" {
		return 0
	}
	if resource == "cpu" {
		return milli(q)
	}
	return units(q)
}

// formatQuantity writes v, as quantityOf reads it, in the form Kubernetes
// stores it.
func formatQuantity(resource string, v int64) string {
	switch {
	case resource == "cpu" && v%1000 == 0:
		return strconv.FormatInt(v/1000, 10)
	case resource == "cpu":
		return strconv.FormatInt(v, 10) + "m"
	case v%(1<<30) == 0:
		return strconv.FormatInt(v>>30, 10) + "Gi"
	case v%(1<<20) == 0:
		return strconv.FormatInt(v>>20, 10) + "Mi"
	}
	return strconv.FormatInt(v, 10)
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// eachContainer calls f with each init container and then each container,
// its patch path under /spec/ ("initContainers/0") and whether it runs
// alongside the app (a container or a sidecar) rather than before it.
func eachContainer(pod *resourcePod, f func(key string, c *resourceContainer, sidecarOrApp bool)) {
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		f("initContainers/"+strconv.Itoa(i), c, c.RestartPolicy == "Always")
	}
	for i := range pod.Spec.Containers {
		f("containers/"+strconv.Itoa(i), &pod.Spec.Containers[i], true)
	}
}

// ---- admission ----

// The hooks of an Autopilot cluster's webhooks.
const (
	hookRestrict  = "restrict"  // refuses what Autopilot refuses
	hookResources = "resources" // sets a new pod's requests
)

// serveAutopilotAdmission answers an Autopilot cluster's admission
// webhooks: hookRestrict refuses what Autopilot refuses, with GKE
// Warden's message, and hookResources changes a new pod's requests. A
// Standard cluster's requests, which never come but for a mode change
// the API refuses, are admitted unchanged.
func (s *Service) serveAutopilotAdmission(w http.ResponseWriter, r *http.Request, key, hook string) {
	var ar admissionReview
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil || ar.Request == nil {
		http.Error(w, "bad AdmissionReview", http.StatusBadRequest)
		return
	}
	req := ar.Request
	resp := &admissionResponse{UID: req.UID, Allowed: true}
	rec, err := s.loadKey(key)
	if err == nil && autopilotOn(rec.cluster()) {
		isPod := req.Resource.Group == "" && req.Resource.Resource == "pods" && req.Operation == "CREATE" && req.Namespace != kubeSystem
		switch {
		case hook == hookRestrict && req.Namespace == kubeSystem && !systemWriter(req):
			resp.Allowed, resp.Status = false, refusal(req, map[string][]string{"managed-namespaces-limitation": {
				fmt.Sprintf("the namespace %q is managed and the request's verb %q is denied", req.Namespace, strings.ToLower(req.Operation))}})
		case hook == hookRestrict && req.Namespace != kubeSystem:
			if pod, ok := podTemplate(req); ok {
				allowNetAdmin := rec.cluster().GetAutopilot().GetWorkloadPolicyConfig().GetAllowNetAdmin()
				if v := restrictionViolations(pod, allowNetAdmin); len(v) > 0 {
					resp.Allowed, resp.Status = false, refusal(req, v)
				}
			}
		case hook == hookResources && isPod:
			var pod resourcePod
			if json.Unmarshal(req.Object, &pod) == nil {
				if ops, warning := autopilotResources(&pod, req.Namespace); len(ops) > 0 {
					b, _ := json.Marshal(ops)
					resp.PatchType, resp.Patch, resp.Warnings = "JSONPatch", base64.StdEncoding.EncodeToString(b), []string{warning}
				}
			}
		}
	}
	writeJSON(w, admissionReview{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview", Response: resp})
}

// templated are the workloads whose pod template Autopilot checks, by
// API group and resource, with whether the template is a Job's.
var templated = map[[2]string]bool{
	{"", "replicationcontrollers"}: false, {"apps", "deployments"}: false, {"apps", "replicasets"}: false,
	{"apps", "statefulsets"}: false, {"apps", "daemonsets"}: false, {"batch", "jobs"}: false, {"batch", "cronjobs"}: true,
}

// podTemplate returns the pod a request creates, or a workload's pod
// template, which has a pod's shape.
func podTemplate(req *admissionRequest) (*restrictedPod, bool) {
	var pod restrictedPod
	if req.Resource.Group == "" && req.Resource.Resource == "pods" {
		return &pod, req.Operation == "CREATE" && json.Unmarshal(req.Object, &pod) == nil
	}
	job, ok := templated[[2]string{req.Resource.Group, req.Resource.Resource}]
	if !ok {
		return nil, false
	}
	var w struct {
		Spec struct {
			Template    *restrictedPod `json:"template"`
			JobTemplate struct {
				Spec struct {
					Template *restrictedPod `json:"template"`
				} `json:"spec"`
			} `json:"jobTemplate"`
		} `json:"spec"`
	}
	if json.Unmarshal(req.Object, &w) != nil {
		return nil, false
	}
	t := w.Spec.Template
	if job {
		t = w.Spec.JobTemplate.Spec.Template
	}
	return t, t != nil
}

const kubeSystem = "kube-system"

// systemWriter reports whether a write to kube-system is the cluster's
// own, which GKE's system stands for: a system user other than a service
// account of another namespace. system:admin is k3s's own controllers and
// the emulator, so the admin kubeconfig (kubeconfig.go), which has no GKE
// equivalent, writes there too; users authenticated as Google identities
// are refused.
func systemWriter(req *admissionRequest) bool {
	u := req.UserInfo.Username
	if strings.HasPrefix(u, "system:serviceaccount:") {
		return strings.HasPrefix(u, "system:serviceaccount:"+kubeSystem+":")
	}
	return strings.HasPrefix(u, "system:")
}

// refusal is GKE Warden's refusal of req for violations, keyed by
// constraint name.
func refusal(req *admissionRequest, violations map[string][]string) *admissionStatus {
	details := map[string][]string{}
	for k, v := range violations {
		details["[denied by "+k+"]"] = v
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(details)
	return &admissionStatus{
		Code:   http.StatusForbidden,
		Reason: "GKE Warden constraints violations",
		Message: fmt.Sprintf("GKE Warden rejected the request because it violates one or more constraints.\nViolations details: %s\nRequested by user: '%s', groups: '%s'.",
			strings.TrimSpace(b.String()), req.UserInfo.Username, strings.Join(req.UserInfo.Groups, ",")),
	}
}

// ---- webhooks ----

// wardenWebhook is GKE Warden's webhook name, which the API server puts
// in every refusal.
const wardenWebhook = "warden-validating.common-webhooks.networking.gke.io"

// Names of the webhook configurations an Autopilot cluster gets.
const (
	autopilotPodsName       = "gcpemu-autopilot-restrictions"
	autopilotNamespacesName = "gcpemu-autopilot-managed-namespaces"
	autopilotResourcesName  = "gcpemu-autopilot-resources"
)

// notSystemWriter is systemWriter's negation in CEL, so that the cluster's
// own writes to kube-system never wait on the emulator.
const notSystemWriter = `!request.userInfo.username.startsWith('system:') || ` +
	`(request.userInfo.username.startsWith('system:serviceaccount:') && !request.userInfo.username.startsWith('system:serviceaccount:kube-system:'))`

// installAutopilotWebhooks registers an Autopilot cluster's Workload
// restrictions, served by the emulator through the frontend as the CA
// injector is. They fail closed, as GKE's do: while the emulator is
// unreachable, pods cannot be created and users cannot write to
// kube-system. Errors are logged.
func (s *Service) installAutopilotWebhooks(ctx context.Context, d *deps, key string) {
	rec, err := s.loadKey(key)
	if err != nil || !autopilotOn(rec.cluster()) {
		return
	}
	hooks, kc, ok := s.hookBase(ctx, d, key, "Workload restrictions are not applied")
	if !ok {
		s.env.Log.Warn("gke: the Autopilot cluster's Workload restrictions are not applied", "cluster", key)
		return
	}
	for _, o := range autopilotWebhooks(hooks, s.env.CA.PEM()) {
		coll := "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations"
		if o["kind"] == "MutatingWebhookConfiguration" {
			coll = "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations"
		}
		name := o["metadata"].(map[string]any)["name"].(string)
		if err := applyObject(ctx, kc, coll, name, o); err != nil {
			s.env.Log.Warn("gke: registering an Autopilot webhook failed", "cluster", key, "webhook", name, "err", err)
		}
	}
}

// autopilotWebhooks are the webhook configurations of the Workload
// restrictions, with hooks the URL hookBase returns.
func autopilotWebhooks(hooks string, caPEM []byte) []map[string]any {
	inKubeSystem := func(op string) map[string]any {
		return map[string]any{"matchExpressions": []map[string]any{{"key": "kubernetes.io/metadata.name", "operator": op, "values": []string{kubeSystem}}}}
	}
	podCreate := []map[string]any{{
		"apiGroups": []string{""}, "apiVersions": []string{"v1"},
		"operations": []string{"CREATE"}, "resources": []string{"pods"}, "scope": "Namespaced",
	}}
	webhook := func(name, hook string, rules []map[string]any, ns map[string]any) map[string]any {
		return map[string]any{
			"name":                    name,
			"admissionReviewVersions": []string{"v1"},
			"sideEffects":             "None",
			"failurePolicy":           "Fail",
			"timeoutSeconds":          10,
			"clientConfig":            map[string]any{"url": hooks + hook, "caBundle": base64.StdEncoding.EncodeToString(caPEM)},
			"rules":                   rules,
			"namespaceSelector":       ns,
		}
	}
	config := func(kind, name string, wh map[string]any) map[string]any {
		return map[string]any{
			"apiVersion": "admissionregistration.k8s.io/v1",
			"kind":       kind,
			"metadata":   map[string]any{"name": name, "labels": map[string]string{"app.kubernetes.io/managed-by": "gcpemu"}},
			"webhooks":   []map[string]any{wh},
		}
	}
	workloads := map[string][]string{}
	for gr := range templated {
		workloads[gr[0]] = append(workloads[gr[0]], gr[1])
	}
	pods := slices.Clone(podCreate)
	for _, g := range slices.Sorted(maps.Keys(workloads)) {
		slices.Sort(workloads[g])
		pods = append(pods, map[string]any{
			"apiGroups": []string{g}, "apiVersions": []string{"*"},
			"operations": []string{"CREATE", "UPDATE"}, "resources": workloads[g], "scope": "Namespaced",
		})
	}
	restrict := webhook(wardenWebhook, hookRestrict, pods, inKubeSystem("NotIn"))
	namespaces := webhook(wardenWebhook, hookRestrict, []map[string]any{{
		"apiGroups": []string{"*"}, "apiVersions": []string{"*"},
		"operations": []string{"CREATE", "UPDATE"}, "resources": []string{"*/*"}, "scope": "Namespaced",
	}}, inKubeSystem("In"))
	namespaces["matchConditions"] = []map[string]any{{"name": "not-system", "expression": notSystemWriter}}
	resources := webhook("autopilot-default-resources-mutator.gcpemu.dev", hookResources, podCreate, inKubeSystem("NotIn"))
	// Rerun after webhooks that add containers, such as Istio's injector.
	resources["reinvocationPolicy"] = "IfNeeded"
	return []map[string]any{
		config("ValidatingWebhookConfiguration", autopilotPodsName, restrict),
		config("ValidatingWebhookConfiguration", autopilotNamespacesName, namespaces),
		config("MutatingWebhookConfiguration", autopilotResourcesName, resources),
	}
}
