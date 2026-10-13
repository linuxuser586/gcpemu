package lb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/linuxuser586/gcpemu/internal/agent"
)

// The "lb-echo" agent is a diagnostic HTTP(S) backend for load balancer
// tests and demos: it answers every path with the pod/host name, the
// request path, the client certificate's common name (mTLS) and selected
// request headers, and /healthz with 200. Configured by environment:
//
//	ECHO_PORT        listen port (default 8080)
//	ECHO_TLS_CERT    PEM certificate chain: serve HTTPS
//	ECHO_TLS_KEY     PEM private key
//	ECHO_CLIENT_CA   PEM CA bundle: require client certificates it signed
//	ECHO_FETCH       "1": also serve /fetch?url=, which GETs url from the
//	                 pod and answers with its status and body, so tests
//	                 can call a load balancer from a pod
func init() { agent.Register("lb-echo", runEcho) }

func runEcho(ctx context.Context, _ []string) error {
	port := os.Getenv("ECHO_PORT")
	if port == "" {
		port = "8080"
	}
	host, _ := os.Hostname()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	if os.Getenv("ECHO_FETCH") == "1" {
		mux.HandleFunc("/fetch", echoFetch)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		cn := "-"
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			cn = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, "pod=%s path=%s client=%s xff=%s\n", host, r.URL.Path, cn, r.Header.Get("X-Forwarded-For"))
	})
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	var err error
	if crt := os.Getenv("ECHO_TLS_CERT"); crt != "" {
		pair, perr := tls.X509KeyPair([]byte(crt), []byte(os.Getenv("ECHO_TLS_KEY")))
		if perr != nil {
			return perr
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{pair}}
		if ca := os.Getenv("ECHO_CLIENT_CA"); ca != "" {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM([]byte(ca))
			srv.TLSConfig.ClientCAs, srv.TLSConfig.ClientAuth = pool, tls.RequireAndVerifyClientCert
		}
		// ECHO_HEALTH_PORT serves plain-HTTP /healthz for health checks.
		if hp := os.Getenv("ECHO_HEALTH_PORT"); hp != "" {
			go func() {
				hs := &http.Server{Addr: ":" + hp, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
				go func() { <-ctx.Done(); hs.Close() }()
				_ = hs.ListenAndServe()
			}()
		}
		err = srv.ListenAndServeTLS("", "")
	} else {
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// echoFetch GETs ?url= and answers "status=<code>\n<body>", or 502 with
// the error.
func echoFetch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL.Query().Get("url"), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "status=%d\n%s", resp.StatusCode, b)
}
