package gateway

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// linkedTokenGateway is a gateway under IAM mode whose links answer every
// access token with principal, and unreachable.
func linkedTokenGateway(t *testing.T, mode, principal string, unreachable map[string]error) *Gateway {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	g := New(&emu.Env{Config: &config.Config{DefaultPrincipal: "user:default@example.com"}, Auth: emu.NewPolicyAuthorizer(mode, log), Log: log}, nil)
	g.SetLinks(fakeLinks{delegated: map[string]string{"team-shared": "office"}, principal: principal, unreachable: unreachable})
	return g
}

// bearerREST serves GET /v1/things with the access token tok through g,
// returning the status, body and the Principal the handler ran as.
func bearerREST(g *Gateway, tok string) (int, string, emu.Principal) {
	var ran emu.Principal
	h := g.Middleware("svc", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { ran = emu.PrincipalFrom(r.Context()) }))
	r := httptest.NewRequest(http.MethodGet, "/v1/things", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String(), ran
}

const linkedTok = "ya29.gcpemu.issued-elsewhere"

// With no Linked Instance claiming an access token and a link unreachable,
// a request with it fails UNAVAILABLE naming the link and why, in every
// IAM mode, and never runs as the default Principal (#733).
func TestLinkedTokenWithUnreachableLink(t *testing.T) {
	down := map[string]error{"office": errors.New(`link "office" does not answer: connection refused`)}
	for _, mode := range []string{config.IAMEnforce, config.IAMAudit, config.IAMOff} {
		g := linkedTokenGateway(t, mode, "", down)
		code, body, ran := bearerREST(g, linkedTok)
		if code != http.StatusServiceUnavailable || !strings.Contains(body, `\"office\"`) || !strings.Contains(body, "connection refused") || ran != "" {
			t.Errorf("%s: REST with the office unreachable = %d %s, ran as %q; want 503 naming the office and why", mode, code, body, ran)
		}
		conn := serveGated(t, g, make(chan struct{}, 1))
		ctx := metadata.AppendToOutgoingContext(t.Context(), "authorization", "Bearer "+linkedTok)
		err := callGated(ctx, conn)
		if s, _ := status.FromError(err); s.Code() != codes.Unavailable || !strings.Contains(s.Message(), `"office"`) {
			t.Errorf("%s: gRPC with the office unreachable = %v, want UNAVAILABLE naming the office", mode, err)
		}
	}
}

// A token a Linked Instance claims runs as its Principal, and when every
// link was reached and none claims it, a request is 401 under enforcement
// and the default Principal's otherwise (#733).
func TestLinkedTokenAnswered(t *testing.T) {
	if code, body, ran := bearerREST(linkedTokenGateway(t, config.IAMEnforce, "serviceAccount:runner@team-shared.iam.gserviceaccount.com", nil), linkedTok); code != http.StatusOK || ran != "serviceAccount:runner@team-shared.iam.gserviceaccount.com" {
		t.Errorf("REST with a claimed token = %d %s, ran as %q", code, body, ran)
	}
	for mode, want := range map[string]int{config.IAMEnforce: http.StatusUnauthorized, config.IAMAudit: http.StatusOK, config.IAMOff: http.StatusOK} {
		code, body, ran := bearerREST(linkedTokenGateway(t, mode, "", nil), linkedTok)
		if code != want || (want == http.StatusOK) != (ran == "user:default@example.com") {
			t.Errorf("%s: REST with a token no link claims = %d %s, ran as %q; want %d", mode, code, body, ran, want)
		}
	}
}
