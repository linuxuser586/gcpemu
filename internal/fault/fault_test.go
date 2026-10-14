package fault

import (
	"testing"

	"google.golang.org/grpc/codes"
)

func TestMatch(t *testing.T) {
	var s Set
	if _, err := s.Add(Rule{Service: "gcs", Method: "^POST ", Code: "UNAVAILABLE", Count: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(Rule{Code: "BOGUS"}); err == nil {
		t.Error("expected bad code error")
	}
	if a := s.Match("pubsub", "POST /x", ""); a != nil {
		t.Error("matched wrong service")
	}
	if a := s.Match("gcs", "GET /x", ""); a != nil {
		t.Error("matched wrong method")
	}
	for i := 0; i < 2; i++ {
		a := s.Match("gcs", "POST /upload", "")
		if a == nil || a.Err == nil || a.Err.Code != codes.Unavailable {
			t.Fatalf("fire %d: %+v", i, a)
		}
	}
	if a := s.Match("gcs", "POST /upload", ""); a != nil {
		t.Error("count limit not honoured")
	}
	if got := s.List()[0].Fired; got != 2 {
		t.Errorf("Fired = %d", got)
	}
	if _, ok := s.SetDisabled("nope", true); ok {
		t.Error("disabled a missing rule")
	}
	if _, err := s.Add(Rule{ID: "slow", Latency: 1}); err != nil {
		t.Fatal(err)
	}
	if r, ok := s.SetDisabled("slow", true); !ok || !r.Disabled {
		t.Fatalf("SetDisabled = %+v, %v", r, ok)
	}
	if a := s.Match("gcs", "GET /x", ""); a != nil {
		t.Error("disabled rule fired")
	}
	if r, _ := s.SetDisabled("slow", false); r.Disabled {
		t.Error("rule still disabled")
	}
	if a := s.Match("gcs", "GET /x", ""); a == nil || a.Latency != 1 {
		t.Errorf("re-enabled rule: %+v", a)
	}
	if _, err := s.Add(Rule{ID: "slow", Drop: true}); err == nil {
		t.Error("added a second rule with ID slow")
	}
	if _, err := s.Add(Rule{ID: "fault-2", Drop: true}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Add(Rule{Drop: true}); err != nil || r.ID != "fault-3" {
		t.Errorf("generated ID = %q, %v; want fault-3, skipping the taken fault-2", r.ID, err)
	}
	s.Remove("")
	if len(s.List()) != 0 {
		t.Error("clear failed")
	}
}
