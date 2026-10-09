package gke

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Emulator-internal endpoints under /container/_emu/ (not part of the
// container API):
//
//	POST /_emu/hooks/{clusterId}/{secret}/authn   TokenReview webhook
//	POST /_emu/hooks/{clusterId}/{secret}/authz   SubjectAccessReview webhook
//	POST /_emu/hooks/{clusterId}/{secret}/admit   CA injection admission webhook (via the frontend, HTTPS)
//	GET  /_emu/node/{clusterId}/{secret}/{node}/token        node SA token
//	*    /_emu/node/{clusterId}/{secret}/{node}/md/{ip}/...  metadata server
//	GET  /_emu/kubeconfig[?cluster=projects/P/locations/L/clusters/C]
//
// Hook and node URLs carry the cluster's secret; the kubeconfig endpoint
// requires container.clusters.getCredentials.

func (s *Service) internalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/_emu/"), "/")
		switch {
		case len(segs) == 1 && segs[0] == "kubeconfig":
			s.serveKubeconfig(w, r)
		case len(segs) == 4 && segs[0] == "hooks":
			key, ok := s.authorizeHook(segs[1], segs[2])
			if !ok {
				http.NotFound(w, r)
				return
			}
			switch segs[3] {
			case "authn":
				s.serveTokenReview(w, r, key)
			case "authz":
				s.serveSubjectAccessReview(w, r, key)
			case "admit":
				s.servePodAdmission(w, r, key)
			default:
				http.NotFound(w, r)
			}
		case len(segs) >= 5 && segs[0] == "node":
			key, ok := s.authorizeHook(segs[1], segs[2])
			if !ok {
				http.NotFound(w, r)
				return
			}
			node := segs[3]
			switch {
			case len(segs) == 5 && segs[4] == "token":
				s.serveNodeToken(w, r, key, node)
			case len(segs) == 5 && segs[4] == "mount" && r.Method == http.MethodPost:
				s.serveProviderMount(w, r, key)
			case len(segs) >= 6 && segs[4] == "md":
				s.serveMetadata(w, r, key, node, segs[5], "/"+strings.Join(segs[6:], "/"))
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	})
}

// authorizeHook checks a cluster ID and secret and returns the cluster key.
func (s *Service) authorizeHook(id, secret string) (string, bool) {
	key, ok := s.keyOf(id)
	if !ok {
		return "", false
	}
	rec, err := s.loadKey(key)
	if err != nil {
		return "", false
	}
	return key, subtle.ConstantTimeCompare([]byte(rec.Int.Secret), []byte(secret)) == 1
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- authentication (FR-GKE-004) ----

type tokenReview struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Token     string   `json:"token"`
		Audiences []string `json:"audiences,omitempty"`
	} `json:"spec"`
	Status struct {
		Authenticated bool `json:"authenticated"`
		User          *struct {
			Username string   `json:"username"`
			UID      string   `json:"uid,omitempty"`
			Groups   []string `json:"groups,omitempty"`
		} `json:"user,omitempty"`
		Audiences []string `json:"audiences,omitempty"`
		Error     string   `json:"error,omitempty"`
	} `json:"status"`
}

// serveTokenReview maps an emulator IAM access token to its principal's
// email, which is the Kubernetes user name on GKE. Outside enforce mode an
// unknown token maps to the default principal, as on the gateway
// (FR-CORE-050/051).
func (s *Service) serveTokenReview(w http.ResponseWriter, r *http.Request, key string) {
	var tr tokenReview
	if err := json.NewDecoder(r.Body).Decode(&tr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, ok := s.env.Auth.Authenticate(r.Context(), tr.Spec.Token)
	if !ok && s.env.Auth.Mode() != config.IAMEnforce && tr.Spec.Token != "" {
		p, ok = emu.Principal(s.env.Config.DefaultPrincipal), true
	}
	tr.Status.Audiences = tr.Spec.Audiences
	tr.Spec.Token = ""
	if ok && p != "" {
		tr.Status.Authenticated = true
		tr.Status.User = &struct {
			Username string   `json:"username"`
			UID      string   `json:"uid,omitempty"`
			Groups   []string `json:"groups,omitempty"`
		}{Username: p.Email(), UID: string(p), Groups: []string{"system:authenticated"}}
	} else {
		tr.Status.Error = "invalid or expired emulator access token"
	}
	writeJSON(w, tr)
}

// ---- authorization (FR-GKE-004) ----

type subjectAccessReview struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		User               string            `json:"user"`
		Groups             []string          `json:"groups,omitempty"`
		UID                string            `json:"uid,omitempty"`
		ResourceAttributes *resourceAttrs    `json:"resourceAttributes,omitempty"`
		NonResourceAttrs   *nonResourceAttrs `json:"nonResourceAttributes,omitempty"`
	} `json:"spec"`
	Status struct {
		Allowed bool   `json:"allowed"`
		Denied  bool   `json:"denied,omitempty"`
		Reason  string `json:"reason,omitempty"`
	} `json:"status"`
}

