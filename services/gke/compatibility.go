package gke

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Autopilot compatibility check (FR-GKE-017).
//
// A Standard cluster's running pods go through the Workload restrictions
// of an Autopilot cluster (restrictions.go), with their rules and
// namespace scope, and what they would refuse or change is reported
// against each pod's top-level workload. The check reads the cluster as it
// is when the request arrives; it neither waits for nor depends on
// workloadPolicyConfig.autopilotCompatibilityAuditingEnabled.
//
// GKE documents none of the descriptions or the subject format below, and
// no Recording of the call exists yet (ADR 0023), so they and the error
// messages are unverified.

// podResourcesConstraint names the issue of pods whose requests Autopilot
// would set or adjust.
const podResourcesConstraint = "autogke-pod-resources"

// GKE's public Autopilot docs for refusals and for resource requests.
const (
	securityDocs  = "https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-security"
	resourcesDocs = "https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-resource-requests"
)

// compatDescriptions are the issues' descriptions, by constraint and
// issue type.
var compatDescriptions = map[string]string{
	"autogke-disallow-privilege":         "Privileged containers are not allowed in Autopilot.",
	"autogke-disallow-hostnamespaces":    "Enabling hostNetwork, hostPID or hostIPC is not allowed in Autopilot.",
	"autogke-no-write-mode-hostpath":     "Autopilot allows hostPath volumes only read-only under /var/log/.",
	"autogke-no-host-port":               "Host ports are not allowed in Autopilot.",
	"autogke-default-linux-capabilities": "Autopilot allows containers to add only its default Linux capabilities.",
	podResourcesConstraint:               "Autopilot would set or adjust the resource requests of these workloads' Pods.",
}

// netAdminDescription describes the issue of containers that add
// NET_ADMIN.
const netAdminDescription = "Containers add the NET_ADMIN capability, which Autopilot allows only with workloadPolicyConfig.allowNetAdmin set."

// checkedPod is the part of a pod the check reads besides what the
// Workload restrictions read.
type checkedPod struct {
	Metadata objectMeta `json:"metadata"`
	Status   struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// ownerKey keys the owners compatibilityIssues follows: a ReplicaSet or
// Job, say, by kind, namespace and name.
func ownerKey(kind, ns, name string) string { return kind + "/" + ns + "/" + name }

// compatibilityIssues checks pods, as the Kubernetes API lists them,
// against the Workload restrictions and returns an issue per constraint
// and issue type, naming the top-level workloads, followed through
// owners, of the pods it applies to.
func compatibilityIssues(pods []json.RawMessage, owners map[string]objectMeta, now time.Time) []*containerpb.AutopilotCompatibilityIssue {
	type group struct {
		constraint string
		typ        containerpb.AutopilotCompatibilityIssue_IssueType
	}
	subjects := map[group][]string{}
	for _, raw := range pods {
		var p checkedPod
		var rp restrictedPod
		var res resourcePod
		if json.Unmarshal(raw, &p) != nil || json.Unmarshal(raw, &rp) != nil || json.Unmarshal(raw, &res) != nil {
			continue
		}
		ns := p.Metadata.Namespace
		if !autopilotRestricted(ns) || p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			continue
		}
		subject := ns + "/" + topOwner(p.Metadata, owners)
		add := func(g group) { subjects[g] = append(subjects[g], subject) }
		refused, withNetAdmin := restrictionViolations(&rp, false), restrictionViolations(&rp, true)
		for c, msgs := range refused {
			if len(withNetAdmin[c]) > 0 {
				add(group{c, containerpb.AutopilotCompatibilityIssue_INCOMPATIBILITY})
			}
			if len(msgs) > len(withNetAdmin[c]) {
				add(group{c, containerpb.AutopilotCompatibilityIssue_ADDITIONAL_CONFIG_REQUIRED})
			}
		}
		if ops, _ := autopilotResources(&res, ns); len(ops) > 0 {
			add(group{podResourcesConstraint, containerpb.AutopilotCompatibilityIssue_PASSED_WITH_OPTIONAL_CONFIG})
		}
	}
	var issues []*containerpb.AutopilotCompatibilityIssue
	for g, s := range subjects {
		slices.Sort(s)
		desc, docs := compatDescriptions[g.constraint], securityDocs
		switch {
		case g.typ == containerpb.AutopilotCompatibilityIssue_ADDITIONAL_CONFIG_REQUIRED:
			desc = netAdminDescription
		case g.constraint == podResourcesConstraint:
			docs = resourcesDocs
		case desc == "":
			desc = "Autopilot refuses these workloads (" + g.constraint + ")."
		}
		issues = append(issues, &containerpb.AutopilotCompatibilityIssue{
			LastObservation:     timestamppb.New(now),
			ConstraintType:      g.constraint,
			IncompatibilityType: g.typ,
			Subjects:            slices.Compact(s),
			DocumentationUrl:    docs,
			Description:         desc,
		})
	}
	slices.SortFunc(issues, func(a, b *containerpb.AutopilotCompatibilityIssue) int {
		return cmp.Or(cmp.Compare(a.ConstraintType, b.ConstraintType), cmp.Compare(a.IncompatibilityType, b.IncompatibilityType))
	})
	return issues
}

