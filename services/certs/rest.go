package certs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// REST surfaces (v1) of both APIs, mounted on the gateway at
// /certificatemanager/ and /networksecurity/ and on their real hosts:
//
//	GET    /v1/projects/P/locations[/L]
//	GET    /v1/{parent}/COLL                 list
//	POST   /v1/{parent}/COLL?COLLId=ID       create (LRO)
//	GET    /v1/{name}                        get
//	PATCH  /v1/{name}?updateMask=...         update (LRO)
//	DELETE /v1/{name}[?etag=...]             delete (LRO)
//	...    /v1/projects/P/locations/L/operations[...]
func (s *Service) restHandler(api string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/v1/")
		if !ok {
			apierr.Write(w, apierr.NotFound("The requested URL %s was not found on this server.", r.URL.Path))
			return
		}
		if s.ops.ServeREST(w, r, rest) {
			return
		}
		s.serveREST(w, r, api, rest)
	})
}

func (s *Service) serveREST(w http.ResponseWriter, r *http.Request, api, path string) {
	if strings.Contains(path, ":") {
		apierr.Write(w, apierr.NotFound("The requested URL %s was not found on this server.", r.URL.Path))
		return
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, sg := range segs {
		if u, err := url.PathUnescape(sg); err == nil {
			segs[i] = u
		}
	}
	ctx := r.Context()
	q := r.URL.Query()
	if len(segs) < 2 || segs[0] != "projects" {
		apierr.Write(w, apierr.NotFound("The requested URL %s was not found on this server.", r.URL.Path))
		return
	}
	// Locations.
	if len(segs) == 3 && segs[2] == "locations" && r.Method == http.MethodGet {
		size, _ := strconv.Atoi(q.Get("pageSize"))
		resp, err := s.listLocations(ctx, api, segs[1], int32(size), q.Get("pageToken"))
		writeProto(w, resp, err)
		return
	}
	if len(segs) == 4 && segs[2] == "locations" && r.Method == http.MethodGet {
		resp, err := s.getLocation(ctx, api, "projects/"+segs[1]+"/locations/"+segs[3])
		writeProto(w, resp, err)
		return
	}
	if len(segs) < 5 || segs[2] != "locations" {
		apierr.Write(w, apierr.NotFound("The requested URL %s was not found on this server.", r.URL.Path))
		return
	}
	isColl := len(segs)%2 == 1
	coll := segs[len(segs)-1]
	if !isColl {
		coll = segs[len(segs)-2]
	}
	k := kindFor(api, coll)
	if k == nil || (k.parentColl == "" && len(segs) > 6) || (k.parentColl != "" && (len(segs) < 7 || segs[4] != k.parentColl)) {
		apierr.Write(w, apierr.NotFound("The requested URL %s was not found on this server.", r.URL.Path))
		return
	}
	full := strings.Join(segs, "/")
	if isColl {
		parent := strings.Join(segs[:len(segs)-1], "/")
		switch r.Method {
		case http.MethodGet:
			size, err := strconv.Atoi(q.Get("pageSize"))
			if err != nil && q.Get("pageSize") != "" {
				apierr.Write(w, apierr.InvalidArgument("Invalid value at 'page_size' (TYPE_INT32), %q", q.Get("pageSize")))
				return
			}
			res, err := s.list(ctx, k, parent, int32(size), q.Get("pageToken"), q.Get("filter"), q.Get("orderBy"))
			if err != nil {
				apierr.Write(w, err)
				return
			}
			items := make([]any, len(res.items))
			for i, o := range res.items {
				items[i] = o
			}
			out := map[string]any{}
			if len(items) > 0 {
				out[k.coll] = items
			}
			if res.next != "" {
				out["nextPageToken"] = res.next
			}
			writeJSON(w, out)
		case http.MethodPost:
			body, err := readBody(r)
			if err != nil {
				apierr.Write(w, err)
				return
			}
			in, err := decodeREST(k, body)
			if err != nil {
				apierr.Write(w, err)
				return
			}
			op, err := s.create(ctx, k, parent, q.Get(k.idParam), in)
			writeProto(w, op, err)
		default:
			methodNotAllowed(w, r)
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		o, err := s.get(ctx, k, full)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		writeJSON(w, o)
	case http.MethodPatch:
		body, err := readBody(r)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		in, err := decodeREST(k, body)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		var mask []string
		for _, m := range q["updateMask"] {
			mask = append(mask, strings.Split(m, ",")...)
		}
		op, err := s.patch(ctx, k, full, in, mask)
		writeProto(w, op, err)
	case http.MethodDelete:
		op, err := s.remove(ctx, k, full, q.Get("etag"))
		writeProto(w, op, err)
	default:
		methodNotAllowed(w, r)
	}
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	apierr.Write(w, apierr.New(codes.Unimplemented, "Method %s is not allowed for %s.", r.Method, r.URL.Path).WithHTTP(http.StatusMethodNotAllowed))
}

func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, apierr.InvalidArgument("read body: %v", err)
	}
	return b, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}

// writeProto writes msg as JSON, or err as the REST error envelope.
func writeProto(w http.ResponseWriter, msg proto.Message, err error) {
	if err != nil {
		apierr.Write(w, err)
		return
	}
	b, err := protojson.Marshal(msg)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}
