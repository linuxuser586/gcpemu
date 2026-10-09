// Package cdn emulates Cloud CDN (SRS 5.9): the HTTP cache the load
// balancer puts in front of CDN-enabled backend services and backend
// buckets. It implements Cloud CDN's cache modes, TTL settings, cache key
// policies, negative caching, serve-while-stale, request coalescing, byte
// ranges, revalidation, signed URLs/cookies and invalidation, over a
// size-limited LRU store with a memory and a disk tier.
//
// The service itself only owns the Cache and its admin endpoints; lb calls
// (*Cache).Serve for CDN-enabled backends (env.Lookup("cdn") and
// interface{ Cache() *cdn.Cache }) and (*Cache).Invalidate for
// urlMaps.invalidateCache.
package cdn

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/emu"
)

// Name is the service name.
const Name = "cdn"

// Service is the Cloud CDN service module.
type Service struct {
	env   *emu.Env
	log   *slog.Logger
	cache *Cache
}

// New returns the service. The cache exists from construction so lb can
// obtain it before cdn starts; its disk tier lives under the service data
// directory and is rebuilt on restart in --data-dir mode.
func New(env *emu.Env) emu.Service {
	s := &Service{env: env, log: env.Log.With("service", Name)}
	limit, err := ParseSize(env.Config.CDNCacheSize)
	if err != nil {
		s.log.Warn("invalid cdnCacheSize; using default", "value", env.Config.CDNCacheSize, "err", err)
		limit = DefaultCacheSize
	}
	dir, err := env.ServiceDir(Name)
	if err != nil {
		s.log.Warn("cdn disk tier unavailable; caching in memory", "err", err)
		dir = ""
	}
	if s.cache, err = NewCache(Options{Dir: dir, MaxBytes: limit, Clock: env.Clock}); err != nil {
		s.log.Warn("cdn cache index rebuild failed", "err", err)
	}
	return s
}

// Cache returns the shared CDN cache (the CDN ⇄ LB contract).
func (s *Service) Cache() *Cache { return s.cache }

// Name implements emu.Service.
func (s *Service) Name() string { return Name }

// Register mounts the admin endpoints:
//
//	POST /_emu/v1/cdn/purge    clears the cache (`gcpemu cdn purge`)
//	GET  /_emu/v1/cdn          reports entries, bytes used and the limit, and each backend's
//	GET  /_emu/v1/cdn/entries  lists a backend's cached entries
//
// Purging keeps the backends' hit and miss totals; Reset clears them.
func (s *Service) Register(r emu.Router) error {
	r.Handle("POST /_emu/v1/cdn/purge", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := s.cache.purge()
		s.log.Info("cache purged", "entries", n)
		writeJSON(w, map[string]any{"purged": n})
	}))
	r.Handle("GET /_emu/v1/cdn", http.HandlerFunc(s.serveStats))
	r.Handle("GET /_emu/v1/cdn/entries", http.HandlerFunc(s.serveEntries))
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Start implements emu.Service.
func (s *Service) Start(ctx context.Context) error { return nil }

// Stop waits for background revalidations.
func (s *Service) Stop(ctx context.Context) error {
	s.cache.Wait()
	return nil
}

// Ready implements emu.Service.
func (s *Service) Ready() error { return nil }

// Reset clears the cache and the backends' totals on `gcpemu reset`.
func (s *Service) Reset(ctx context.Context) error {
	s.cache.Purge()
	s.cache.resetCounts()
	return nil
}

// ParseSize parses a size such as "1GiB", "512MiB", "100MB", "64k" or a
// plain byte count. Empty means DefaultCacheSize.
func ParseSize(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultCacheSize, nil
	}
	i := len(v)
	for i > 0 && (v[i-1] < '0' || v[i-1] > '9') {
		i--
	}
	num, unit := strings.TrimSpace(v[:i]), strings.ToLower(strings.TrimSpace(v[i:]))
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q", v)
	}
	mult := map[string]int64{
		"": 1, "b": 1,
		"k": 1 << 10, "kb": 1000, "kib": 1 << 10,
		"m": 1 << 20, "mb": 1000 * 1000, "mib": 1 << 20,
		"g": 1 << 30, "gb": 1000 * 1000 * 1000, "gib": 1 << 30,
		"t": 1 << 40, "tb": 1000 * 1000 * 1000 * 1000, "tib": 1 << 40,
	}[unit]
	if mult == 0 {
		return 0, fmt.Errorf("invalid size unit in %q", v)
	}
	return n * mult, nil
}
