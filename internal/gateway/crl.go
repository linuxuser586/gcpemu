package gateway

import (
	"net"
	"net/http"

	"github.com/linuxuser586/gcpemu/internal/ca"
)

// crlRequest reports whether r asks for the Instance CA's CRL (ADR 0046):
// a request for ca.CRLPath on the API gateway's own listener, whose Host is
// the IP address leaves name. It needs no credentials and no Admin token,
// and the Google front end doesn't serve it.
func (g *Gateway) crlRequest(r *http.Request) bool {
	if r.URL.Path != ca.CRLPath || g.env.CA == nil || isGRPC(r) || viaFrontend(r) {
		return false
	}
	return net.ParseIP(hostOnly(r.Host)) != nil
}

// serveCRL answers GET /ca.crl with an empty CRL signed now.
func (g *Gateway) serveCRL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	der, err := g.env.CA.CRL()
	if err != nil {
		http.Error(w, "sign CRL: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/pkix-crl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(der)
}
