package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
)

// Delegated APIs (ADR 0027): an API that no Service serves, such as
// Firestore's, which the API gateway forwards as it comes to the External
// emulator configured for it by address, or to the Managed emulator the
// Instance runs for it (ADR 0029). The gateway checks no IAM for it.

// knownAPI is what the API gateway knows of an API Google ships an
// emulator for, beyond the google.API.* gRPC package and API.googleapis.com
// host that every Delegated API has.
type knownAPI struct {
	// hosts are its hostnames besides API.googleapis.com.
	hosts []string
	// operations match the names of its Operations, by which google.longrunning
	// calls on the API gateway's own address are told apart. None matches the
	// Services' own, projects/P/locations/L/operations/ID.
	operations *regexp.Regexp
	// rest matches the paths of its REST requests, which clients send to the
	// root of the address envVar names. No Service serves them there.
	rest *regexp.Regexp
	// envVar is the variable its client libraries read for the emulator's
	// address.
	envVar string
}

var knownAPIs = map[string]knownAPI{
	"firestore": {
		operations: regexp.MustCompile(`^projects/[^/]+/databases/`),
		rest:       regexp.MustCompile(`^/(v1(beta\d)?/projects/[^/]+/databases/|emulator/v1/projects/)`),
		envVar:     "FIRESTORE_EMULATOR_HOST",
	},
	"datastore": {
		operations: regexp.MustCompile(`^projects/[^/]+/operations(/|$)`),
		rest:       regexp.MustCompile(`^/v1(beta\d)?/projects/[^/:]+:[A-Za-z]+$`),
		envVar:     "DATASTORE_EMULATOR_HOST",
	},
	"spanner": {
		operations: regexp.MustCompile(`^projects/[^/]+/instances/`),
		envVar:     "SPANNER_EMULATOR_HOST",
	},
	"bigtable": {
		hosts:      []string{"bigtableadmin.googleapis.com"},
		operations: regexp.MustCompile(`^operations/projects/`),
		envVar:     "BIGTABLE_EMULATOR_HOST",
	},
	"pubsub": {envVar: "PUBSUB_EMULATOR_HOST"},
}

// APIHosts returns the real hostnames on which Google serves the API named
// api, which the Google front end serves when it is delegated.
func APIHosts(api string) []string {
	return append([]string{api + ".googleapis.com"}, knownAPIs[api].hosts...)
}

// EmulatorHostVar returns the variable, such as FIRESTORE_EMULATOR_HOST,
// that the client libraries of the API named api read for its emulator's
// address, or "" when they read none.
func EmulatorHostVar(api string) string { return knownAPIs[api].envVar }

// delegatedAPI is one Delegated API and the proxies to its External
// emulator.
type delegatedAPI struct {
	DelegatedAPI
	known knownAPI
	// rest forwards over HTTP/1.1, grpc over prior-knowledge h2c, as the
	// External emulator's gRPC server expects.
	rest, grpc *httputil.ReverseProxy
}

// DelegatedAPI is a Delegated API and its External emulator.
type DelegatedAPI struct {
	API string `json:"api"`
	// Address is "" for a Managed emulator, whose address the Instance
	// knows.
	Address string `json:"address"`
	// Managed is whether the Instance runs the External emulator (ADR
	// 0029).
	Managed bool     `json:"managed,omitempty"`
	Hosts   []string `json:"hosts"` // the hostnames the Google front end serves for it
}

// ManagedDialer connects to a Managed emulator's gRPC server, or to its
// REST server when grpc is false. Its error says why the emulator is
// unavailable.
type ManagedDialer func(ctx context.Context, grpc bool) (net.Conn, error)

// Delegate forwards the API named api to the External emulator at addr
// (host:port). It refuses an API that a registered Service serves, by its
// path, one of its hostnames or a gRPC service in its google.API package,
// and one already delegated or whose hostname another Delegated API has.
// Call it after Register, before Serve.
func (g *Gateway) Delegate(api, addr string) error {
	d := &delegatedAPI{DelegatedAPI: DelegatedAPI{API: api, Address: addr, Hosts: APIHosts(api)}, known: knownAPIs[api]}
	h2c := &http.Transport{Protocols: new(http.Protocols)}
	h2c.Protocols.SetUnencryptedHTTP2(true)
	return g.delegate(d, addr, nil, h2c)
}