// topOwner is the name of m's top-level owner, following the first owner
// reference of each object through owners, or m's own name if it has
// none.
func topOwner(m objectMeta, owners map[string]objectMeta) string {
	name, ns := m.Name, m.Namespace
	// The bound stops a cycle of owner references.
	for range 16 {
		if len(m.OwnerReferences) == 0 {
			break
		}
		o := m.OwnerReferences[0]
		name = o.Name
		next, ok := owners[ownerKey(o.Kind, ns, o.Name)]
		if !ok {
			break
		}
		m = next
	}
	return name
}

// ownerLists are the owners topOwner follows past, by kind, and where the
// Kubernetes API lists them.
var ownerLists = map[string]string{
	"ReplicaSet":            "/apis/apps/v1/replicasets",
	"Job":                   "/apis/batch/v1/jobs",
	"ReplicationController": "/api/v1/replicationcontrollers",
}

func (a *api) CheckAutopilotCompatibility(ctx context.Context, req *containerpb.CheckAutopilotCompatibilityRequest) (*containerpb.CheckAutopilotCompatibilityResponse, error) {
	r, err := parseRef(req.Name, "", "", "", "", 1)
	if err != nil {
		return nil, err
	}
	if err := a.check(ctx, r, "container.clusters.get", r.clusterResource()); err != nil {
		return nil, err
	}
	rec, err := a.load(r)
	if err != nil {
		return nil, err
	}
	c := rec.cluster()
	if autopilotOn(c) {
		return nil, apierr.FailedPrecondition("CheckAutopilotCompatibility is only supported on Standard clusters.")
	}
	if c.Status != containerpb.Cluster_RUNNING && c.Status != containerpb.Cluster_RECONCILING {
		return nil, apierr.FailedPrecondition("Cluster %s is not running (status %s).", r.Cluster, c.Status)
	}
	kc, err := a.s.kube(r.key())
	if err != nil {
		return nil, err
	}
	var pods kubeList[json.RawMessage]
	if err := kc.get(ctx, "/api/v1/pods", &pods); err != nil {
		return nil, apierr.Internal("listing the cluster's pods: %v", err)
	}
	owners := map[string]objectMeta{}
	for kind, path := range ownerLists {
		var list kubeList[struct {
			Metadata objectMeta `json:"metadata"`
		}]
		if err := kc.get(ctx, path, &list); err != nil {
			return nil, apierr.Internal("listing the cluster's %ss: %v", kind, err)
		}
		for _, o := range list.Items {
			owners[ownerKey(kind, o.Metadata.Namespace, o.Metadata.Name)] = o.Metadata
		}
	}
	return &containerpb.CheckAutopilotCompatibilityResponse{
		Issues: compatibilityIssues(pods.Items, owners, a.s.env.Clock.Now()),
	}, nil
}
