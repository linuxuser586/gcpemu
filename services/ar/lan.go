package ar

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/gateway"
)

// --ar-bind (Config.ARBind): the registry is also served on each
// configured address, on the registry's own port when it is free there
// and on a free one otherwise, recorded as endpoint "ar-lan".
//
// Those listeners differ from the loopback one in three ways:
//
//   - They speak TLS, with a certificate from the instance CA covering
//     every --ar-bind address, so clients on other machines trust
//     ca.pem rather than an insecure registry.
//   - Every request but the token endpoint must carry credentials that
//     authenticate, whatever the IAM enforcement level, and the token
//     endpoint never hands out the anonymous token. IAM permissions are
//     then checked at the enforcement level as usual.
//   - Clients are challenged for Basic credentials, which docker,
//     containerd (BuildKit) and crane resend on every request. A Bearer
//     realm on a private IP literal is refused by go-containerregistry
//     (crane, ko) as an SSRF guard, and LAN addresses are private.

// lanKey marks the context of a request that arrived on an --ar-bind
// listener.
type lanKey struct{}

func isLAN(ctx context.Context) bool { v, _ := ctx.Value(lanKey{}).(bool); return v }

// authRequired reports whether a request must authenticate: in enforce
// mode, and always on an --ar-bind listener.
func (s *Service) authRequired(ctx context.Context) bool { return s.enforcing() || isLAN(ctx) }

// lanHandler wraps the registry handler for the --ar-bind listeners.
func (s *Service) lanHandler(next http.Handler) http.Handler {
	guard := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/token" {
			tok, ok := cutBearer(r.Header.Get("Authorization"))
			if ok {
				_, ok = s.env.Auth.Authenticate(r.Context(), tok)
			}
			if !ok {
				w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
				s.challenge(w, r, "", "")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
	auth := s.withRegistryAuth(guard)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), lanKey{}, true)))
	})
}

// LANHandler returns the registry as the --ar-bind listeners serve it,
// requiring credentials, or nil while the registry isn't running. The
// Google front end's --gfe-bind listeners serve LOCATION-docker.pkg.dev
// with it (ADR 0021).
func (s *Service) LANHandler() http.Handler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lanH
}

// startLAN serves h on every --ar-bind address. A failed address is
// logged and skipped.
func (s *Service) startLAN(ctx context.Context, port int, h http.Handler) []*http.Server {
	binds := s.env.Config.ARBind
	if len(binds) == 0 {
		return nil
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.env.CA.ServerCert(binds...)
		},
	}
	var srvs []*http.Server
	var addrs []string
	for _, ip := range binds {
		l, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
		if err != nil {
			if why := s.env.ExplainBind(ctx, err); why != err {
				s.env.Log.Warn("ar: cannot serve the registry on --ar-bind address", "address", ip, "err", why)
				continue
			}
			s.env.Log.Warn("ar: registry port taken on --ar-bind address; using a free one", "address", ip, "port", port, "err", err)
			if l, err = net.Listen("tcp", net.JoinHostPort(ip, "0")); err != nil {
				s.env.Log.Warn("ar: cannot serve the registry on --ar-bind address", "address", ip, "err", err)
				continue
			}
		}
		srv := gateway.NewServer(h, s.env.Log)
		srv.TLSConfig = cfg
		srvs = append(srvs, srv)
		addrs = append(addrs, l.Addr().String())
		go func() {
			if err := srv.ServeTLS(l, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.env.Log.Error("ar registry stopped on --ar-bind address", "address", l.Addr().String(), "err", err)
			}
		}()
		s.env.Log.Info("ar: registry exposed", "address", l.Addr().String())
	}
	if len(addrs) > 0 {
		sort.Strings(addrs)
		s.env.Endpoints.Set("ar-lan", strings.Join(addrs, ","))
	}
	return srvs
}

// linkURI returns uri, an image or repository URI on LOCATION-docker.pkg.dev,
// as a caller that came over a link must use it (ADR 0027): behind the
// --ar-bind address it should be told, in the location-in-path form, since
// on its machine pkg.dev names reach the front Instance instead. Any other
// caller, or a link to an Instance without --ar-bind, keeps uri.
func (s *Service) linkURI(ctx context.Context, uri string) string {
	if _, _, ok := emu.AssertedBy(ctx); !ok || uri == "" {
		return uri
	}
	var served []string
	if v := s.env.Endpoints.Get("ar-lan"); v != "" {
		served = strings.Split(v, ",")
	}
	if addr, _ := emu.LANServed(ctx, served); addr != "" {
		return addr + "/" + uri
	}
	return uri
}
