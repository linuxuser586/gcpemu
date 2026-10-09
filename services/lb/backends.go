package lb

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
	"github.com/linuxuser586/gcpemu/services/compute"
)

// Backend services (global and regional) and backend buckets (FR-LB-001,
// FR-LB-005..008, FR-CDN-001/007).

var kindBackendService = &kind{
	coll: "backendServices", typ: "compute#backendService", snake: "backend_service",
	gperm: "compute.backendServices", rperm: "compute.regionBackendServices",
	newObj: func() any { return &computev1.BackendService{} },
	refs: func(obj any) []string {
		b := obj.(*computev1.BackendService)
		out := append([]string{}, b.HealthChecks...)
		for _, be := range b.Backends {
			out = append(out, be.Group)
		}
		return append(out, b.SecurityPolicy, b.EdgeSecurityPolicy)
	},
	afterDelete: func(ctx context.Context, s *Service, path string, obj any) { s.deleteSignedKeys(path) },
	keep:        []string{"securityPolicy", "edgeSecurityPolicy"},
}

var kindBackendBucket = &kind{
	coll: "backendBuckets", typ: "compute#backendBucket", snake: "backend_bucket",
	gperm:  "compute.backendBuckets",
	newObj: func() any { return &computev1.BackendBucket{} },
	refs:   func(obj any) []string { return []string{obj.(*computev1.BackendBucket).EdgeSecurityPolicy} },
	afterDelete: func(ctx context.Context, s *Service, path string, obj any) {
		s.deleteSignedKeys(path)
	},
	keep: []string{"edgeSecurityPolicy"},
}

