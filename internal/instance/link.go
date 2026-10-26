package instance

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/link"
	"github.com/linuxuser586/gcpemu/internal/mdns"
)

// newLink returns the Instance's link Manager (ADR 0027). A Persistent
// Instance keeps its Links in its instance dir, outside the state that
// Reset and State snapshots replace; an Ephemeral Instance keeps them in
// memory.
func (in *Instance) newLink() (*link.Manager, error) {
	path := ""
	if !in.Config.Ephemeral {
		path = filepath.Join(in.Dir, link.StoreFile)
	}
	st, err := link.OpenStore(path)
	if err != nil {
		return nil, err
	}
	return link.New(link.Config{
		CA: in.Env.CA, Instance: in.Config.Instance, ID: in.ID, Store: st,
		Log:         in.Env.Log.With("component", "link"),
		StaticAddrs: in.Config.LinkDiscover, MDNS: mdns.Options{Port: in.Config.LinkMDNSPort},
		HasProject:  in.Env.HasProject,
		LBEndpoints: in.lanEndpoints,
		Serve: func(w http.ResponseWriter, r *http.Request, from, principal string) {
			in.gw.ServeLinked(w, r, from, principal)
		},
		Buckets: func() map[string]string {
			if svc, ok := in.Env.Lookup("gcs"); ok {
				if b, ok := svc.(emu.Buckets); ok {
					return b.Buckets()
				}
			}
			return nil
		},
		SigningKey: func() (link.SigningKey, bool) {
			if svc, ok := in.Env.Lookup("iam"); ok {
				if s, ok := svc.(emu.SigningKeyer); ok {
					return link.SigningKey(s.SigningKey()), true
				}
			}
			return link.SigningKey{}, false
		},
		VerifyIDToken: func(token string) (string, bool) {
			if svc, ok := in.Env.Lookup("iam"); ok {
				if v, ok := svc.(emu.IDTokenVerifier); ok {
					return v.VerifyIDToken(token)
				}
			}
			return "", false
		},
		// Only from the tokens this Instance issued: Instances that
		// delegate to each other must not ask each other in circles.
		Authenticate: func(ctx context.Context, token string) (string, bool) {
			if svc, ok := in.Env.Lookup("iam"); ok {
				if a, ok := svc.(emu.LocalAuthenticator); ok {
					p, err := a.AuthenticateLocal(ctx, token)
					return string(p), err == nil
				}
			}
			return "", false
		},
	}), nil
}

// startLink serves the link protocol on every --link-bind address. An
// address that cannot be bound is logged and skipped, as for the other
// --*-bind flags.
func (in *Instance) startLink() {
	port, set := in.Config.PortSet(link.Endpoint)
	if !set {
		port = link.DefaultPort
	}
	var addrs []string
	for _, ip := range in.Config.LinkBind {
		l, err := in.Env.ListenTCP(ip, port)
		if err != nil {
			in.Env.Log.Warn("cannot serve the link protocol on --link-bind address", "address", net.JoinHostPort(ip, strconv.Itoa(port)), "err", in.Env.ExplainBind(context.Background(), err))
			continue
		}
		addrs = append(addrs, l.Addr().String())
		go func() {
			if err := in.link.Serve(l); err != nil {
				in.Env.Log.Error("link listener stopped", "address", l.Addr().String(), "err", err)
			}
		}()
		in.Env.Log.Info("link protocol served to paired Instances", "address", l.Addr().String())
	}
	if len(addrs) > 0 {
		sort.Strings(addrs)
		in.Env.Endpoints.Set(link.Endpoint, strings.Join(addrs, ","))
		// Only an Instance with --link-bind advertises. Without mDNS it is
		// still paired by address.
		if err := in.link.Advertise(addrs); err != nil {
			in.Env.Log.Warn("cannot advertise the link protocol over mDNS; pair with this Instance by address", "err", err)
		}
	}
	in.link.StartWatching()
}