type resourceAttrs struct {
	Namespace   string `json:"namespace,omitempty"`
	Verb        string `json:"verb"`
	Group       string `json:"group,omitempty"`
	Version     string `json:"version,omitempty"`
	Resource    string `json:"resource,omitempty"`
	Subresource string `json:"subresource,omitempty"`
	Name        string `json:"name,omitempty"`
}

type nonResourceAttrs struct {
	Path string `json:"path"`
	Verb string `json:"verb"`
}

// serveSubjectAccessReview grants a request when the caller's IAM roles
// include the matching container.* permission on the cluster (GKE's IAM
// authorizer); otherwise it has no opinion and RBAC's decision stands.
func (s *Service) serveSubjectAccessReview(w http.ResponseWriter, r *http.Request, key string) {
	var sar subjectAccessReview
	if err := json.NewDecoder(r.Body).Decode(&sar); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	user := sar.Spec.User
	perm := ""
	if ra := sar.Spec.ResourceAttributes; ra != nil {
		perm = k8sPermission(ra)
	}
	if perm != "" && user != "" && !strings.HasPrefix(user, "system:") && strings.Contains(user, "@") {
		p := principalFor(user, s.env.Config.DefaultPrincipal)
		segs := strings.SplitN(key, "/", 3)
		res := "//container.googleapis.com/projects/" + segs[0] + "/locations/" + segs[1] + "/clusters/" + segs[2]
		if err := s.env.Auth.Check(emu.WithPrincipal(r.Context(), p), perm, res); err == nil {
			sar.Status.Allowed = true
			sar.Status.Reason = "allowed by IAM permission " + perm
		} else {
			sar.Status.Reason = apierr.From(err).Message
		}
	}
	writeJSON(w, sar)
}

// principalFor turns a Kubernetes user name (an email) into an IAM
// principal.
func principalFor(user, defaultPrincipal string) emu.Principal {
	if strings.EqualFold(emu.Principal(defaultPrincipal).Email(), user) {
		return emu.Principal(defaultPrincipal)
	}
	if strings.HasSuffix(user, ".gserviceaccount.com") {
		return emu.Principal("serviceAccount:" + user)
	}
	return emu.Principal("user:" + user)
}

// k8sResources maps lower-case Kubernetes resources to the camel-case
// names GKE's container.* permissions use.
var k8sResources = map[string]string{
	"apiservices": "apiServices", "certificatesigningrequests": "certificateSigningRequests",
	"clusterrolebindings": "clusterRoleBindings", "clusterroles": "clusterRoles",
	"componentstatuses": "componentStatuses", "configmaps": "configMaps",
	"controllerrevisions": "controllerRevisions", "cronjobs": "cronJobs",
	"csidrivers": "csiDrivers", "csinodes": "csiNodes", "customresourcedefinitions": "customResourceDefinitions",
	"daemonsets": "daemonSets", "endpointslices": "endpointSlices",
	"horizontalpodautoscalers": "horizontalPodAutoscalers", "ingressclasses": "ingressClasses",
	"limitranges": "limitRanges", "mutatingwebhookconfigurations": "mutatingWebhookConfigurations",
	"networkpolicies": "networkPolicies", "persistentvolumeclaims": "persistentVolumeClaims",
	"persistentvolumes": "persistentVolumes", "poddisruptionbudgets": "podDisruptionBudgets",
	"podsecuritypolicies": "podSecurityPolicies", "podtemplates": "podTemplates",
	"priorityclasses": "priorityClasses", "replicasets": "replicaSets",
	"replicationcontrollers": "replicationControllers", "resourcequotas": "resourceQuotas",
	"rolebindings": "roleBindings", "runtimeclasses": "runtimeClasses",
	"selfsubjectaccessreviews": "selfSubjectAccessReviews", "selfsubjectrulesreviews": "selfSubjectRulesReviews",
	"serviceaccounts": "serviceAccounts", "statefulsets": "statefulSets", "storageclasses": "storageClasses",
	"subjectaccessreviews": "subjectAccessReviews", "tokenreviews": "tokenReviews",
	"validatingwebhookconfigurations": "validatingWebhookConfigurations", "volumeattachments": "volumeAttachments",
	"localsubjectaccessreviews": "localSubjectAccessReviews", "leases": "leases",
}

