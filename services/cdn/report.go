package cdn

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The Web console's Cloud CDN view (SRS 4.8.3) reads what GCP reports only
// through Cloud Monitoring and logs, on the admin API:
//
//	GET /_emu/v1/cdn[?project=P]              usage, and each backend's entries, bytes and hit ratio
//	GET /_emu/v1/cdn/entries?backend=B[&limit=N]  a backend's cached entries

// counts are one backend's {cdn_cache_status} totals since start or Reset.
type counts struct{ hit, miss, revalidated, uncacheable atomic.Int64 }

// count records one Serve verdict.
func (c *Cache) count(backendID, status string) {
	v, ok := c.counts.Load(backendID)
	if !ok {
		v, _ = c.counts.LoadOrStore(backendID, &counts{})
	}
	n := v.(*counts)
	switch status {
	case StatusHit:
		n.hit.Add(1)
	case StatusMiss:
		n.miss.Add(1)
	case StatusRevalidated:
		n.revalidated.Add(1)
	default:
		n.uncacheable.Add(1)
	}
}

// resetCounts forgets every backend's totals.
func (c *Cache) resetCounts() { c.counts.Clear() }

// BackendStats is one backend's cache usage and traffic.
type BackendStats struct {
	// Backend is the backend service or bucket's resource path.
	Backend     string `json:"backend"`
	Entries     int    `json:"entries"`
	Bytes       int64  `json:"bytes"`
	Hits        int64  `json:"hits"`
	Misses      int64  `json:"misses"`
	Revalidated int64  `json:"revalidated"`
	Uncacheable int64  `json:"uncacheable"`
}

// EntryInfo is one cached response, as the console lists it.
type EntryInfo struct {
	Host        string    `json:"host"`
	Path        string    `json:"path"`
	CacheKey    string    `json:"cacheKey"`
	Status      int       `json:"status"`
	Bytes       int64     `json:"bytes"`
	ContentType string    `json:"contentType,omitempty"`
	Stored      time.Time `json:"stored"`
	Age         int64     `json:"age"`
	TTL         int64     `json:"ttl"`
	Vary        []string  `json:"vary,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	// Tier is "memory" when the body is held in memory, else "disk".
	Tier string `json:"tier"`
}

// relPath turns a selfLink into "projects/..."; other IDs are unchanged.
func relPath(id string) string {
	if i := strings.Index(id, "projects/"); i >= 0 {
		return id[i:]
	}
	return id
}

// inProject reports whether a backend ID belongs to project ("" = any).
func inProject(id, project string) bool {
	return project == "" || strings.HasPrefix(relPath(id), "projects/"+project+"/")
}

// BackendStats reports each backend with cached entries or traffic, in
// project ("" for all), sorted by backend.
func (c *Cache) BackendStats(project string) []BackendStats {
	by := map[string]*BackendStats{}
	get := func(id string) *BackendStats {
		p := relPath(id)
		if by[p] == nil {
			by[p] = &BackendStats{Backend: p}
		}
		return by[p]
	}
	c.st.mu.Lock()
	for _, e := range c.st.entries {
		if inProject(e.BackendID, project) {
			b := get(e.BackendID)
			b.Entries++
			b.Bytes += e.Size
		}
	}
	c.st.mu.Unlock()
	c.counts.Range(func(k, v any) bool {
		if id := k.(string); inProject(id, project) {
			n, b := v.(*counts), get(id)
			b.Hits += n.hit.Load()
			b.Misses += n.miss.Load()
			b.Revalidated += n.revalidated.Load()
			b.Uncacheable += n.uncacheable.Load()
		}
		return true
	})
	out := make([]BackendStats, 0, len(by))
	for _, b := range by {
		out = append(out, *b)
	}
	slices.SortFunc(out, func(a, b BackendStats) int { return strings.Compare(a.Backend, b.Backend) })
	return out
}

// Entries lists up to limit (0 = all) of a backend's cached responses,
// by host and path, and whether more were left out.
func (c *Cache) Entries(backend string, limit int) ([]EntryInfo, bool) {
	backend = relPath(backend)
	now := c.clock.Now()
	var out []EntryInfo
	c.st.mu.Lock()
	for _, e := range c.st.entries {
		if relPath(e.BackendID) != backend {
			continue
		}
		_, key, _ := strings.Cut(e.Primary, "\x00")
		tier := "disk"
		if e.body != nil || e.File == "" {
			tier = "memory"
		}
		out = append(out, EntryInfo{
			Host: e.Host, Path: e.Path, CacheKey: key, Status: e.Status, Bytes: e.Size,
			ContentType: e.Header.Get("Content-Type"), Stored: e.Stored, Age: e.age(now), TTL: e.TTL,
			Vary: e.VaryNames, Tags: e.Tags, Tier: tier,
		})
	}
	c.st.mu.Unlock()
	slices.SortFunc(out, func(a, b EntryInfo) int {
		return cmp.Or(strings.Compare(a.Host, b.Host), strings.Compare(a.Path, b.Path), strings.Compare(a.CacheKey, b.CacheKey))
	})
	if limit > 0 && len(out) > limit {
		return out[:limit], true
	}
	return out, false
}

// maxEntries is the default and largest page of GET /_emu/v1/cdn/entries.
const maxEntries = 1000

// serveStats is GET /_emu/v1/cdn.
func (s *Service) serveStats(w http.ResponseWriter, r *http.Request) {
	n, used := s.cache.Stats()
	writeJSON(w, map[string]any{"entries": n, "bytes": used, "limitBytes": s.cache.Limit(),
		"backends": s.cache.BackendStats(r.URL.Query().Get("project"))})
}

// serveEntries is GET /_emu/v1/cdn/entries.
func (s *Service) serveEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	backend := q.Get("backend")
	if backend == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "backend is required, e.g. projects/P/global/backendBuckets/B"})
		return
	}
	limit := maxEntries
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < maxEntries {
			limit = n
		}
	}
	entries, more := s.cache.Entries(backend, limit)
	if entries == nil {
		entries = []EntryInfo{}
	}
	writeJSON(w, map[string]any{"entries": entries, "truncated": more})
}