// DelegateManaged forwards the API named api, as Delegate does, to the
// Managed emulator that dial connects to (ADR 0029).
func (g *Gateway) DelegateManaged(api string, dial ManagedDialer) error {
	d := &delegatedAPI{DelegatedAPI: DelegatedAPI{API: api, Managed: true, Hosts: APIHosts(api)}, known: knownAPIs[api]}
	rest := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx, false) }}
	h2c := &http.Transport{Protocols: new(http.Protocols), DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx, true) }}
	h2c.Protocols.SetUnencryptedHTTP2(true)
	return g.delegate(d, api+".managed", rest, h2c)
}

// delegate forwards d's requests to target (host:port, or a name that
// rest's and grpc's dialers ignore), REST over rest and gRPC over grpc;
// a nil rest is http.DefaultTransport.
func (g *Gateway) delegate(d *delegatedAPI, target string, rest, grpc http.RoundTripper) error {
	u := &url.URL{Scheme: "http", Host: target}
	rewrite := func(pr *httputil.ProxyRequest) {
		pr.SetURL(u)
		pr.Out.Host = pr.In.Host
	}
	d.rest = &httputil.ReverseProxy{Rewrite: rewrite, Transport: rest, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
		apierr.Write(w, d.unavailable(err))
	}}
	d.grpc = &httputil.ReverseProxy{Rewrite: rewrite, Transport: grpc, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
		writeGRPCError(w, d.unavailable(err))
	}}

	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.delegable(d); err != nil {
		value := d.Address
		if d.Managed {
			value = config.Managed
		}
		return fmt.Errorf("--external %s=%s: %w", d.API, value, err)
	}
	for _, h := range d.Hosts {
		g.hosts[h] = d.rest
		g.delegatedHosts[h] = d
	}
	g.delegated = append(g.delegated, d)
	prefix := "/" + d.API
	g.mux.Handle(prefix+"/", http.StripPrefix(prefix, d.rest))
	return nil
}

// delegable reports why d cannot be delegated, or nil. Call it with g.mu
// held.
func (g *Gateway) delegable(d *delegatedAPI) error {
	for _, o := range g.delegated {
		if o.API == d.API && o.Managed {
			return fmt.Errorf("%s is already delegated to a Managed emulator", d.API)
		}
		if o.API == d.API {
			return fmt.Errorf("%s is already delegated to %s", d.API, o.Address)
		}
	}
	if svc := g.apiOwners[d.API]; svc != "" {
		return fmt.Errorf("the %q Service serves the %s API, so it cannot be delegated", svc, d.API)
	}
	for _, h := range d.Hosts {
		if svc := g.apiOwners[h]; svc != "" {
			return fmt.Errorf("the %q Service serves %s, so the %s API cannot be delegated", svc, h, d.API)
		}
		if o := g.delegatedHosts[h]; o != nil {
			return fmt.Errorf("%s is served for the Delegated API %s", h, o.API)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(g.grpcOwners)) {
		if strings.HasPrefix(name, "google."+d.API+".") {
			return fmt.Errorf("the %q Service serves the %s API, so it cannot be delegated", g.grpcOwners[name], d.API)
		}
	}
	return nil
}

// DelegatedAPIs returns the Delegated APIs, sorted by API name.
func (g *Gateway) DelegatedAPIs() []DelegatedAPI {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]DelegatedAPI, 0, len(g.delegated))
	for _, d := range g.delegated {
		out = append(out, d.DelegatedAPI)
	}
	slices.SortFunc(out, func(a, b DelegatedAPI) int { return strings.Compare(a.API, b.API) })
	return out
}

// delegatedAPIs returns the Delegated APIs and those by hostname, which
// Delegate only adds to before the gateway serves.
func (g *Gateway) delegatedAPIs() ([]*delegatedAPI, map[string]*delegatedAPI) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.delegated, g.delegatedHosts
}