// linkRoutes adds the Admin API's /_emu/v1/links routes, through which
// `gcpemu link` and the Web console pair and unpair Instances.
func (in *Instance) linkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /_emu/v1/links", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"links": in.link.States(), "fingerprint": link.Fingerprint(in.Env.CA.Certificate()),
			"shared": in.link.Shares(), "delegated": in.link.Delegations(),
		})
	})
	mux.HandleFunc("POST /_emu/v1/links/shares", func(w http.ResponseWriter, r *http.Request) {
		var body link.Share
		if !decodeBody(w, r, &body) {
			return
		}
		if err := in.link.Share(body.Project, body.With); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("DELETE /_emu/v1/links/shares/{project}", func(w http.ResponseWriter, r *http.Request) {
		if err := in.link.Unshare(r.PathValue("project"), r.URL.Query().Get("with")); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "unshared"})
	})
	mux.HandleFunc("POST /_emu/v1/links/delegations", func(w http.ResponseWriter, r *http.Request) {
		var body link.Delegation
		if !decodeBody(w, r, &body) {
			return
		}
		d, err := in.link.Delegate(r.Context(), body.Project, body.Link)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, d)
	})
	mux.HandleFunc("DELETE /_emu/v1/links/delegations/{project}", func(w http.ResponseWriter, r *http.Request) {
		if err := in.link.Undelegate(r.PathValue("project")); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "undelegated"})
	})
	mux.HandleFunc("GET /_emu/v1/links/status", func(w http.ResponseWriter, r *http.Request) {
		ps := in.link.Links()
		out := make([]link.Status, len(ps))
		var wg sync.WaitGroup
		for i, p := range ps {
			wg.Go(func() { out[i] = in.link.Check(r.Context(), p.Name) })
		}
		wg.Wait()
		writeJSON(w, http.StatusOK, map[string]any{"links": out})
	})
	mux.HandleFunc("GET /_emu/v1/links/lb-endpoints", in.serveLinkedLB)
	mux.HandleFunc("GET /_emu/v1/links/discovered", func(w http.ResponseWriter, r *http.Request) {
		ds, err := in.link.Discover(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"instances": ds})
	})
	mux.HandleFunc("POST /_emu/v1/links/identify", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Address string }
		if !decodeBody(w, r, &body) {
			return
		}
		id, err := in.link.Identify(r.Context(), body.Address)
		if err != nil {
			linkError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, id)
	})
	// Pairing waits for the other side's approval.
	mux.HandleFunc("POST /_emu/v1/links", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Address, Fingerprint, Name string }
		if !decodeBody(w, r, &body) {
			return
		}
		p, err := in.link.Pair(r.Context(), body.Address, body.Fingerprint, body.Name)
		if err != nil {
			linkError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	})
	mux.HandleFunc("DELETE /_emu/v1/links/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := in.link.Unpair(r.PathValue("name")); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "unpaired"})
	})
	mux.HandleFunc("GET /_emu/v1/links/requests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"requests": in.link.Requests()})
	})
	mux.HandleFunc("POST /_emu/v1/links/requests/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name string }
		if r.ContentLength != 0 && !decodeBody(w, r, &body) {
			return
		}
		p, err := in.link.Approve(r.PathValue("id"), body.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, p)
	})
	mux.HandleFunc("POST /_emu/v1/links/requests/{id}/reject", func(w http.ResponseWriter, r *http.Request) {
		if err := in.link.Reject(r.PathValue("id")); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
	})
}

// decodeBody decodes r's JSON body into v, or answers 400 and reports
// false.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return false
	}
	return true
}

// linkError writes err from pairing with another Instance: a refusal by
// it is a conflict, failing to reach it a bad gateway, and anything else,
// such as a link name already taken, a bad request.
func linkError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	body := map[string]string{"error": err.Error()}
	var ref *link.RefusedError
	var ue *url.Error
	switch {
	case errors.As(err, &ref):
		code, body["reason"] = http.StatusConflict, ref.Reason
	case errors.As(err, &ue):
		code = http.StatusBadGateway
	}
	writeJSON(w, code, body)
}
