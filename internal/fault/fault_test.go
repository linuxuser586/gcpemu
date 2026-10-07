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
	s.Remove("")
	if len(s.List()) != 0 {
		t.Error("clear failed")
	}
}