func prepareBackendService(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	b := obj.(*computev1.BackendService)
	if b.Protocol == "" {
		b.Protocol = "HTTP"
	}
	switch b.Protocol {
	case "HTTP", "HTTPS", "HTTP2", "H2C", "GRPC", "TCP", "SSL", "UDP", "UNSPECIFIED":
	default:
		return errInvalid("resource.protocol", b.Protocol, "")
	}
	if b.LoadBalancingScheme == "" {
		b.LoadBalancingScheme = "EXTERNAL"
	}
	switch b.LoadBalancingScheme {
	case "EXTERNAL", "EXTERNAL_MANAGED", "INTERNAL_MANAGED", "INTERNAL_SELF_MANAGED", "INTERNAL":
	default:
		return errInvalid("resource.loadBalancingScheme", b.LoadBalancingScheme, "")
	}
	if b.TimeoutSec == 0 {
		b.TimeoutSec = 30
	}
	if b.TimeoutSec < 1 || b.TimeoutSec > 2147483647 {
		return errInvalid("resource.timeoutSec", b.TimeoutSec, "Must be greater than or equal to 1.")
	}
	if b.Port == 0 {
		b.Port = 80
	}
	if b.PortName == "" {
		b.PortName = "http"
	}
	if b.SessionAffinity == "" {
		b.SessionAffinity = "NONE"
	}
	switch b.SessionAffinity {
	case "NONE", "CLIENT_IP", "CLIENT_IP_PORT_PROTO", "CLIENT_IP_PROTO", "CLIENT_IP_NO_DESTINATION",
		"GENERATED_COOKIE", "HEADER_FIELD", "HTTP_COOKIE", "STRONG_COOKIE_AFFINITY":
	default:
		return errInvalid("resource.sessionAffinity", b.SessionAffinity, "")
	}
	if (b.SessionAffinity == "HEADER_FIELD" || b.SessionAffinity == "HTTP_COOKIE") &&
		b.LocalityLbPolicy != "RING_HASH" && b.LocalityLbPolicy != "MAGLEV" {
		return errInvalid("resource.sessionAffinity", b.SessionAffinity, "HEADER_FIELD and HTTP_COOKIE session affinity require localityLbPolicy RING_HASH or MAGLEV.")
	}
	switch b.LocalityLbPolicy {
	case "", "ROUND_ROBIN", "LEAST_REQUEST", "RING_HASH", "RANDOM", "ORIGINAL_DESTINATION", "MAGLEV", "WEIGHTED_MAGLEV", "WEIGHTED_ROUND_ROBIN":
	default:
		return errInvalid("resource.localityLbPolicy", b.LocalityLbPolicy, "")
	}
	if b.ConnectionDraining == nil {
		b.ConnectionDraining = &computev1.ConnectionDraining{}
	}
	b.ConnectionDraining.ForceSendFields = []string{"DrainingTimeoutSec"}
	if b.LogConfig != nil && b.LogConfig.Enable && b.LogConfig.SampleRate == 0 {
		b.LogConfig.SampleRate = 1
	}
	if b.LogConfig != nil && (b.LogConfig.SampleRate < 0 || b.LogConfig.SampleRate > 1) {
		return errInvalid("resource.logConfig.sampleRate", b.LogConfig.SampleRate, "Must be between 0.0 and 1.0.")
	}
	if b.CdnPolicy != nil {
		if err := validateCdnPolicy(serviceCdnPolicy(b.CdnPolicy)); err != nil {
			return err
		}
	}
	if b.EnableCDN {
		b.CdnPolicy = defaultServiceCdnPolicy(b.CdnPolicy, old == nil)
	}
	if b.CdnPolicy != nil {
		b.CdnPolicy.SignedUrlKeyNames = s.signedKeyNames(path)
	}
	// Health checks: at most one, same scope.
	if len(b.HealthChecks) > 1 {
		return errInvalid("resource.healthChecks", strings.Join(b.HealthChecks, ","), "At most one health check can be specified.")
	}
	for i, hc := range b.HealthChecks {
		l, _, err := s.refExisting(sc, hc, "resource.healthChecks["+itoa(i)+"]", kindHealthCheck)
		if err != nil {
			return err
		}
		b.HealthChecks[i] = l
	}
	// Cloud Armor policies (recorded).
	var err error
	if b.SecurityPolicy, err = s.refSecurityPolicy(sc, b.SecurityPolicy, "resource.securityPolicy", "CLOUD_ARMOR"); err != nil {
		return err
	}
	if b.EdgeSecurityPolicy, err = s.refSecurityPolicy(sc, b.EdgeSecurityPolicy, "resource.edgeSecurityPolicy", "CLOUD_ARMOR_EDGE"); err != nil {
		return err
	}
	// Backends: NEGs (zonal, regional or global) or instance groups.
	seen := map[string]bool{}
	for i, be := range b.Backends {
		f := "resource.backends[" + itoa(i) + "].group"
		if be.Group == "" {
			return errRequired(f)
		}
		p := relPath(be.Group)
		if strings.HasPrefix(p, "zones/") || strings.HasPrefix(p, "regions/") || strings.HasPrefix(p, "global/") {
			p = "projects/" + sc.project + "/" + p
		}
		segs := strings.Split(p, "/")
		global := len(segs) == 5 && segs[0] == "projects" && segs[2] == "global" && segs[3] == "networkEndpointGroups"
		if !global && (len(segs) != 6 || segs[0] != "projects" || (segs[2] != "zones" && segs[2] != "regions") ||
			(segs[4] != "networkEndpointGroups" && segs[4] != "instanceGroups")) {
			return errInvalid(f, be.Group, "The URL is malformed.")
		}
		switch {
		case global || segs[4] == "networkEndpointGroups":
			_, err = s.cmp.NEGEndpoints(ctx, p)
		case segs[2] == "zones":
			_, err = s.cmp.InstanceGroup(ctx, p)
		}
		if err != nil {
			return errNotFound(p)
		}
		if seen[p] {
			return errInvalid(f, be.Group, "Duplicate backend group.")
		}
		seen[p] = true
		be.Group = compute.SelfLink(p)
		if be.BalancingMode == "" {
			be.BalancingMode = "UTILIZATION"
			if global || segs[4] == "networkEndpointGroups" {
				be.BalancingMode = "RATE"
			}
		}
		switch be.BalancingMode {
		case "UTILIZATION", "RATE", "CONNECTION", "CUSTOM_METRICS", "IN_FLIGHT":
		default:
			return errInvalid("resource.backends["+itoa(i)+"].balancingMode", be.BalancingMode, "")
		}
		if be.CapacityScaler < 0 || be.CapacityScaler > 1 {
			return errInvalid("resource.backends["+itoa(i)+"].capacityScaler", be.CapacityScaler, "Must be between 0.0 and 1.0.")
		}
		// capacityScaler defaults to 1 for a newly added group (an omitted
		// field decodes as 0; GET always returns it, so updates carry it).
		if be.CapacityScaler == 0 && !hasForceSend(be.ForceSendFields, "CapacityScaler") && !hadGroup(old, p) {
			be.CapacityScaler = 1
		}
		be.ForceSendFields = append(be.ForceSendFields, "CapacityScaler")
	}
	if t := b.TlsSettings; t != nil && t.AuthenticationConfig != "" {
		if b.Protocol != "HTTPS" && b.Protocol != "HTTP2" {
			return errInvalid("resource.tlsSettings.authenticationConfig", t.AuthenticationConfig, "Backend authentication requires protocol HTTPS or HTTP2.")
		}
	}
	for i, h := range b.CustomRequestHeaders {
		if !strings.Contains(h, ":") {
			return errInvalid("resource.customRequestHeaders["+itoa(i)+"]", h, "Headers must be of the form 'name:value'.")
		}
	}
	for i, h := range b.CustomResponseHeaders {
		if !strings.Contains(h, ":") {
			return errInvalid("resource.customResponseHeaders["+itoa(i)+"]", h, "Headers must be of the form 'name:value'.")
		}
	}
	return nil
}