// grpcDelegated returns the Delegated API a gRPC request is for: the one
// whose hostname it names, as in Host mode, or the one whose google.API.*
// package its service is in, as from FIRESTORE_EMULATOR_HOST and the like,
// or whose Operation a google.longrunning call names; nil when it is for
// none.
func (g *Gateway) grpcDelegated(r *http.Request) *delegatedAPI {
	apis, byHost := g.delegatedAPIs()
	if len(apis) == 0 {
		return nil
	}
	if d, ok := byHost[hostOnly(r.Host)]; ok {
		return d
	}
	for _, d := range apis {
		if strings.HasPrefix(r.URL.Path, "/google."+d.API+".") {
			return d
		}
	}
	if method, ok := strings.CutPrefix(r.URL.Path, operationsService); ok {
		name := operationName(r, method)
		for _, d := range apis {
			if re := d.known.operations; re != nil && re.MatchString(name) {
				return d
			}
		}
	}
	return nil
}

// restDelegated returns the Delegated API whose REST path a request to the
// API gateway's root has, as clients send them to FIRESTORE_EMULATOR_HOST
// and the like; nil when it has none.
func (g *Gateway) restDelegated(r *http.Request) *delegatedAPI {
	apis, _ := g.delegatedAPIs()
	for _, d := range apis {
		if re := d.known.rest; re != nil && re.MatchString(r.URL.Path) {
			return d
		}
	}
	return nil
}

// hostOnly is a Host header without its port.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// operationsService prefixes the google.longrunning.Operations methods,
// which a Delegated API's clients call for its Operations and the
// Services' clients for theirs.
const operationsService = "/google.longrunning.Operations/"

// maxPeek bounds the google.longrunning request operationName reads.
const maxPeek = 64 << 10

// operationName returns the Operation, or for ListOperations the
// collection, that a google.longrunning call names, reading its one
// uncompressed request message and putting it back; "" when it can't.
func operationName(r *http.Request, method string) string {
	field := protowire.Number(1)
	if method == "ListOperations" {
		field = 4
	}
	for b := firstMessage(r); len(b) > 0; {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return ""
		}
		b = b[n:]
		if num == field && typ == protowire.BytesType {
			v, _ := protowire.ConsumeBytes(b)
			return string(v)
		}
		if n = protowire.ConsumeFieldValue(num, typ, b); n < 0 {
			return ""
		}
		b = b[n:]
	}
	return ""
}

// firstMessage returns a gRPC call's first request message, reading it and
// putting it back, decompressed when it is gzip-compressed; nil when it is
// otherwise compressed, larger than maxPeek or malformed.
func firstMessage(r *http.Request) []byte {
	var head [5]byte
	n, _ := io.ReadFull(r.Body, head[:])
	read := head[:n]
	defer func() {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(read), r.Body), r.Body}
	}()
	size := binary.BigEndian.Uint32(head[1:])
	gzipped := head[0] == 1 && r.Header.Get("Grpc-Encoding") == "gzip"
	if n < len(head) || head[0] != 0 && !gzipped || size > maxPeek {
		return nil
	}
	msg := make([]byte, size)
	m, _ := io.ReadFull(r.Body, msg)
	read = append(read, msg[:m]...)
	if m < len(msg) {
		return nil
	}
	if gzipped {
		zr, err := gzip.NewReader(bytes.NewReader(msg))
		if err != nil {
			return nil
		}
		if msg, err = io.ReadAll(io.LimitReader(zr, maxPeek+1)); err != nil || len(msg) > maxPeek {
			return nil
		}
	}
	return msg
}

// unavailable is the error a request gets when the External emulator could
// not be reached, or stopped answering, with err.
func (d *delegatedAPI) unavailable(err error) *apierr.Error {
	if d.Managed {
		return apierr.New(codes.Unavailable, "The Managed emulator for the Delegated API %s is unavailable: %v", d.API, err)
	}
	return apierr.New(codes.Unavailable, "The External emulator for the Delegated API %s at %s is unavailable: %v", d.API, d.Address, err)
}

// writeGRPCError answers a gRPC call with e and no message, in a
// trailers-only response.
func writeGRPCError(w http.ResponseWriter, e *apierr.Error) {
	s := e.GRPCStatus()
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Grpc-Status", strconv.Itoa(int(s.Code())))
	w.Header().Set("Grpc-Message", grpcMessage(s.Message()))
	w.WriteHeader(http.StatusOK)
}

// grpcMessage percent-encodes msg for the grpc-message header, as gRPC's
// HTTP/2 transport specifies.
func grpcMessage(msg string) string {
	var b strings.Builder
	for i := 0; i < len(msg); i++ {
		if c := msg[i]; c >= ' ' && c <= '~' && c != '%' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
