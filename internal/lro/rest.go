package lro

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// ServeREST serves the google.longrunning REST mappings for this service's
// operations. path is the resource path after the API version, e.g.
// "projects/P/locations/L/operations/ID" (optionally with ":cancel" or
// ":wait"), or "projects/P/locations/L/operations" to list. It reports
// whether path addressed an operation; when false nothing was written.
//
//	GET    {name=**/operations/*}
//	DELETE {name=**/operations/*}
//	POST   {name=**/operations/*}:cancel
//	POST   {name=**/operations/*}:wait
//	GET    {name=**}/operations
func (m *Manager) ServeREST(w http.ResponseWriter, r *http.Request, path string) bool {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	n := len(segs)
	switch {
	case n >= 2 && segs[n-1] == "operations" && r.Method == http.MethodGet:
		q := r.URL.Query()
		size, _ := strconv.Atoi(q.Get("pageSize"))
		req := &longrunningpb.ListOperationsRequest{
			Name:      strings.Join(segs[:n-1], "/"),
			Filter:    q.Get("filter"),
			PageSize:  int32(size),
			PageToken: q.Get("pageToken"),
		}
		resp, err := listOps(m.env.Store, m.service, req)
		writeProto(w, resp, err)
		return true
	case n >= 2 && segs[n-2] == "operations":
	default:
		return false
	}
	name, verb, _ := strings.Cut(path, ":")
	name = strings.Trim(name, "/")
	switch {
	case verb == "" && r.Method == http.MethodGet:
		op, err := m.Get(name)
		writeProto(w, op, err)
	case verb == "" && r.Method == http.MethodDelete:
		resp, err := deleteOp(m.env.Store, m.service, name)
		writeProto(w, resp, err)
	case verb == "cancel" && r.Method == http.MethodPost:
		op, err := m.Get(name)
		if err == nil && !op.Done {
			m.cancel(name)
		}
		writeProto(w, &emptypb.Empty{}, err)
	case verb == "wait" && r.Method == http.MethodPost:
		req := &longrunningpb.WaitOperationRequest{}
		if b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)); len(strings.TrimSpace(string(b))) > 0 {
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, req); err != nil {
				writeProto(w, nil, apierr.InvalidArgument("Invalid JSON payload received. %v", err))
				return true
			}
		}
		req.Name = name
		op, err := waitOp(r.Context(), req, func() (*longrunningpb.Operation, error) { return m.Get(name) })
		writeProto(w, op, err)
	default:
		apierr.Write(w, apierr.New(codes.Unimplemented, "Method not allowed.").WithHTTP(http.StatusMethodNotAllowed))
	}
	return true
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