// hadGroup reports whether the previous version had a backend group.
func hadGroup(old any, group string) bool {
	if old == nil {
		return false
	}
	for _, be := range old.(*computev1.BackendService).Backends {
		if relPath(be.Group) == group {
			return true
		}
	}
	return false
}

func hasForceSend(fs []string, f string) bool {
	for _, x := range fs {
		if x == f {
			return true
		}
	}
	return false
}

// defaultServiceCdnPolicy fills Cloud CDN defaults (as GCP does when CDN is
// enabled without an explicit policy).
// requestCoalescing and signedUrlCacheMaxAgeSec are not echoed by GCP when
// unset; their defaults are applied when the cache is consulted
// (cdnServicePolicy / cdnBucketPolicy).
func defaultServiceCdnPolicy(p *computev1.BackendServiceCdnPolicy, create bool) *computev1.BackendServiceCdnPolicy {
	if p == nil {
		p = &computev1.BackendServiceCdnPolicy{}
	}

	if p.CacheMode == "" {
		p.CacheMode = "CACHE_ALL_STATIC"
	}
	if p.CacheMode != "USE_ORIGIN_HEADERS" {
		if p.DefaultTtl == 0 {
			p.DefaultTtl = 3600
		}
		if p.MaxTtl == 0 && p.CacheMode != "FORCE_CACHE_ALL" {
			p.MaxTtl = 86400
		}
		if p.ClientTtl == 0 {
			p.ClientTtl = p.DefaultTtl
		}
	}
	if k := p.CacheKeyPolicy; k == nil {
		p.CacheKeyPolicy = &computev1.CacheKeyPolicy{IncludeHost: true, IncludeProtocol: true, IncludeQueryString: true}
	} else if create && !k.IncludeHost && !k.IncludeProtocol && !k.IncludeQueryString &&
		len(k.QueryStringWhitelist) == 0 && len(k.QueryStringBlacklist) == 0 {
		// A key policy naming only headers/cookies keeps the default parts.
		k.IncludeHost, k.IncludeProtocol, k.IncludeQueryString = true, true, true
	}
	return p
}

func defaultBucketCdnPolicy(p *computev1.BackendBucketCdnPolicy, create bool) *computev1.BackendBucketCdnPolicy {
	if p == nil {
		p = &computev1.BackendBucketCdnPolicy{}
	}

	if p.CacheMode == "" {
		p.CacheMode = "CACHE_ALL_STATIC"
	}
	if p.CacheMode != "USE_ORIGIN_HEADERS" {
		if p.DefaultTtl == 0 {
			p.DefaultTtl = 3600
		}
		if p.MaxTtl == 0 && p.CacheMode != "FORCE_CACHE_ALL" {
			p.MaxTtl = 86400
		}
		if p.ClientTtl == 0 {
			p.ClientTtl = p.DefaultTtl
		}
	}
	return p
}

func prepareBackendBucket(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	b := obj.(*computev1.BackendBucket)
	if b.BucketName == "" {
		return errRequired("resource.bucketName")
	}
	var err error
	if b.EdgeSecurityPolicy, err = s.refSecurityPolicy(sc, b.EdgeSecurityPolicy, "resource.edgeSecurityPolicy", "CLOUD_ARMOR_EDGE"); err != nil {
		return err
	}
	if b.CdnPolicy != nil {
		if err := validateCdnPolicy(bucketCdnPolicy(b.CdnPolicy)); err != nil {
			return err
		}
	}
	if b.EnableCdn {
		b.CdnPolicy = defaultBucketCdnPolicy(b.CdnPolicy, old == nil)
	}
	if b.CdnPolicy != nil {
		b.CdnPolicy.SignedUrlKeyNames = s.signedKeyNames(path)
	}
	for i, h := range b.CustomResponseHeaders {
		if !strings.Contains(h, ":") {
			return errInvalid("resource.customResponseHeaders["+itoa(i)+"]", h, "Headers must be of the form 'name:value'.")
		}
	}
	return nil
}

