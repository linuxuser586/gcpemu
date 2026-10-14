package gke

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// The Ingress and Gateway API controllers (FR-GKE-010). Every second, with
// the NEG controller, each cluster's Ingresses, Gateways and HTTPRoutes
// are translated (l7translate.go) and the plan applied through the "lb"
// service (emu.LoadBalancerResources): the NEGs its backends need first,
// then each resource that is new or whose desired form changed, then the
// removal of what the cluster owns but no longer needs, users before what
// they use. The forwarding rules' addresses are written back as the
// Ingress's status.loadBalancer and the Gateway's status.addresses, with
// GKE's resource annotations on Ingresses and Gateway API conditions on
// Gateways, their listeners and HTTPRoutes. While the Gateway API CRDs are
// installed the gke-l7 GatewayClasses exist, labelled as the gateway-api
// add-on's so that disabling the add-on removes them with the CRDs. A
// cluster's load balancers go with it. Without the "lb" service only NEGs
// are synced.

// l7Record is a load balancer resource a cluster owns.
type l7Record struct {
	Path string `json:"path"`
	// Hash of the desired form last applied.
	Hash string `json:"hash"`
}

const gwAPI = "/apis/gateway.networking.k8s.io/v1"

// lbResources returns the load balancer, or nil when it isn't running.
func (s *Service) lbResources() emu.LoadBalancerResources {
	if s.testLB != nil {
		return s.testLB
	}
	if p, ok := s.env.Lookup("lb"); ok {
		if r, ok := p.(emu.LoadBalancerResources); ok {
			return r
		}
	}
	return nil
}

// syncLoadBalancers reconciles a cluster's NEGs and load balancers.
func (s *Service) syncLoadBalancers(ctx context.Context, key string) error {
	rt := s.rtFor(key)
	rt.l7Mu.Lock()
	defer rt.l7Mu.Unlock()
	// A cluster being deleted cancels ctx: nothing may be created then.
	if err := ctx.Err(); err != nil {
		return err
	}
	lbr := s.lbResources()
	if lbr == nil {
		return s.syncNEGs(ctx, key, nil)
	}
	kc, err := s.kube(key)
	if err != nil {
		return err
	}
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	// The GatewayClasses first, so that a Gateway of a new cluster is
	// served on the first sync.
	var errs []error
	if err := s.ensureGatewayClasses(ctx, kc); err != nil && !isKubeNotFound(err) {
		errs = append(errs, err)
	}
	// A failed read must not look like objects that went away.
	in, err := s.l7Input(ctx, kc, rec)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	plan := translateL7(in)
	// NEGs a removed backend service still uses go on a later sync.
	errs = append(errs, s.syncNEGs(ctx, key, plan.NEGPorts))
	ips, err := s.applyL7(ctx, lbr, key, plan)
	errs = append(errs, err, s.writeL7Status(ctx, kc, in, plan, ips))
	return errors.Join(errs...)
}

// l7Input reads the objects the translation needs. Without the Gateway
// API CRDs there are no Gateways.
func (s *Service) l7Input(ctx context.Context, kc *kubeClient, rec *clusterRecord) (l7Input, error) {
	c := rec.cluster()
	in := l7Input{Cluster: l7Cluster{Project: rec.Int.Project, ID: c.Id, Zones: negZones(rec)}, Classes: map[string]bool{}}
	var svcs kubeList[kubeService]
	var ings kubeList[kubeIngress]
	if err := kc.get(ctx, "/api/v1/services", &svcs); err != nil {
		return in, err
	}
	if err := kc.get(ctx, "/apis/networking.k8s.io/v1/ingresses", &ings); err != nil {
		return in, err
	}
	in.Services, in.Ingresses = svcs.Items, ings.Items
	var classes kubeList[kubeGatewayClass]
	var gws kubeList[kubeGateway]
	var routes kubeList[kubeHTTPRoute]
	for _, l := range []struct {
		path string
		out  any
	}{{gwAPI + "/gatewayclasses", &classes}, {gwAPI + "/gateways", &gws}, {gwAPI + "/httproutes", &routes}} {
		if err := kc.get(ctx, l.path, l.out); err != nil && !isKubeNotFound(err) {
			return in, err
		}
	}
	for _, gc := range classes.Items {
		if gc.Spec.ControllerName == gwController {
			in.Classes[gc.Metadata.Name] = true
		}
	}
	in.Gateways, in.Routes = gws.Items, routes.Items
	secrets := map[string]*kubeSecret{}
	in.Secret = func(ns, name string) (string, string, bool) {
		k := ns + "/" + name
		sec, ok := secrets[k]
		if !ok {
			sec = &kubeSecret{}
			if err := kc.get(ctx, "/api/v1/namespaces/"+ns+"/secrets/"+name, sec); err != nil {
				sec = nil
			}
			secrets[k] = sec
		}
		if sec == nil || len(sec.Data["tls.crt"]) == 0 || len(sec.Data["tls.key"]) == 0 {
			return "", "", false
		}
		return string(sec.Data["tls.crt"]), string(sec.Data["tls.key"]), true
	}
	return in, nil
}

