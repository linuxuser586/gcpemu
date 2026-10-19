package cdn

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/linuxuser586/gcpemu/internal/clock"
)

// A background revalidation in flight across a Reset stores nothing
// afterwards: the response it fetched for the cache before the Reset is
// not served after it (#307).
func TestResetWaitsForRevalidation(t *testing.T) {
	env := newEnv(t, t.TempDir(), "64MiB")
	clk := clock.NewOffset(clock.Real{})
	env.Clock = clk
	s := New(env).(*Service)
	c, b := s.Cache(), svc(UseOriginHeaders)
	const cc = "max-age=10, stale-while-revalidate=30"
	o := &origin{serve: static(200, "v1", "Cache-Control", cc)}
	do(c, b, o, get("http://h/swr"))

	// Stale: the next request triggers a revalidation, held at the origin.
	clk.Advance(15 * time.Second)
	entered, release := make(chan struct{}), make(chan struct{})
	o.serve = func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		static(200, "old", "Cache-Control", cc)(w, r)
	}
	do(c, b, o, get("http://h/swr"))
	<-entered

	done := make(chan error, 1)
	go func() { done <- s.Reset(context.Background()) }()
	select {
	case err := <-done:
		close(release)
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		close(release) // Reset is waiting for the revalidation
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	c.Wait()
	o.serve = static(200, "new", "Cache-Control", cc)
	if w, st := do(c, b, o, get("http://h/swr")); st != StatusMiss || w.Body.String() != "new" {
		t.Fatalf("after Reset: %s %q, want a miss serving the origin's new response", st, w.Body.String())
	}
}

// After a Reset stale entries are revalidated in the background again
// (#307).
func TestRevalidationRunsAfterReset(t *testing.T) {
	env := newEnv(t, t.TempDir(), "64MiB")
	clk := clock.NewOffset(clock.Real{})
	env.Clock = clk
	s := New(env).(*Service)
	if err := s.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, b := s.Cache(), svc(UseOriginHeaders)
	const cc = "max-age=10, stale-while-revalidate=30"
	o := &origin{serve: static(200, "v1", "Cache-Control", cc)}
	do(c, b, o, get("http://h/swr"))
	clk.Advance(15 * time.Second)
	o.serve = static(200, "v2", "Cache-Control", cc)
	do(c, b, o, get("http://h/swr"))
	c.Wait()
	if w, st := do(c, b, o, get("http://h/swr")); st != StatusHit || w.Body.String() != "v2" {
		t.Fatalf("after background refresh: %s %q", st, w.Body.String())
	}
}
