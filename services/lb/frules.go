package lb

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/compute"
)

// Forwarding rules (globalForwardingRules and regional forwardingRules) for
// Application Load Balancers: EXTERNAL_MANAGED (global and regional
// external), INTERNAL_MANAGED (regional internal) and the classic EXTERNAL
// scheme, all served by the same proxy (FR-LB-001/002). Rules may also
// target SSL, TCP and gRPC proxies, which are recorded without a listener.

var kindForwardingRule = &kind{
	coll: "forwardingRules", typ: "compute#forwardingRule", snake: "forwarding_rule",
	gperm: "compute.globalForwardingRules", rperm: "compute.forwardingRules",
	newObj: func() any { return &computev1.ForwardingRule{} },
	refs:   forwardingRuleRefs,
	keep:   []string{"IPAddress"},
}

func forwardingRuleRefs(obj any) []string {
	f := obj.(*computev1.ForwardingRule)
	return []string{f.Target, f.Network, f.Subnetwork}
}

// allowedExternalPorts are the ports a global or regional external
// Application Load Balancer forwarding rule may use.
var allowedExternalPorts = map[int]bool{80: true, 8080: true, 443: true}

func prepareForwardingRule(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	f := obj.(*computev1.ForwardingRule)
	if f.IPProtocol == "" {
		f.IPProtocol = "TCP"
	}
	if f.IPProtocol != "TCP" {
		return errInvalid("resource.IPProtocol", f.IPProtocol, "Forwarding rules of proxy load balancers must use TCP.")
	}
	if f.LoadBalancingScheme == "" {
		f.LoadBalancingScheme = "EXTERNAL"
	}
	switch f.LoadBalancingScheme {
	case "EXTERNAL", "EXTERNAL_MANAGED":
	case "INTERNAL_MANAGED":
		// Regional internal, or global (cross-region internal) ALB.
	case "INTERNAL_SELF_MANAGED":
		if sc.region != "" {
			return errInvalid("resource.loadBalancingScheme", f.LoadBalancingScheme, "INTERNAL_SELF_MANAGED is only supported for global forwarding rules.")
		}
	default:
		return errInvalid("resource.loadBalancingScheme", f.LoadBalancingScheme, "Only proxy load balancer schemes (EXTERNAL, EXTERNAL_MANAGED, INTERNAL_MANAGED, INTERNAL_SELF_MANAGED) are supported by the emulator.")
	}
	internal := strings.HasPrefix(f.LoadBalancingScheme, "INTERNAL")
	if !internal && f.NetworkTier == "" {
		f.NetworkTier = "PREMIUM"
	}
	// Target proxy.
	if f.Target == "" {
		return errRequired("resource.target")
	}
	tl, tk, err := s.refExisting(sc, f.Target, "resource.target",
		kindTargetHTTPSProxy, kindTargetHTTPProxy, kindTargetSSLProxy, kindTargetTCPProxy, kindTargetGRPCProxy)
	if err != nil {
		return err
	}
	f.Target = tl
	if tk == kindTargetGRPCProxy && f.LoadBalancingScheme != "INTERNAL_SELF_MANAGED" {
		return errInvalid("resource.loadBalancingScheme", f.LoadBalancingScheme, "Forwarding rules that target a gRPC proxy must use INTERNAL_SELF_MANAGED.")
	}
	l7 := tk == kindTargetHTTPSProxy || tk == kindTargetHTTPProxy
	if f.BackendService != "" {
		return errInvalid("resource.backendService", f.BackendService, "Proxy load balancer forwarding rules use a target proxy.")
	}
	// Port range: a single port.
	pr := strings.TrimSpace(f.PortRange)
	if pr == "" && len(f.Ports) == 1 {
		pr = f.Ports[0]
		f.Ports = nil
	}
	if pr == "" {
		pr = "80"
		if tk == kindTargetHTTPSProxy || tk == kindTargetSSLProxy {
			pr = "443"
		}
	}
	lo, hi, ok := strings.Cut(pr, "-")
	if !ok {
		hi = lo
	}
	pl, err1 := strconv.Atoi(lo)
	ph, err2 := strconv.Atoi(hi)
	if err1 != nil || err2 != nil || pl < 1 || ph > 65535 || pl != ph {
		return errInvalid("resource.portRange", f.PortRange, "Proxy load balancer forwarding rules must specify a single port.")
	}
	if l7 && !internal && !allowedExternalPorts[pl] {
		return errInvalid("resource.portRange", f.PortRange, "Port range must be one of 80, 8080 or 443 for external Application Load Balancers.")
	}
	f.PortRange = strconv.Itoa(pl) + "-" + strconv.Itoa(pl)
	// Network and subnetwork (internal schemes).
	if internal && sc.region != "" {
		net := f.Network
		if net == "" {
			net = "default"
		}
		np, err := canonRef(scope{project: sc.project}, "networks", net, "resource.network")
		if err != nil {
			return err
		}
		f.Network = compute.SelfLink(np)
		if f.Subnetwork != "" {
			sp, err := canonRef(sc, "subnetworks", f.Subnetwork, "resource.subnetwork")
			if err != nil {
				return err
			}
			f.Subnetwork = compute.SelfLink(sp)
		}
	}
	// IP address: literal, reserved address reference, or ephemeral.
	if old != nil {
		o := old.(*computev1.ForwardingRule)
		if f.IPAddress == "" {
			f.IPAddress = o.IPAddress
		}
	}
	if err := s.resolveFRAddress(ctx, sc, f, internal); err != nil {
		return err
	}
	// IP:port must be unique among forwarding rules of the same scope.
	for _, other := range s.loadAll(kindForwardingRule, sc.coll("forwardingRules")+"/") {
		o := other.(*computev1.ForwardingRule)
		if o.Name != f.Name && o.IPAddress == f.IPAddress && o.PortRange == f.PortRange {
			return apierr.InvalidArgument("Invalid value for field 'resource.IPAddress': '%s'. Specified IP address is in-use and would result in a conflict.", f.IPAddress).WithLegacy("invalid")
		}
	}
	return nil
}

