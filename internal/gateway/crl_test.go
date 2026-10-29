package gateway

import (
	"context"
	"crypto/x509"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/ca"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// TestCRL: the API gateway serves the Instance CA's empty CRL at /ca.crl
// to anyone on its loopback Host, ahead of the Admin token and any
// Service's fallback, and not to the Google front end (ADR 0046).
func TestCRL(t *testing.T) {
	authority, err := ca.Load(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	g := New(&emu.Env{Config: &config.Config{}, Log: slog.New(slog.DiscardHandler), CA: authority}, nil)
	g.SetAdminToken("secret")
	g.Router("svc").Fallback(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))

	fetch := func() *x509.RevocationList {
		t.Helper()
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4443/ca.crl", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET /ca.crl = %d %s", w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/pkix-crl" {
			t.Errorf("Content-Type = %q, want application/pkix-crl", ct)
		}
		l, err := x509.ParseRevocationList(w.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	first := fetch()
	if len(first.RevokedCertificateEntries) != 0 {
		t.Errorf("CRL revokes %d certificates", len(first.RevokedCertificateEntries))
	}
	if err := first.CheckSignatureFrom(authority.Certificate()); err != nil {
		t.Errorf("CRL signature: %v", err)
	}
	if d := first.NextUpdate.Sub(first.ThisUpdate); d != ca.CRLValidity {
		t.Errorf("NextUpdate - ThisUpdate = %v, want %v", d, ca.CRLValidity)
	}
	if second := fetch(); second.Number.Cmp(first.Number) <= 0 {
		t.Errorf("second CRL number %v is not above %v", second.Number, first.Number)
	}

	old := authority.Certificate()
	if err := authority.Rotate("test"); err != nil {
		t.Fatal(err)
	}
	rotated := fetch()
	if rotated.CheckSignatureFrom(authority.Certificate()) != nil || rotated.CheckSignatureFrom(old) == nil {
		t.Error("CRL after rotation is not signed by the new CA alone")
	}

	r := httptest.NewRequest(http.MethodGet, "https://www.googleapis.com/ca.crl", nil)
	r = r.WithContext(context.WithValue(r.Context(), frontKey{}, true))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Error("the Google front end served the CRL")
	}
}