// negZones returns the zones NEGs are made in: the nodes', or the
// cluster's locations before it has nodes.
func negZones(rec *clusterRecord) []string {
	set := map[string]bool{}
	for _, n := range rec.Int.Nodes {
		set[n.Zone] = true
	}
	if len(set) == 0 {
		for _, z := range rec.cluster().Locations {
			set[z] = true
		}
	}
	zones := make([]string, 0, len(set))
	for z := range set {
		zones = append(zones, z)
	}
	sort.Strings(zones)
	return zones
}

func l7Key(cluster, path string) string { return cluster + "|" + path }

func (s *Service) ownedL7(key string) map[string]l7Record {
	owned := map[string]l7Record{}
	_ = s.env.Store.View(func(tx store.Tx) error {
		recs, _ := store.ListJSON[l7Record](tx, nsL7, key+"|")
		for _, r := range recs {
			owned[r.Path] = r
		}
		return nil
	})
	return owned
}

// applyL7 brings the cluster's load balancer resources in line with plan
// and returns the forwarding rules' addresses by path.
func (s *Service) applyL7(ctx context.Context, lbr emu.LoadBalancerResources, key string, plan l7Plan) (map[string]string, error) {
	owned := s.ownedL7(key)
	ips := map[string]string{}
	noteIP := func(path string, b []byte) {
		var fr computev1.ForwardingRule
		if isForwardingRule(path) && json.Unmarshal(b, &fr) == nil {
			ips[path] = fr.IPAddress
		}
	}
	desired := map[string]bool{}
	var errs []error
	for _, r := range plan.Resources {
		desired[r.Path] = true
		body := r.Body
		if r.SameIPAs != "" {
			ip := ips[r.SameIPAs]
			if ip == "" {
				errs = append(errs, fmt.Errorf("%s: %s has no address yet", r.Path, r.SameIPAs))
				continue
			}
			fr := *r.Body.(*computev1.ForwardingRule)
			fr.IPAddress = ip
			body = &fr
		}
		b, err := json.Marshal(body)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:8])
		if o, ok := owned[r.Path]; ok && o.Hash == h {
			if cur, err := lbr.LBResource(ctx, r.Path); err == nil {
				noteIP(r.Path, cur)
				continue
			}
		}
		out, err := lbr.PutLBResource(ctx, r.Path, b)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Path, err))
			continue
		}
		noteIP(r.Path, out)
		rec := l7Record{Path: r.Path, Hash: h}
		_ = s.env.Store.Update(func(tx store.Tx) error { return store.PutJSON(tx, nsL7, l7Key(key, r.Path), rec) })
	}
	var stale []string
	for p := range owned {
		if !desired[p] {
			stale = append(stale, p)
		}
	}
	errs = append(errs, s.removeL7(ctx, lbr, key, stale))
	return ips, errors.Join(errs...)
}

// removeL7 deletes owned resources, users before what they use; one still
// in use stays owned, to be retried.
func (s *Service) removeL7(ctx context.Context, lbr emu.LoadBalancerResources, key string, paths []string) error {
	sort.Slice(paths, func(i, j int) bool {
		if ri, rj := collRank(paths[i]), collRank(paths[j]); ri != rj {
			return ri > rj
		}
		return paths[i] < paths[j]
	})
	var errs []error
	for _, p := range paths {
		if err := lbr.DeleteLBResource(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("deleting %s: %w", p, err))
			continue
		}
		_ = s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsL7, l7Key(key, p)) })
	}
	return errors.Join(errs...)
}