// builtinGroup reports whether an API group is part of Kubernetes (custom
// resources map to container.thirdPartyObjects.*).
func builtinGroup(g string) bool {
	switch g {
	case "", "apps", "batch", "autoscaling", "policy", "extensions":
		return true
	}
	return strings.HasSuffix(g, ".k8s.io")
}

// k8sPermission maps resource attributes to a GKE IAM permission, e.g.
// get pods → container.pods.get, create pods/exec → container.pods.exec.
func k8sPermission(ra *resourceAttrs) string {
	if ra.Resource == "" {
		return ""
	}
	verb := map[string]string{
		"get": "get", "list": "list", "watch": "list", "create": "create", "update": "update",
		"patch": "update", "delete": "delete", "deletecollection": "delete",
		"bind": "bind", "escalate": "escalate", "impersonate": "impersonate",
	}[ra.Verb]
	if verb == "" {
		return ""
	}
	if !builtinGroup(ra.Group) {
		return "container.thirdPartyObjects." + verb
	}
	res := ra.Resource
	if m, ok := k8sResources[res]; ok {
		res = m
	}
	switch ra.Subresource {
	case "":
	case "exec":
		verb = "exec"
	case "attach":
		verb = "attach"
	case "portforward":
		verb = "portForward"
	case "log":
		verb = "getLogs"
	case "status":
		if verb == "get" || verb == "list" {
			verb = "getStatus"
		} else {
			verb = "updateStatus"
		}
	case "scale":
		if verb == "get" {
			verb = "getScale"
		} else {
			verb = "updateScale"
		}
	}
	return "container." + res + "." + verb
}

// ---- node endpoints ----

// serveNodeToken serves the node service account's access token (used by
// the node's registry proxy).
func (s *Service) serveNodeToken(w http.ResponseWriter, r *http.Request, key, node string) {
	rec, err := s.loadKey(key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c := rec.cluster()
	email := ""
	for _, n := range rec.Int.Nodes {
		if n.Name == node {
			if np := findPool(c, n.Pool); np != nil {
				email = nodeServiceAccount(rec.Int.Project, np.Config.ServiceAccount)
			}
		}
	}
	if email == "" {
		http.NotFound(w, r)
		return
	}
	keys := s.saKeys()
	if keys == nil {
		http.Error(w, "the iam service is not running", http.StatusServiceUnavailable)
		return
	}
	tok, exp, err := keys.AccessToken(r.Context(), emu.Principal("serviceAccount:"+email))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"access_token": tok, "expires_in": exp, "token_type": "Bearer"})
}

func (s *Service) saKeys() emu.ServiceAccountKeys {
	if p, ok := s.env.Lookup("iam"); ok {
		if k, ok := p.(emu.ServiceAccountKeys); ok {
			return k
		}
	}
	return nil
}

// nodeServiceAccount resolves a node pool's service account ("default" is
// the project's default compute service account).
func nodeServiceAccount(projectID, sa string) string {
	if sa == "" || sa == "default" {
		return projectNumber(projectID) + "-compute@developer.gserviceaccount.com"
	}
	return sa
}

// ---- registry mirrors ----

// arRegistryHosts lists every LOCATION-docker.pkg.dev host; nodes resolve
// them to the registry proxy so pulls never reach Google. Pulls are routed
// by ar.RegistriesYAML (FR-AR-005).
func arRegistryHosts() []string {
	locs := []string{"us", "europe", "asia"}
	for r := range regionZones {
		locs = append(locs, r)
	}
	sort.Strings(locs)
	out := make([]string, 0, len(locs))
	for _, l := range locs {
		out = append(out, l+"-docker.pkg.dev")
	}
	return out
}
