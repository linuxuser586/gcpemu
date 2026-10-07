// Package fault implements fault injection rules (FR-CORE-060): inject an
// error code, latency or a dropped connection by service, method, resource
// pattern and probability or count.
package fault

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"sync"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Rule is one fault-injection rule. Empty matchers match everything.
type Rule struct {
	ID string `json:"id" yaml:"id"`
	// Service is the emulator service name ("gcs", "pubsub", ...).
	Service string `json:"service,omitempty" yaml:"service"`
	// Method is a regexp matched against the method: "GET /storage/v1/b"
	// for HTTP, "/google.pubsub.v1.Publisher/Publish" for gRPC.
	Method string `json:"method,omitempty" yaml:"method"`
	// Resource is a regexp matched against the request path/resource.
	Resource string `json:"resource,omitempty" yaml:"resource"`
	// Code is the canonical error name to return (e.g. "UNAVAILABLE").
	Code string `json:"code,omitempty" yaml:"code"`
	// HTTPStatus overrides the HTTP status for Code.
	HTTPStatus int `json:"httpStatus,omitempty" yaml:"httpStatus"`
	// Latency is added before the request is handled (or the error returned).
	Latency Duration `json:"latency,omitempty" yaml:"latency"`
	// Drop closes the connection without a response.
	Drop bool `json:"drop,omitempty" yaml:"drop"`
	// Probability in (0,1]; 0 means always.
	Probability float64 `json:"probability,omitempty" yaml:"probability"`
	// Count limits how many times the rule fires; 0 means unlimited.
	Count int `json:"count,omitempty" yaml:"count"`
	// Fired is how many times the rule has fired.
	Fired int `json:"fired" yaml:"-"`

	method, resource *regexp.Regexp
}

// Duration is a time.Duration that (un)marshals as a Go duration string.
type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	*d = Duration(v)
	return err
}

// Action is what to do for a matched request.
type Action struct {
	Latency time.Duration
	Err     *apierr.Error
	Drop    bool
}

// Set is a concurrency-safe ordered rule list.
type Set struct {
	mu    sync.Mutex
	rules []*Rule
	n     int
}

// Add validates and appends r, assigning an ID if empty.
func (s *Set) Add(r Rule) (Rule, error) {
	var err error
	if r.Method != "" {
		if r.method, err = regexp.Compile(r.Method); err != nil {
			return r, fmt.Errorf("method: %w", err)
		}
	}
	if r.Resource != "" {
		if r.resource, err = regexp.Compile(r.Resource); err != nil {
			return r, fmt.Errorf("resource: %w", err)
		}
	}
	if r.Code != "" {
		if _, ok := codeByName[r.Code]; !ok {
			return r, fmt.Errorf("unknown code %q", r.Code)
		}
	}
	if r.Probability < 0 || r.Probability > 1 {
		return r, fmt.Errorf("probability must be in [0,1]")
	}
	if r.Code == "" && r.Latency == 0 && !r.Drop {
		return r, fmt.Errorf("rule needs code, latency or drop")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if r.ID == "" {
		r.ID = fmt.Sprintf("fault-%d", s.n)
	}
	rc := r
	s.rules = append(s.rules, &rc)
	return r, nil
}

// List returns a copy of all rules.
func (s *Set) List() []Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Rule, len(s.rules))
	for i, r := range s.rules {
		out[i] = *r
	}
	return out
}

// Remove deletes a rule by ID; empty id clears all.
func (s *Set) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		s.rules = nil
		return true
	}
	for i, r := range s.rules {
		if r.ID == id {
			s.rules = append(s.rules[:i], s.rules[i+1:]...)
			return true
		}
	}
	return false
}

// Match returns the action of the first rule that fires, or nil.
func (s *Set) Match(service, method, resource string) *Action {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rules {
		if r.Service != "" && r.Service != service {
			continue
		}
		if r.method != nil && !r.method.MatchString(method) {
			continue
		}
		if r.resource != nil && !r.resource.MatchString(resource) {
			continue
		}
		if r.Count > 0 && r.Fired >= r.Count {
			continue
		}
		if r.Probability > 0 && rand.Float64() >= r.Probability {
			continue
		}
		r.Fired++
		a := &Action{Latency: time.Duration(r.Latency), Drop: r.Drop}
		if r.Code != "" {
			a.Err = &apierr.Error{
				Code:       codeByName[r.Code],
				Message:    fmt.Sprintf("Injected fault %s.", r.ID),
				HTTPStatus: r.HTTPStatus,
				Reason:     "INJECTED_FAULT",
				Domain:     "gcpemu.dev",
			}
		}
		return a
	}
	return nil
}

var codeByName = func() map[string]codes.Code {
	m := map[string]codes.Code{}
	for c := codes.OK; c <= codes.Unauthenticated; c++ {
		m[apierr.CodeName(c)] = c
	}
	return m
}()
