package lb

import (
	"context"
	"net/http"
	"strconv"
)

// init wires the validation functions (they reference other kinds, which
// would otherwise form initialization cycles).
func init() {
	kindForwardingRule.prepare = prepareForwardingRule
	kindTargetHTTPProxy.prepare = prepareHTTPProxy
	kindTargetHTTPSProxy.prepare = prepareHTTPSProxy
	kindURLMap.prepare = prepareURLMapKind
	kindBackendService.prepare = prepareBackendService
	kindBackendBucket.prepare = prepareBackendBucket
	kindHealthCheck.prepare = prepareHealthCheck
	kindSSLCertificate.prepare = prepareSSLCertificate
	kindSSLPolicy.prepare = prepareSSLPolicy
	kindSecurityPolicy.prepare = prepareSecurityPolicy
	kindHTTPHealthCheck.prepare = prepareLegacyHealthCheck
	kindHTTPSHealthCheck.prepare = prepareLegacyHealthCheck
	kindTargetSSLProxy.prepare = prepareSSLProxy
	kindTargetTCPProxy.prepare = prepareTCPProxy
	kindTargetGRPCProxy.prepare = prepareGRPCProxy
	kindForwardingRule.afterSave = func(ctx context.Context, s *Service, path string, obj, old any) { s.syncAddressUsers(ctx, obj, old) }
	kindForwardingRule.afterDelete = func(ctx context.Context, s *Service, path string, obj any) { s.syncAddressUsers(ctx, nil, obj) }
}

// allKinds lists every collection in dependency order (referenced kinds
// first), which is also the seed order.
func allKinds() []*kind {
	return []*kind{
		kindHealthCheck, kindHTTPHealthCheck, kindHTTPSHealthCheck, kindSSLCertificate, kindSSLPolicy,
		kindSecurityPolicy, kindBackendService, kindBackendBucket, kindURLMap,
		kindTargetHTTPProxy, kindTargetHTTPSProxy, kindTargetSSLProxy, kindTargetTCPProxy, kindTargetGRPCProxy,
		kindForwardingRule,
	}
}

// customMethods returns the POST {name}/{verb} methods of a kind.
func (s *Service) customMethods(k *kind) map[string]http.HandlerFunc {
	switch k {
	case kindForwardingRule:
		return s.forwardingRuleMethods()
	case kindTargetHTTPProxy:
		return s.httpProxyMethods()
	case kindTargetHTTPSProxy:
		return s.httpsProxyMethods()
	case kindURLMap:
		return s.urlMapMethods()
	case kindBackendService:
		return s.backendServiceMethods()
	case kindBackendBucket:
		return s.backendBucketMethods()
	case kindSecurityPolicy:
		return s.securityPolicyMethods()
	case kindTargetSSLProxy:
		return s.sslProxyMethods()
	case kindTargetTCPProxy:
		return s.tcpProxyMethods()
	}
	return nil
}

// extraRoutes registers methods outside the {name} pattern.
func (s *Service) extraRoutes(h func(string, http.HandlerFunc), p string) {
	h("GET "+p+"/global/sslPolicies/listAvailableFeatures", s.listAvailableFeatures)
	h("GET "+p+"/regions/{region}/sslPolicies/listAvailableFeatures", s.listAvailableFeatures)
	h("GET "+p+"/global/securityPolicies/{name}/getRule", s.getRule)
	h("GET "+p+"/regions/{region}/securityPolicies/{name}/getRule", s.getRule)
}

func itoa(i int) string { return strconv.Itoa(i) }