// --- signed URL keys (FR-CDN-007) ---

const nsSignedKeys = "lb/signedUrlKeys"

// signedKeys maps key name → base64url key value; stored apart from the
// resource because GCP never returns key values.
type signedKeys map[string]string

func (s *Service) signedKeyNames(path string) []string {
	var ks signedKeys
	_ = s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsSignedKeys, path, &ks) })
	var out []string
	for n := range ks {
		out = append(out, n)
	}
	sortStrings(out)
	return out
}

// SignedURLKeys returns the signed URL/cookie keys (name → raw 128-bit key)
// of a backend service or bucket given by selfLink or path, for Cloud CDN
// signed request validation (FR-CDN-007).
func (s *Service) SignedURLKeys(backend string) map[string][]byte {
	var ks signedKeys
	_ = s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsSignedKeys, relPath(backend), &ks) })
	out := map[string][]byte{}
	for n, v := range ks {
		if b, err := base64.URLEncoding.DecodeString(v); err == nil {
			out[n] = b
		} else if b, err := base64.RawURLEncoding.DecodeString(v); err == nil {
			out[n] = b
		}
	}
	return out
}

func (s *Service) deleteSignedKeys(path string) {
	_ = s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsSignedKeys, path) })
}

// signedKeyMethods implements addSignedUrlKey and deleteSignedUrlKey.
func (s *Service) signedKeyMethods(k *kind) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"addSignedUrlKey":    s.signedKeyH(k, true),
		"deleteSignedUrlKey": s.signedKeyH(k, false),
	}
}

func (s *Service) signedKeyH(k *kind, add bool) http.HandlerFunc {
	verb := "deleteSignedUrlKey"
	if add {
		verb = "addSignedUrlKey"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		sc, path, obj, err := s.loadReq(r, k, verb)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		var key computev1.SignedUrlKey
		if add {
			if _, err := decode(r, &key); err != nil {
				apierr.Write(w, err)
				return
			}
			if err := validName("signedUrlKey.keyName", key.KeyName); err != nil {
				apierr.Write(w, err)
				return
			}
			raw, err := base64.URLEncoding.DecodeString(key.KeyValue)
			if err != nil {
				raw, err = base64.RawURLEncoding.DecodeString(key.KeyValue)
			}
			if err != nil || len(raw) != 16 {
				apierr.Write(w, errInvalid("signedUrlKey.keyValue", "<redacted>", "Key value must be a base64url-encoded 128-bit value."))
				return
			}
		} else {
			key.KeyName = r.URL.Query().Get("keyName")
			if key.KeyName == "" {
				apierr.Write(w, errRequired("keyName"))
				return
			}
		}
		var ks signedKeys
		_ = s.env.Store.View(func(tx store.Tx) error { return store.GetJSON(tx, nsSignedKeys, path, &ks) })
		if ks == nil {
			ks = signedKeys{}
		}
		_, exists := ks[key.KeyName]
		switch {
		case add && exists:
			apierr.Write(w, apierr.AlreadyExists("The key '%s' already exists.", key.KeyName).WithLegacy("alreadyExists"))
			return
		case add && len(ks) >= 3:
			apierr.Write(w, apierr.InvalidArgument("At most 3 signed URL keys can be added to a backend.").WithLegacy("invalid"))
			return
		case !add && !exists:
			apierr.Write(w, apierr.NotFound("The key '%s' was not found.", key.KeyName).WithLegacy("notFound"))
			return
		}
		op, err := s.cmp.StartOperation(r.Context(), sc.project, sc.String(), verb, path, getID(obj), func(ctx context.Context) error {
			if err := s.env.Store.Update(func(tx store.Tx) error {
				var cur signedKeys
				_ = store.GetJSON(tx, nsSignedKeys, path, &cur)
				if cur == nil {
					cur = signedKeys{}
				}
				if add {
					cur[key.KeyName] = key.KeyValue
				} else {
					delete(cur, key.KeyName)
				}
				return store.PutJSON(tx, nsSignedKeys, path, cur)
			}); err != nil {
				return err
			}
			cur, ok := s.load(k, path)
			if !ok {
				return errNotFound(path)
			}
			next := clone(k, cur)
			switch b := next.(type) {
			case *computev1.BackendService:
				if b.CdnPolicy == nil {
					b.CdnPolicy = &computev1.BackendServiceCdnPolicy{}
				}
				b.CdnPolicy.SignedUrlKeyNames = s.signedKeyNames(path)
			case *computev1.BackendBucket:
				if b.CdnPolicy == nil {
					b.CdnPolicy = &computev1.BackendBucketCdnPolicy{}
				}
				b.CdnPolicy.SignedUrlKeyNames = s.signedKeyNames(path)
			}
			return s.save(ctx, k, sc, path, next, cur)
		})
		reply(w, op, err)
	}
}

