package lb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/services/lb/urlmap"
)

// URL maps (global and regional): validation per GCP (urlmap.Validate plus
// existence of every referenced backend), tests[] run on insert/update,
// urlMaps.validate and urlMaps.invalidateCache (FR-LB-003, FR-CDN-005).

var kindURLMap = &kind{
	coll: "urlMaps", typ: "compute#urlMap", snake: "url_map",
	gperm: "compute.urlMaps", rperm: "compute.regionUrlMaps",
	aggKind: "compute#urlMapsAggregatedList",
	newObj:  func() any { return &computev1.UrlMap{} },
	refs:    urlMapRefs,
}

func urlMapRefs(obj any) []string {
	m := obj.(*computev1.UrlMap)
	var out []string
	cp := *m
	cp.Tests = nil
	_ = urlmap.WalkServices(&cp, func(_ string, ref *string) error {
		out = append(out, *ref)
		return nil
	})
	return out
}

// prepareURLMap validates m, canonicalises its backend references and,
// when runTests, runs its tests.
func (s *Service) prepareURLMap(sc scope, m *computev1.UrlMap, runTests bool) error {
	backends := []*kind{kindBackendService, kindBackendBucket}
	if sc.region != "" {
		backends = []*kind{kindBackendService}
	}
	if err := urlmap.WalkServices(m, func(field string, ref *string) error {
		l, _, err := s.refExisting(sc, *ref, field, backends...)
		if err != nil {
			return err
		}
		*ref = l
		return nil
	}); err != nil {
		return err
	}
	if errs := urlmap.Validate(m); len(errs) > 0 {
		return apierr.InvalidArgument("%s", errs[0]).WithLegacy("invalid")
	}
	if runTests {
		if f := urlmap.RunTests(m, sameRef); len(f) > 0 {
			return apierr.InvalidArgument("Invalid value for field 'resource.tests': %s", describeFailure(f[0])).WithLegacy("invalid")
		}
	}
	return nil
}

func sameRef(a, b string) bool { return relPath(a) == relPath(b) }

func describeFailure(f *computev1.TestFailure) string {
	switch {
	case f.ExpectedService != "":
		return fmt.Sprintf("Test failure: Expect URL '%s%s' to map to service '%s', but actually mapped to '%s'.", f.Host, f.Path, f.ExpectedService, f.ActualService)
	case f.ExpectedRedirectResponseCode != 0 && f.ExpectedRedirectResponseCode != f.ActualRedirectResponseCode:
		return fmt.Sprintf("Test failure: Expect URL '%s%s' to redirect with response code %d, but actually redirected with %d.", f.Host, f.Path, f.ExpectedRedirectResponseCode, f.ActualRedirectResponseCode)
	}
	return fmt.Sprintf("Test failure: Expect URL '%s%s' to map to '%s', but actually mapped to '%s'.", f.Host, f.Path, f.ExpectedOutputUrl, f.ActualOutputUrl)
}

func (s *Service) urlMapMethods() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"validate":        s.validateURLMap,
		"invalidateCache": s.invalidateCache,
	}
}

// validateURLMap implements urlMaps.validate: it checks the URL map in the
// request (not the stored one) and runs its tests.
func (s *Service) validateURLMap(w http.ResponseWriter, r *http.Request) {
	sc, _, _, err := s.loadReq(r, kindURLMap, "validate")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var req computev1.UrlMapsValidateRequest
	if _, err := decode(r, &req); err != nil {
		apierr.Write(w, err)
		return
	}
	res := &computev1.UrlMapValidationResult{LoadSucceeded: true, TestPassed: true}
	if req.Resource == nil {
		apierr.Write(w, errRequired("resource"))
		return
	}
	if err := s.prepareURLMap(sc, req.Resource, false); err != nil {
		res.LoadSucceeded, res.TestPassed = false, false
		res.LoadErrors = []string{apierr.From(err).Message}
	} else if f := urlmap.RunTests(req.Resource, sameRef); len(f) > 0 {
		res.TestPassed = false
		res.TestFailures = f
	}
	res.ForceSendFields = []string{"LoadSucceeded", "TestPassed"}
	writeJSON(w, http.StatusOK, &computev1.UrlMapsValidateResponse{Result: res})
}

// invalidateCache implements urlMaps.invalidateCache (FR-CDN-005): an
// Operation whose work removes matching entries from the caches of every
// CDN-enabled backend of the URL map before it completes.
func (s *Service) invalidateCache(w http.ResponseWriter, r *http.Request) {
	sc, path, obj, err := s.loadReq(r, kindURLMap, "invalidateCache")
	if err != nil {
		apierr.Write(w, err)
		return
	}
	var rule computev1.CacheInvalidationRule
	if _, err := decode(r, &rule); err != nil {
		apierr.Write(w, err)
		return
	}
	if rule.Path == "" && len(rule.CacheTags) == 0 {
		rule.Path = "/*"
	}
	if rule.Path != "" && !strings.HasPrefix(rule.Path, "/") {
		apierr.Write(w, errInvalid("path", rule.Path, "Path must start with /."))
		return
	}
	if i := strings.Index(rule.Path, "*"); i >= 0 && i != len(rule.Path)-1 {
		apierr.Write(w, errInvalid("path", rule.Path, "A wildcard * is only allowed at the end of the path."))
		return
	}
	m := obj.(*computev1.UrlMap)
	ids := s.cdnBackends(m)
	op, err := s.cmp.StartOperation(r.Context(), sc.project, sc.String(), "invalidateCache", path, m.Id, func(ctx context.Context) error {
		if c := s.cdnCache(); c != nil && len(ids) > 0 {
			c.Invalidate(ids, rule.Host, rule.Path, rule.CacheTags)
		}
		return nil
	})
	reply(w, op, err)
}

// cdnBackends lists the selfLinks of the URL map's backends with Cloud CDN
// enabled.
func (s *Service) cdnBackends(m *computev1.UrlMap) []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range urlMapRefs(m) {
		p := relPath(ref)
		if seen[p] {
			continue
		}
		seen[p] = true
		switch collOf(p) {
		case "backendServices":
			if o, ok := s.load(kindBackendService, p); ok && o.(*computev1.BackendService).EnableCDN {
				out = append(out, o.(*computev1.BackendService).SelfLink)
			}
		case "backendBuckets":
			if o, ok := s.load(kindBackendBucket, p); ok && o.(*computev1.BackendBucket).EnableCdn {
				out = append(out, o.(*computev1.BackendBucket).SelfLink)
			}
		}
	}
	return out
}

// jsonClone deep-copies v into a new value of the same type.
func jsonClone[T any](v *T) *T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return &out
}

func prepareURLMapKind(ctx context.Context, s *Service, sc scope, path string, obj, old any) error {
	return s.prepareURLMap(sc, obj.(*computev1.UrlMap), true)
}