// resolveFRAddress turns IPAddress into a literal IP, allocating an
// ephemeral one when it is empty.
func (s *Service) resolveFRAddress(ctx context.Context, sc scope, f *computev1.ForwardingRule, internal bool) error {
	ref := strings.TrimSpace(f.IPAddress)
	if ref == "" {
		// The callback runs inside compute's store transaction: no store
		// access from it, so the used set is collected first.
		used := map[string]bool{}
		for _, other := range s.loadAll(kindForwardingRule, sc.coll("forwardingRules")+"/") {
			used[other.(*computev1.ForwardingRule).IPAddress] = true
		}
		inUse := func(ip string) bool { return used[ip] }
		ip, err := s.cmp.EphemeralIP(ctx, sc.project, sc.region, internal, f.Subnetwork, inUse)
		if err != nil {
			return err
		}
		f.IPAddress = ip
		return nil
	}
	if ip := net.ParseIP(ref); ip != nil {
		if ip.To4() == nil {
			return errInvalid("resource.IPAddress", ref, "IPv6 forwarding rules are not supported by the emulator.")
		}
		f.IPAddress = ip.String()
		return nil
	}
	ap, err := canonRef(sc, "addresses", ref, "resource.IPAddress")
	if err != nil {
		return err
	}
	a, err := s.cmp.Address(ctx, ap)
	if err != nil {
		return errInvalid("resource.IPAddress", ref, "The referenced address resource cannot be found.")
	}
	f.IPAddress = a.Address
	return nil
}

// syncAddressUsers records which forwarding rules use the reserved
// addresses of now and before (status IN_USE, delete protection).
func (s *Service) syncAddressUsers(ctx context.Context, now, before any) {
	for _, obj := range []any{now, before} {
		if obj == nil {
			continue
		}
		f := obj.(*computev1.ForwardingRule)
		sc := scopeOfPath(relPath(f.SelfLink))
		ap, ok := s.cmp.AddressByIP(ctx, sc.project, sc.region, f.IPAddress)
		if !ok {
			continue
		}
		_ = s.cmp.SetAddressUsers(ctx, ap, func(tx store.Tx) []string {
			var users []string
			tx.Scan(kindForwardingRule.ns(), sc.coll("forwardingRules")+"/", func(_ string, b []byte) bool {
				var o computev1.ForwardingRule
				if json.Unmarshal(b, &o) == nil && o.IPAddress == f.IPAddress {
					users = append(users, o.SelfLink)
				}
				return true
			})
			return users
		})
	}
}

func (s *Service) forwardingRuleMethods() map[string]http.HandlerFunc {
	k := kindForwardingRule
	return map[string]http.HandlerFunc{
		"setTarget": s.mutateH(k, "setTarget", "setTarget",
			func() any { return &computev1.TargetReference{} },
			func(r *http.Request, sc scope, obj, req any) error {
				t := req.(*computev1.TargetReference).Target
				if t == "" {
					return errRequired("target")
				}
				obj.(*computev1.ForwardingRule).Target = t
				return nil
			}),
		"setLabels": s.setLabelsH(k),
	}
}
