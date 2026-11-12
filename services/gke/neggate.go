package gke

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// The NEG readiness gate (ADR 0071). GKE's NEG controller gives pods
// behind a NEG the readiness gate cloud.google.com/load-balancer-neg-ready
// and holds the pod condition of that type until the load balancer finds
// the pod's endpoint HEALTHY, so a rollout cannot run ahead of the load
// balancer. Here a mutating webhook, served by the Instance through the
// Google front end like the CA injector's, adds the gate to new pods, and
// the NEG sync (neg.go) sets the condition, by the rule of GKE's
// readiness reflector (kubernetes/ingress-gce pkg/neg/readiness).

const (
	negReadinessGate = "cloud.google.com/load-balancer-neg-ready"
	// negGateTimeout is ingress-gce's unreadyTimeout, from the pod's
	// creation.
	negGateTimeout = 10 * time.Minute

	negReadyReason          = "LoadBalancerNegReady"
	negReadyTimedOutReason  = "LoadBalancerNegTimeout"
	negReadyUnhealthChecked = "LoadBalancerNegWithoutHealthCheck"
)

// negGateInput is what the gate rule decides on for one gated pod.
type negGateInput struct {
	// ContainersReady is the pod's ContainersReady condition.
	ContainersReady bool
	// NEGs are the NEGs the controller owns that cover the pod: those of
	// the Services that select it.
	NEGs []negGateNEG
	// Created is the pod's creation time, and Now the time of the sync.
	Created, Now time.Time
}

// negGateNEG is one NEG covering the pod: whether the pod's endpoint is in
// it, and the endpoint's state in each backend service with a health check
// that uses the NEG.
type negGateNEG struct {
	Name        string
	HasEndpoint bool
	Health      []emu.NEGBackendHealth
}

// negGateResult is the condition to set: none unless Ready, as the
// condition never goes from True back to False.
type negGateResult struct {
	Ready           bool
	Reason, Message string
}

// negGateCondition is the gate rule: True once the endpoint is HEALTHY in
// any health-checked backend service using one of its NEGs; at once for an
// endpoint in a NEG no such backend service uses; once containers ready
// for a pod no owned NEG covers; and otherwise after negGateTimeout.
func negGateCondition(in negGateInput) negGateResult {
	marking := fmt.Sprintf(" Marking condition %q to True.", negReadinessGate)
	for _, n := range in.NEGs {
		for _, h := range n.Health {
			if h.State == "HEALTHY" {
				return negGateResult{true, negReadyReason,
					fmt.Sprintf("Pod has become Healthy in NEG %q attached to BackendService %q.", n.Name, h.BackendService) + marking}
			}
		}
	}
	for _, n := range in.NEGs {
		if n.HasEndpoint && len(n.Health) == 0 {
			return negGateResult{true, negReadyUnhealthChecked,
				fmt.Sprintf("Pod is in NEG %q. NEG is not attached to any BackendService with health checking.", n.Name) + marking}
		}
	}
	if len(in.NEGs) == 0 {
		if in.ContainersReady {
			return negGateResult{true, negReadyReason, "Pod does not belong to any NEG." + marking}
		}
		return negGateResult{}
	}
	if in.Now.After(in.Created.Add(negGateTimeout)) {
		names := make([]string, 0, len(in.NEGs))
		for _, n := range in.NEGs {
			names = append(names, n.Name)
		}
		return negGateResult{true, negReadyTimedOutReason,
			fmt.Sprintf("Timeout waiting for pod to become healthy in at least one of the NEG(s): %v.", names) + marking}
	}
	return negGateResult{}
}

// gatedPod is a pod as the gate reads it.
type gatedPod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		ReadinessGates []readinessGate `json:"readinessGates,omitempty"`
	} `json:"spec"`
	Status struct {
		PodIP      string         `json:"podIP"`
		Conditions []podCondition `json:"conditions,omitempty"`
	} `json:"status"`
}

type readinessGate struct {
	ConditionType string `json:"conditionType"`
}

type podCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// gated reports whether the pod has the gate.
func (p *gatedPod) gated() bool {
	for _, g := range p.Spec.ReadinessGates {
		if g.ConditionType == negReadinessGate {
			return true
		}
	}
	return false
}

// condition returns the status of the pod condition typ, or "".
func (p *gatedPod) condition(typ string) string {
	for _, c := range p.Status.Conditions {
		if c.Type == typ {
			return c.Status
		}
	}
	return ""
}

// negGated reports whether the NEG controller gates the pods svc selects:
// its NEG annotation exposes ports, or a load balancer uses its NEGs
// (lbSvcs, by namespace/name), as syncNEGs treats them.
func negGated(svc *kubeService, lbSvcs map[string]bool) bool {
	if len(svc.Spec.Selector) == 0 {
		return false
	}
	if lbSvcs[svc.Metadata.Namespace+"/"+svc.Metadata.Name] {
		return true
	}
	var ann negAnnotation
	return json.Unmarshal([]byte(svc.Metadata.Annotations[annoNEG]), &ann) == nil && len(ann.ExposedPorts) > 0
}

// selects reports whether svc's selector selects labels.
func selects(svc *kubeService, labels map[string]string) bool {
	if len(svc.Spec.Selector) == 0 {
		return false
	}
	for k, v := range svc.Spec.Selector {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// negGatePatch returns the JSON patch adding the gate to a new pod in
// namespace ns, given the Services of ns and the load balancers' NEG
// Services (lbSvcs, by namespace/name), or nil when no gated Service
// selects it, it is in kube-system or it has the gate.
func negGatePatch(pod *gatedPod, ns string, svcs []kubeService, lbSvcs map[string]bool) []patchOp {
	if ns == kubeSystem || pod.gated() {
		return nil
	}
	for i := range svcs {
		if svcs[i].Metadata.Namespace == ns && negGated(&svcs[i], lbSvcs) && selects(&svcs[i], pod.Metadata.Labels) {
			gate := readinessGate{ConditionType: negReadinessGate}
			if len(pod.Spec.ReadinessGates) == 0 {
				return []patchOp{{Op: "add", Path: "/spec/readinessGates", Value: []readinessGate{gate}}}
			}
			return []patchOp{{Op: "add", Path: "/spec/readinessGates/-", Value: gate}}
		}
	}
	return nil
}

// negCover is a NEG covering a gated pod: in is set when the pod's
// endpoint, member, is in it.
type negCover struct {
	name   string
	member negMember
	in     bool
}

// negHealth returns the load balancer's view of NEG endpoints, or nil
// without one, when every NEG counts as having no health check.
func (s *Service) negHealth() emu.NEGHealth {
	if h, ok := s.testLB.(emu.NEGHealth); ok {
		return h
	}
	if p, ok := s.env.Lookup("lb"); ok {
		if h, ok := p.(emu.NEGHealth); ok {
			return h
		}
	}
	return nil
}

// syncNEGGates sets the gate's condition True on each gated pod that
// meets the gate rule, given the NEGs covering each (covers, by
// namespace/name). A pod whose condition is True, or that is terminating,
// is left as it is.
func (s *Service) syncNEGGates(ctx context.Context, kc *kubeClient, project string, pods map[string]*gatedPod, covers map[string][]negCover) []error {
	health := s.negHealth()
	now := time.Now()
	var errs []error
	for pk, p := range pods {
		if !p.gated() || p.condition(negReadinessGate) == "True" || p.Metadata.DeletionTimestamp != nil {
			continue
		}
		created, _ := time.Parse(time.RFC3339, p.Metadata.CreationTimestamp)
		in := negGateInput{ContainersReady: p.condition("ContainersReady") == "True", Created: created, Now: now}
		for _, c := range covers[pk] {
			n := negGateNEG{Name: c.name, HasEndpoint: c.in}
			if c.in && health != nil {
				n.Health = health.NEGEndpointHealth(ctx, project, c.member.zone, c.name, c.member.ip, c.member.port)
			}
			in.NEGs = append(in.NEGs, n)
		}
		res := negGateCondition(in)
		if !res.Ready {
			continue
		}
		cond := podCondition{Type: negReadinessGate, Status: "True", Reason: res.Reason, Message: res.Message,
			LastTransitionTime: now.UTC().Format(time.RFC3339)}
		path := "/api/v1/namespaces/" + p.Metadata.Namespace + "/pods/" + p.Metadata.Name + "/status"
		if err := kc.strategicPatch(ctx, path, map[string]any{"status": map[string]any{"conditions": []podCondition{cond}}}); err != nil && !isKubeNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errs
}

const (
	hookNEGGate = "neggate" // adds the NEG readiness gate to a new pod
	negGateName = "gcpemu-neg-readiness-gate"
)

// installNEGGateWebhook registers (or updates) the webhook adding the NEG
// readiness gate in a running cluster. It fails open: while the Instance
// is unreachable, pods are created without the gate. Errors are logged.
func (s *Service) installNEGGateWebhook(ctx context.Context, d *deps, key string) {
	hooks, kc, ok := s.hookBase(ctx, d, key, "pods behind NEGs get no readiness gate")
	if !ok {
		return
	}
	if err := applyObject(ctx, kc, "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations", negGateName,
		negGateWebhook(hooks+hookNEGGate, s.env.CA.PEM())); err != nil {
		s.env.Log.Warn("gke: registering the NEG readiness gate webhook failed", "cluster", key, "err", err)
	}
}

// negGateWebhook is the MutatingWebhookConfiguration, on new pods outside
// kube-system.
func negGateWebhook(url string, caPEM []byte) map[string]any {
	return map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "MutatingWebhookConfiguration",
		"metadata": map[string]any{
			"name":   negGateName,
			"labels": map[string]string{"app.kubernetes.io/managed-by": "gcpemu"},
		},
		"webhooks": []map[string]any{{
			"name":                    "neg-readiness-gate.gke.gcpemu.dev",
			"admissionReviewVersions": []string{"v1"},
			"sideEffects":             "None",
			"failurePolicy":           "Ignore",
			"timeoutSeconds":          10,
			"clientConfig":            map[string]any{"url": url, "caBundle": base64.StdEncoding.EncodeToString(caPEM)},
			"rules": []map[string]any{{
				"apiGroups": []string{""}, "apiVersions": []string{"v1"},
				"operations": []string{"CREATE"}, "resources": []string{"pods"}, "scope": "Namespaced",
			}},
			"namespaceSelector": map[string]any{"matchExpressions": []map[string]any{
				{"key": "kubernetes.io/metadata.name", "operator": "NotIn", "values": []string{kubeSystem}},
			}},
		}},
	}
}

// serveNEGGate answers the NEG readiness gate webhook. When the Services
// cannot be read, the pod is admitted unchanged.
func (s *Service) serveNEGGate(w http.ResponseWriter, r *http.Request, key string) {
	var ar admissionReview
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil || ar.Request == nil {
		http.Error(w, "bad AdmissionReview", http.StatusBadRequest)
		return
	}
	req := ar.Request
	resp := &admissionResponse{UID: req.UID, Allowed: true}
	var pod gatedPod
	if req.Operation == "CREATE" && json.Unmarshal(req.Object, &pod) == nil {
		var svcs kubeList[kubeService]
		kc, err := s.kube(key)
		if err == nil {
			err = kc.get(r.Context(), "/api/v1/namespaces/"+req.Namespace+"/services", &svcs)
		}
		if err != nil {
			s.env.Log.Warn("gke: reading Services failed; the pod gets no NEG readiness gate", "cluster", key, "namespace", req.Namespace, "err", err)
		} else if ops := negGatePatch(&pod, req.Namespace, svcs.Items, s.rtFor(key).negLBServices()); len(ops) > 0 {
			b, _ := json.Marshal(ops)
			resp.PatchType, resp.Patch = "JSONPatch", base64.StdEncoding.EncodeToString(b)
		}
	}
	writeJSON(w, admissionReview{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview", Response: resp})
}
