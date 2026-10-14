package gateway

import (
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestGRPCBody: a unary call's messages are logged as protojson.
func TestGRPCBody(t *testing.T) {
	b := grpcBody(wrapperspb.String("hi"))
	if b == nil || b.Text != `"hi"` || b.ContentType != "application/json" || b.Size != 4 {
		t.Errorf("grpcBody = %+v", b)
	}
	var nilMsg *wrapperspb.StringValue
	if grpcBody(nilMsg) != nil || grpcBody(nil) != nil {
		t.Error("no message has a body")
	}
}
