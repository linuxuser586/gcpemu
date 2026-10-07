package apierr

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestEnvelope is FR-CORE-025: REST errors use Google's JSON envelope with
// the legacy errors[] item and an ErrorInfo detail; gRPC errors carry the
// same ErrorInfo.
func TestEnvelope(t *testing.T) {
	e := PermissionDenied("Permission %q denied.", "storage.buckets.get").
		WithReason("iam.googleapis.com", "IAM_PERMISSION_DENIED").WithLegacy("forbidden")
	e.Metadata = map[string]string{"permission": "storage.buckets.get"}
	w := httptest.NewRecorder()
	Write(w, e)
	if w.Code != 403 || w.Header().Get("Content-Type") != "application/json; charset=UTF-8" {
		t.Fatalf("status %d, headers %v", w.Code, w.Header())
	}
	var got struct {
		Error struct {
			Code    int
			Message string
			Status  string
			Errors  []struct{ Message, Domain, Reason string }
			Details []map[string]any
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	g := got.Error
	if g.Code != 403 || g.Status != "PERMISSION_DENIED" || g.Message != `Permission "storage.buckets.get" denied.` ||
		len(g.Errors) != 1 || g.Errors[0].Reason != "forbidden" || g.Errors[0].Domain != "global" ||
		len(g.Details) != 1 || g.Details[0]["@type"] != "type.googleapis.com/google.rpc.ErrorInfo" ||
		g.Details[0]["reason"] != "IAM_PERMISSION_DENIED" || g.Details[0]["domain"] != "iam.googleapis.com" {
		t.Fatalf("envelope = %s", w.Body.Bytes())
	}

	st := status.Convert(e)
	if st.Code() != codes.PermissionDenied || len(st.Details()) != 1 {
		t.Fatalf("grpc status = %v", st)
	}
	if info, ok := st.Details()[0].(*errdetails.ErrorInfo); !ok || info.Reason != "IAM_PERMISSION_DENIED" || info.Metadata["permission"] != "storage.buckets.get" {
		t.Fatalf("ErrorInfo = %v", st.Details()[0])
	}

	// Without a reason: the code's legacy reason, no details.
	w = httptest.NewRecorder()
	Write(w, NotFound("nope"))
	var plain struct {
		Error struct {
			Errors  []struct{ Reason string }
			Details []any
		}
	}
	if w.Code != 404 || json.Unmarshal(w.Body.Bytes(), &plain) != nil || plain.Error.Errors[0].Reason != "notFound" || len(plain.Error.Details) != 0 {
		t.Fatalf("plain = %d %s", w.Code, w.Body.Bytes())
	}
}

func TestFromAndHTTP(t *testing.T) {
	if From(nil) != nil {
		t.Error("From(nil)")
	}
	wrapped := errors.Join(errors.New("ctx"), AlreadyExists("x"))
	if e := From(wrapped); e.Code != codes.AlreadyExists || e.HTTP() != http.StatusConflict {
		t.Errorf("wrapped = %+v", e)
	}
	if e := From(status.Error(codes.ResourceExhausted, "slow down")); e.Code != codes.ResourceExhausted || e.HTTP() != 429 || e.Message != "slow down" {
		t.Errorf("grpc = %+v", e)
	}
	if e := From(errors.New("boom")); e.Code != codes.Internal || e.HTTP() != 500 {
		t.Errorf("plain = %+v", e)
	}
	if e := FailedPrecondition("x").WithHTTP(412); e.HTTP() != 412 {
		t.Errorf("WithHTTP = %d", e.HTTP())
	}
	for c, want := range map[codes.Code]int{
		codes.InvalidArgument: 400, codes.FailedPrecondition: 400, codes.OutOfRange: 400, codes.Unauthenticated: 401,
		codes.PermissionDenied: 403, codes.NotFound: 404, codes.Aborted: 409, codes.AlreadyExists: 409,
		codes.ResourceExhausted: 429, codes.Canceled: 499, codes.Internal: 500, codes.Unimplemented: 501,
		codes.Unavailable: 503, codes.DeadlineExceeded: 504,
	} {
		if got := HTTPFromCode(c); got != want {
			t.Errorf("HTTPFromCode(%s) = %d, want %d", c, got, want)
		}
	}
	if CodeName(codes.NotFound) != "NOT_FOUND" || CodeName(codes.FailedPrecondition) != "FAILED_PRECONDITION" {
		t.Error("CodeName")
	}
}
