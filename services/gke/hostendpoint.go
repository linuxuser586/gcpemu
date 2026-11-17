package gke

import (
	"net"
	"strconv"
	"time"
)

// Host clients reach pods from the Host address (ADR 0088): the emulator
// chooses the endpoint of a host client's connection itself, as kube-proxy
// would, so that it can relay through the node the pod runs on.

// defaultAffinityTimeout is kube-proxy's ClientIP affinity timeout when a
// Service sets none.
const defaultAffinityTimeout = 3 * time.Hour

// hostEndpoint is the endpoint a host client's connection goes to: a pod
// address and target port, and the node it runs on.
type hostEndpoint struct {
	addr string // ip:port
	node string
}

// hostAffinity is a Service's ClientIP session affinity for host clients,
// who are all one client: the endpoint IP they stay on, until when.
type hostAffinity struct {
	ip    string
	until time.Time
}

// chooseHostEndpoint chooses the endpoint for a host client's connection
// to svc's port from its EndpointSlices: one of the ready endpoints, or of
// the serving terminating ones when none is ready, on the target port its
// slice names for the Service port. With sessionAffinity ClientIP it keeps
// aff's endpoint while that serves and aff has not expired, and renews
// aff; otherwise pick draws among the candidates. ok is false when
// nothing serves, or when the endpoint chosen is not a pod in podCIDR on a
// named node: the connection then goes to the ClusterIP.
func chooseHostEndpoint(svc *kubeService, port int, slices []kubeEndpointSlice, podCIDR *net.IPNet, aff *hostAffinity, now time.Time, pick func(n int) int) (ep hostEndpoint, ok bool) {
	name, found := "", false
	for _, p := range svc.Spec.Ports {
		if p.Port == port && (p.Protocol == "" || p.Protocol == "TCP") {
			name, found = p.Name, true
		}
	}
	if !found {
		return hostEndpoint{}, false
	}
	type cand struct {
		ip, node string
		port     int
	}
	var ready, terminating []cand
	for _, sl := range slices {
		if sl.AddressType != "IPv4" {
			continue
		}
		target := 0
		for _, p := range sl.Ports {
			if p.Port != nil && (p.Name == nil && name == "" || p.Name != nil && *p.Name == name) &&
				(p.Protocol == "" || p.Protocol == "TCP") {
				target = *p.Port
			}
		}
		if target == 0 {
			continue
		}
		for _, e := range sl.Endpoints {
			if len(e.Addresses) == 0 {
				continue
			}
			c := cand{ip: e.Addresses[0], node: e.NodeName, port: target}
			cond := e.Conditions
			switch {
			case cond.Ready == nil || *cond.Ready:
				ready = append(ready, c)
			case cond.Serving != nil && *cond.Serving && cond.Terminating != nil && *cond.Terminating:
				terminating = append(terminating, c)
			}
		}
	}
	cands := ready
	if len(cands) == 0 {
		cands = terminating
	}
	if len(cands) == 0 {
		return hostEndpoint{}, false
	}
	var c *cand
	sticky := svc.Spec.SessionAffinity == "ClientIP"
	if sticky && aff.ip != "" && now.Before(aff.until) {
		for i := range cands {
			if cands[i].ip == aff.ip {
				c = &cands[i]
				break
			}
		}
	}
	if c == nil {
		c = &cands[pick(len(cands))]
	}
	if sticky {
		aff.ip, aff.until = c.ip, now.Add(svc.affinityTimeout())
	}
	if ip := net.ParseIP(c.ip); c.node == "" || ip == nil || podCIDR == nil || !podCIDR.Contains(ip) {
		return hostEndpoint{}, false
	}
	return hostEndpoint{addr: net.JoinHostPort(c.ip, strconv.Itoa(c.port)), node: c.node}, true
}

// affinityTimeout is how long svc's ClientIP session affinity lasts.
func (svc *kubeService) affinityTimeout() time.Duration {
	if cfg := svc.Spec.SessionAffinityConfig; cfg != nil && cfg.ClientIP != nil && cfg.ClientIP.TimeoutSeconds != nil {
		return time.Duration(*cfg.ClientIP.TimeoutSeconds) * time.Second
	}
	return defaultAffinityTimeout
}