func (s *Service) backendServiceMethods() map[string]http.HandlerFunc {
	k := kindBackendService
	m := s.signedKeyMethods(k)
	m["getHealth"] = s.getHealth
	m["setSecurityPolicy"] = s.mutateH(k, "setSecurityPolicy", "setSecurityPolicy",
		func() any { return &computev1.SecurityPolicyReference{} },
		func(r *http.Request, sc scope, obj, req any) error {
			obj.(*computev1.BackendService).SecurityPolicy = req.(*computev1.SecurityPolicyReference).SecurityPolicy
			return nil
		})
	m["setEdgeSecurityPolicy"] = s.mutateH(k, "setEdgeSecurityPolicy", "setEdgeSecurityPolicy",
		func() any { return &computev1.SecurityPolicyReference{} },
		func(r *http.Request, sc scope, obj, req any) error {
			obj.(*computev1.BackendService).EdgeSecurityPolicy = req.(*computev1.SecurityPolicyReference).SecurityPolicy
			return nil
		})
	return m
}

func (s *Service) backendBucketMethods() map[string]http.HandlerFunc {
	k := kindBackendBucket
	m := s.signedKeyMethods(k)
	m["setEdgeSecurityPolicy"] = s.mutateH(k, "setEdgeSecurityPolicy", "setEdgeSecurityPolicy",
		func() any { return &computev1.SecurityPolicyReference{} },
		func(r *http.Request, sc scope, obj, req any) error {
			obj.(*computev1.BackendBucket).EdgeSecurityPolicy = req.(*computev1.SecurityPolicyReference).SecurityPolicy
			return nil
		})
	return m
}

// getHealth implements backendServices.getHealth (FR-LB-007): the health
// of every endpoint of one backend group.
func (s *Service) getHealth(w http.ResponseWriter, r *http.Request) {
	_, path, obj, err := s.loadReq(r, kindBackendService, "get")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var ref computev1.ResourceGroupReference
	if _, err := decode(r, &ref); err != nil {
		apierr.Write(w, err)
		return
	}
	b := obj.(*computev1.BackendService)
	group := relPath(ref.Group)
	if strings.HasPrefix(group, "zones/") || strings.HasPrefix(group, "regions/") {
		group = "projects/" + scopeOfPath(path).project + "/" + group
	}
	found := false
	for _, be := range b.Backends {
		if relPath(be.Group) == group {
			found = true
		}
	}
	if !found {
		apierr.Write(w, errInvalid("group", ref.Group, "The backend service does not have this group as a backend."))
		return
	}
	out := &computev1.BackendServiceGroupHealth{Kind: "compute#backendServiceGroupHealth"}
	out.HealthStatus = s.dp.groupHealth(r.Context(), path, group)
	writeJSON(w, http.StatusOK, out)
}

// cdnServicePolicy returns the policy handed to the cache: request
// coalescing is on (GCP's default; the stored resource cannot distinguish
// an explicit false from unset).
func cdnServicePolicy(p *computev1.BackendServiceCdnPolicy) *computev1.BackendServiceCdnPolicy {
	if p == nil {
		return nil
	}
	cp := *p
	cp.RequestCoalescing = true
	return &cp
}

func cdnBucketPolicy(p *computev1.BackendBucketCdnPolicy) *computev1.BackendBucketCdnPolicy {
	if p == nil {
		return nil
	}
	cp := *p
	cp.RequestCoalescing = true
	return &cp
}