// deleteL7 removes every load balancer resource a cluster owns, after any
// sync in flight. What can't be deleted, such as a backend service a
// user's URL map took up, is left to the user.
func (s *Service) deleteL7(ctx context.Context, key string) {
	rt := s.rtFor(key)
	rt.l7Mu.Lock()
	defer rt.l7Mu.Unlock()
	owned := s.ownedL7(key)
	if lbr := s.lbResources(); lbr != nil {
		var paths []string
		for p := range owned {
			paths = append(paths, p)
		}
		if err := s.removeL7(ctx, lbr, key, paths); err != nil {
			s.env.Log.Warn("gke: deleting load balancers", "cluster", key, "err", err)
		}
	}
	_ = s.env.Store.Update(func(tx store.Tx) error {
		for p := range owned {
			if err := tx.Delete(nsL7, l7Key(key, p)); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- status ----

// lbAddress returns a load balancer's address: its first forwarding
// rule's, once every one of them has it.
func lbAddress(l *l7LB, ips map[string]string) string {
	if len(l.FRs) == 0 {
		return ""
	}
	for _, fr := range l.FRs {
		if ips[fr] == "" {
			return ""
		}
	}
	return ips[l.FRs[0]]
}

// mergeConds gives conds the transition times of the same conditions in
// old whose status is unchanged, and now otherwise.
func mergeConds(old, conds []gwCondition, now string) []gwCondition {
	out := make([]gwCondition, len(conds))
	for i, c := range conds {
		c.LastTransitionTime = now
		for _, o := range old {
			if o.Type == c.Type && o.Status == c.Status && o.LastTransitionTime != "" {
				c.LastTransitionTime = o.LastTransitionTime
			}
		}
		out[i] = c
	}
	return out
}

func sameJSON(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// writeL7Status reports the load balancers on their objects, patching only
// what differs.
func (s *Service) writeL7Status(ctx context.Context, kc *kubeClient, in l7Input, plan l7Plan, ips map[string]string) error {
	now := s.env.Clock.Now().UTC().Format(time.RFC3339)
	ings := map[string]*kubeIngress{}
	for i := range in.Ingresses {
		ings[objKey(in.Ingresses[i].Metadata)] = &in.Ingresses[i]
	}
	gws := map[string]*kubeGateway{}
	for i := range in.Gateways {
		gws[objKey(in.Gateways[i].Metadata)] = &in.Gateways[i]
	}
	var errs []error
	for _, l := range plan.LBs {
		ip := lbAddress(l, ips)
		k := l.Namespace + "/" + l.Name
		switch l.Kind {
		case "Ingress":
			errs = append(errs, s.ingressStatus(ctx, kc, ings[k], l, ip))
		case "Gateway":
			errs = append(errs, s.gatewayStatus(ctx, kc, gws[k], l, ip, now))
		}
	}
	for i := range in.Routes {
		errs = append(errs, s.routeStatus(ctx, kc, &in.Routes[i], plan.RouteParents[objKey(in.Routes[i].Metadata)], now))
	}
	return errors.Join(errs...)
}

// ingressAnnotations are the annotations GKE reports on an Ingress.
var ingressAnnotations = []string{
	"ingress.kubernetes.io/forwarding-rule", "ingress.kubernetes.io/target-proxy", "ingress.kubernetes.io/url-map",
	"ingress.kubernetes.io/https-forwarding-rule", "ingress.kubernetes.io/https-target-proxy", "ingress.kubernetes.io/ssl-cert",
}

func (s *Service) ingressStatus(ctx context.Context, kc *kubeClient, ing *kubeIngress, l *l7LB, ip string) error {
	path := "/apis/networking.k8s.io/v1/namespaces/" + l.Namespace + "/ingresses/" + l.Name
	var errs []error
	annos := map[string]any{}
	for _, k := range ingressAnnotations {
		cur, has := ing.Metadata.Annotations[k]
		switch v, want := l.Annotations[k]; {
		case want && cur != v:
			annos[k] = v
		case !want && has:
			annos[k] = nil
		}
	}
	if len(annos) > 0 {
		errs = append(errs, kc.mergePatch(ctx, path, map[string]any{"metadata": map[string]any{"annotations": annos}}))
	}
	// Without forwarding rules the Ingress has no address; with them it
	// keeps the last one until they all have theirs.
	want := []lbIngressStatus{}
	if ip != "" {
		want = []lbIngressStatus{{IP: ip}}
	}
	if (ip != "" || len(l.FRs) == 0) && !sameJSON(append([]lbIngressStatus{}, ing.Status.LoadBalancer.Ingress...), want) {
		errs = append(errs, kc.mergePatch(ctx, path+"/status", map[string]any{"status": map[string]any{"loadBalancer": map[string]any{"ingress": want}}}))
	}
	return errors.Join(errs...)
}

func (s *Service) gatewayStatus(ctx context.Context, kc *kubeClient, gw *kubeGateway, l *l7LB, ip, now string) error {
	gen := l.Generation
	st := gwStatus{}
	accepted := gwCond("Accepted", true, "Accepted", "Gateway accepted.", gen)
	programmed := gwCond("Programmed", false, "Pending", "Waiting for the load balancer's address.", gen)
	switch {
	case !l.Valid:
		accepted = gwCond("Accepted", false, "ListenersNotValid", "No listener can be served.", gen)
		programmed = gwCond("Programmed", false, "Invalid", "No listener can be served.", gen)
	case ip != "":
		st.Addresses = []gwAddress{{Type: "IPAddress", Value: ip}}
		programmed = gwCond("Programmed", true, "Programmed", "Load balancer programmed.", gen)
	}
	st.Conditions = mergeConds(gw.Status.Conditions, []gwCondition{accepted, programmed}, now)
	for _, ls := range l.Listeners {
		accepted := false
		for _, c := range ls.Conditions {
			accepted = accepted || c.Type == "Accepted" && c.Status == "True"
		}
		resolved := !accepted
		for _, c := range ls.Conditions {
			resolved = resolved || c.Type == "ResolvedRefs" && c.Status == "True"
		}
		lp := gwCond("Programmed", false, "Invalid", "Listener not served.", gen)
		if accepted && resolved && ip != "" {
			lp = gwCond("Programmed", true, "Programmed", "Listener programmed.", gen)
		} else if accepted && resolved {
			lp = gwCond("Programmed", false, "Pending", "Waiting for the load balancer's address.", gen)
		}
		var old []gwCondition
		for _, o := range gw.Status.Listeners {
			if o.Name == ls.Name {
				old = o.Conditions
			}
		}
		ls.Conditions = mergeConds(old, append(append([]gwCondition{}, ls.Conditions...), lp), now)
		st.Listeners = append(st.Listeners, ls)
	}
	if sameJSON(gw.Status, st) {
		return nil
	}
	return kc.mergePatch(ctx, gwAPI+"/namespaces/"+l.Namespace+"/gateways/"+l.Name+"/status", map[string]any{"status": st})
}

// routeStatus sets the route's parent statuses for the Gateways handled,
// keeping other controllers'.
func (s *Service) routeStatus(ctx context.Context, kc *kubeClient, r *kubeHTTPRoute, ours []gwParentStatus, now string) error {
	var parents []gwParentStatus
	var mine []gwParentStatus
	for _, p := range r.Status.Parents {
		if p.ControllerName == gwController {
			mine = append(mine, p)
		} else {
			parents = append(parents, p)
		}
	}
	if len(mine) == 0 && len(ours) == 0 {
		return nil
	}
	for _, p := range ours {
		var old []gwCondition
		for _, o := range mine {
			if sameJSON(o.ParentRef, p.ParentRef) {
				old = o.Conditions
			}
		}
		p.Conditions = mergeConds(old, p.Conditions, now)
		parents = append(parents, p)
	}
	if sameJSON(r.Status.Parents, parents) {
		return nil
	}
	if parents == nil {
		parents = []gwParentStatus{}
	}
	return kc.mergePatch(ctx, gwAPI+"/namespaces/"+r.Metadata.Namespace+"/httproutes/"+r.Metadata.Name+"/status",
		map[string]any{"status": map[string]any{"parents": parents}})
}

// ensureGatewayClasses creates the gke-l7 GatewayClasses and accepts them.
func (s *Service) ensureGatewayClasses(ctx context.Context, kc *kubeClient) error {
	names := make([]string, 0, len(gatewayClasses))
	for n := range gatewayClasses {
		names = append(names, n)
	}
	sort.Strings(names)
	now := s.env.Clock.Now().UTC().Format(time.RFC3339)
	var errs []error
	for _, n := range names {
		path := gwAPI + "/gatewayclasses/" + n
		var gc kubeGatewayClass
		err := kc.get(ctx, path, &gc)
		if isKubeNotFound(err) {
			obj := map[string]any{
				"apiVersion": "gateway.networking.k8s.io/v1", "kind": "GatewayClass",
				"metadata": map[string]any{"name": n, "labels": map[string]string{addonLabel: gatewayAPIAddon}},
				"spec": map[string]any{"controllerName": gwController,
					"description": "GKE " + strings.TrimPrefix(n, "gke-") + " (gcpemu)"},
			}
			err = kc.create(ctx, gwAPI+"/gatewayclasses", obj, &gc)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("GatewayClass %s: %w", n, err))
			continue
		}
		if gc.Spec.ControllerName != gwController {
			continue
		}
		conds := mergeConds(gc.Status.Conditions, []gwCondition{gwCond("Accepted", true, "Accepted", "Handled by the emulator's GKE Gateway controller.", gc.Metadata.Generation)}, now)
		if !sameJSON(gc.Status.Conditions, conds) {
			errs = append(errs, kc.mergePatch(ctx, path+"/status", map[string]any{"status": map[string]any{"conditions": conds}}))
		}
	}
	return errors.Join(errs...)
}
