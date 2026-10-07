package pubsub

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// REST v1 surface (google.api.http bindings of pubsub.proto and
// schema.proto), translated to the gRPC implementations so both protocols
// share one code path. gcloud and OpenTofu use this.

type restAPI struct {
	s   *Service
	pub *publisherServer
	sub *subscriberServer
	sch *schemaServer
	iam *iamServer
}

func (s *Service) restHandler() http.Handler {
	return &restAPI{s: s, pub: &publisherServer{s: s}, sub: &subscriberServer{s: s}, sch: &schemaServer{s: s}, iam: &iamServer{s: s}}
}

var marshalOpts = protojson.MarshalOptions{}

func writeProto(w http.ResponseWriter, m proto.Message) {
	b, err := marshalOpts.Marshal(m)
	if err != nil {
		apierr.Write(w, apierr.Internal("%v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}

// decode reads the JSON body (if any) and query parameters into m.
func decode(r *http.Request, m proto.Message) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 3*maxPublishBytes))
	if err != nil {
		return apierr.InvalidArgument("Unable to read request body: %v", err)
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := unmarshalOpts.Unmarshal(body, m); err != nil {
			return apierr.InvalidArgument("Invalid JSON payload received. %v", err)
		}
	}
	return bindQuery(m.ProtoReflect(), r.URL.Query())
}

// bindQuery sets (possibly dotted) query parameters on m; unknown
// parameters (alt, prettyPrint, fields, ...) are ignored.
func bindQuery(m protoreflect.Message, q url.Values) error {
	for key, vals := range q {
		if len(vals) == 0 {
			continue
		}
		cur := m
		parts := strings.Split(key, ".")
		var fd protoreflect.FieldDescriptor
		for i, p := range parts {
			fields := cur.Descriptor().Fields()
			fd = fields.ByJSONName(p)
			if fd == nil {
				fd = fields.ByName(protoreflect.Name(p))
			}
			if fd == nil {
				break
			}
			if i < len(parts)-1 {
				if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
					fd = nil
					break
				}
				cur = cur.Mutable(fd).Message()
			}
		}
		if fd == nil || fd.IsMap() || fd.Kind() == protoreflect.MessageKind {
			if fd != nil && fd.Message().FullName() == "google.protobuf.FieldMask" {
				fm := cur.Mutable(fd).Message()
				paths := fm.Descriptor().Fields().ByName("paths")
				list := fm.Mutable(paths).List()
				for _, p := range strings.Split(vals[0], ",") {
					list.Append(protoreflect.ValueOfString(camelToSnake(p)))
				}
			}
			continue
		}
		for _, v := range vals {
			pv, err := scalarValue(fd, v)
			if err != nil {
				return apierr.InvalidArgument("Invalid value for query parameter %q: %q", key, v)
			}
			if fd.IsList() {
				cur.Mutable(fd).List().Append(pv)
			} else {
				cur.Set(fd, pv)
			}
		}
	}
	return nil
}

func camelToSnake(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			b.WriteRune(r + 'a' - 'A')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func scalarValue(fd protoreflect.FieldDescriptor, v string) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(v), nil
	case protoreflect.BoolKind:
		b, err := strconv.ParseBool(v)
		return protoreflect.ValueOfBool(b), err
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		n, err := strconv.ParseInt(v, 10, 32)
		return protoreflect.ValueOfInt32(int32(n)), err
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		n, err := strconv.ParseInt(v, 10, 64)
		return protoreflect.ValueOfInt64(n), err
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		n, err := strconv.ParseUint(v, 10, 32)
		return protoreflect.ValueOfUint32(uint32(n)), err
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		n, err := strconv.ParseUint(v, 10, 64)
		return protoreflect.ValueOfUint64(n), err
	case protoreflect.EnumKind:
		if ev := fd.Enum().Values().ByName(protoreflect.Name(v)); ev != nil {
			return protoreflect.ValueOfEnum(ev.Number()), nil
		}
		n, err := strconv.ParseInt(v, 10, 32)
		return protoreflect.ValueOfEnum(protoreflect.EnumNumber(n)), err
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte(v)), nil
	}
	return protoreflect.Value{}, apierr.InvalidArgument("unsupported parameter type")
}

// call decodes req, runs fn and writes the response.
func call[Req proto.Message, Resp proto.Message](w http.ResponseWriter, r *http.Request, req Req, setup func(Req), fn func(context.Context, Req) (Resp, error)) {
	if err := decode(r, req); err != nil {
		apierr.Write(w, err)
		return
	}
	if setup != nil {
		setup(req)
	}
	resp, err := fn(r.Context(), req)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeProto(w, resp)
}

func methodNotFound(w http.ResponseWriter, r *http.Request) {
	apierr.Write(w, apierr.NotFound("Method not found: %s %s", r.Method, r.URL.Path).WithHTTP(http.StatusNotFound))
}

func (a *restAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok {
		methodNotFound(w, r)
		return
	}
	verb := ""
	if i := strings.LastIndexByte(path, ':'); i > strings.LastIndexByte(path, '/') {
		path, verb = path[:i], path[i+1:]
	}
	segs := strings.Split(path, "/")
	if len(segs) < 3 || segs[0] != "projects" {
		methodNotFound(w, r)
		return
	}
	project := "projects/" + segs[1]
	name := strings.Join(segs[:min(4, len(segs))], "/")
	m := r.Method
	switch {
	case len(segs) == 3 && verb == "":
		a.collection(w, r, segs[2], project)
	case len(segs) == 3 && segs[2] == kindSchemas && m == http.MethodPost && verb == "validate":
		call(w, r, &pubsubpb.ValidateSchemaRequest{}, func(q *pubsubpb.ValidateSchemaRequest) { q.Parent = project }, a.sch.ValidateSchema)
	case len(segs) == 3 && segs[2] == kindSchemas && m == http.MethodPost && verb == "validateMessage":
		call(w, r, &pubsubpb.ValidateMessageRequest{}, func(q *pubsubpb.ValidateMessageRequest) { q.Parent = project }, a.sch.ValidateMessage)
	case len(segs) == 4 && (verb == "getIamPolicy" || verb == "setIamPolicy" || verb == "testIamPermissions"):
		a.iamCall(w, r, verb, name)
	case len(segs) == 4 && segs[2] == kindTopics:
		a.topic(w, r, name, verb)
	case len(segs) == 4 && segs[2] == kindSubscriptions:
		a.subscription(w, r, name, verb)
	case len(segs) == 4 && segs[2] == kindSnapshots:
		a.snapshot(w, r, name, verb)
	case len(segs) == 4 && segs[2] == kindSchemas:
		a.schema(w, r, name, verb)
	case len(segs) == 5 && segs[2] == kindTopics && segs[4] == kindSubscriptions && m == http.MethodGet && verb == "":
		call(w, r, &pubsubpb.ListTopicSubscriptionsRequest{}, func(q *pubsubpb.ListTopicSubscriptionsRequest) { q.Topic = name }, a.pub.ListTopicSubscriptions)
	case len(segs) == 5 && segs[2] == kindTopics && segs[4] == kindSnapshots && m == http.MethodGet && verb == "":
		call(w, r, &pubsubpb.ListTopicSnapshotsRequest{}, func(q *pubsubpb.ListTopicSnapshotsRequest) { q.Topic = name }, a.pub.ListTopicSnapshots)
	default:
		methodNotFound(w, r)
	}
}

func (a *restAPI) collection(w http.ResponseWriter, r *http.Request, kind, project string) {
	switch {
	case kind == kindTopics && r.Method == http.MethodGet:
		call(w, r, &pubsubpb.ListTopicsRequest{}, func(q *pubsubpb.ListTopicsRequest) { q.Project = project }, a.pub.ListTopics)
	case kind == kindSubscriptions && r.Method == http.MethodGet:
		call(w, r, &pubsubpb.ListSubscriptionsRequest{}, func(q *pubsubpb.ListSubscriptionsRequest) { q.Project = project }, a.sub.ListSubscriptions)
	case kind == kindSnapshots && r.Method == http.MethodGet:
		call(w, r, &pubsubpb.ListSnapshotsRequest{}, func(q *pubsubpb.ListSnapshotsRequest) { q.Project = project }, a.sub.ListSnapshots)
	case kind == kindSchemas && r.Method == http.MethodGet:
		call(w, r, &pubsubpb.ListSchemasRequest{}, func(q *pubsubpb.ListSchemasRequest) { q.Parent = project }, a.sch.ListSchemas)
	case kind == kindSchemas && r.Method == http.MethodPost:
		// The body is the Schema; schemaId is a query parameter.
		var sc pubsubpb.Schema
		if err := decodeBody(r, &sc); err != nil {
			apierr.Write(w, err)
			return
		}
		req := &pubsubpb.CreateSchemaRequest{Parent: project, Schema: &sc, SchemaId: r.URL.Query().Get("schemaId")}
		if req.SchemaId == "" {
			req.SchemaId = r.URL.Query().Get("schema_id")
		}
		resp, err := a.sch.CreateSchema(r.Context(), req)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		writeProto(w, resp)
	default:
		methodNotFound(w, r)
	}
}

// decodeBody reads only the JSON body into m.
func decodeBody(r *http.Request, m proto.Message) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 3*maxPublishBytes))
	if err != nil {
		return apierr.InvalidArgument("Unable to read request body: %v", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := unmarshalOpts.Unmarshal(body, m); err != nil {
		return apierr.InvalidArgument("Invalid JSON payload received. %v", err)
	}
	return nil
}

func (a *restAPI) topic(w http.ResponseWriter, r *http.Request, name, verb string) {
	switch {
	case verb == "" && r.Method == http.MethodPut:
		call(w, r, &pubsubpb.Topic{}, func(q *pubsubpb.Topic) { q.Name = name }, a.pub.CreateTopic)
	case verb == "" && r.Method == http.MethodGet:
		call(w, r, &pubsubpb.GetTopicRequest{}, func(q *pubsubpb.GetTopicRequest) { q.Topic = name }, a.pub.GetTopic)
	case verb == "" && r.Method == http.MethodPatch:
		call(w, r, &pubsubpb.UpdateTopicRequest{}, func(q *pubsubpb.UpdateTopicRequest) {
			if q.Topic == nil {
				q.Topic = &pubsubpb.Topic{}
			}
			q.Topic.Name = name
		}, a.pub.UpdateTopic)
	case verb == "" && r.Method == http.MethodDelete:
		call(w, r, &pubsubpb.DeleteTopicRequest{}, func(q *pubsubpb.DeleteTopicRequest) { q.Topic = name }, a.pub.DeleteTopic)
	case verb == "publish" && r.Method == http.MethodPost:
		call(w, r, &pubsubpb.PublishRequest{}, func(q *pubsubpb.PublishRequest) { q.Topic = name }, a.pub.Publish)
	default:
		methodNotFound(w, r)
	}
}

func (a *restAPI) subscription(w http.ResponseWriter, r *http.Request, name, verb string) {
	m := r.Method
	switch {
	case verb == "" && m == http.MethodPut:
		call(w, r, &pubsubpb.Subscription{}, func(q *pubsubpb.Subscription) { q.Name = name }, a.sub.CreateSubscription)
	case verb == "" && m == http.MethodGet:
		call(w, r, &pubsubpb.GetSubscriptionRequest{}, func(q *pubsubpb.GetSubscriptionRequest) { q.Subscription = name }, a.sub.GetSubscription)
	case verb == "" && m == http.MethodPatch:
		call(w, r, &pubsubpb.UpdateSubscriptionRequest{}, func(q *pubsubpb.UpdateSubscriptionRequest) {
			if q.Subscription == nil {
				q.Subscription = &pubsubpb.Subscription{}
			}
			q.Subscription.Name = name
		}, a.sub.UpdateSubscription)
	case verb == "" && m == http.MethodDelete:
		call(w, r, &pubsubpb.DeleteSubscriptionRequest{}, func(q *pubsubpb.DeleteSubscriptionRequest) { q.Subscription = name }, a.sub.DeleteSubscription)
	case m != http.MethodPost:
		methodNotFound(w, r)
	case verb == "pull":
		call(w, r, &pubsubpb.PullRequest{}, func(q *pubsubpb.PullRequest) { q.Subscription = name }, a.sub.Pull)
	case verb == "acknowledge":
		call(w, r, &pubsubpb.AcknowledgeRequest{}, func(q *pubsubpb.AcknowledgeRequest) { q.Subscription = name }, a.sub.Acknowledge)
	case verb == "modifyAckDeadline":
		call(w, r, &pubsubpb.ModifyAckDeadlineRequest{}, func(q *pubsubpb.ModifyAckDeadlineRequest) { q.Subscription = name }, a.sub.ModifyAckDeadline)
	case verb == "modifyPushConfig":
		call(w, r, &pubsubpb.ModifyPushConfigRequest{}, func(q *pubsubpb.ModifyPushConfigRequest) { q.Subscription = name }, a.sub.ModifyPushConfig)
	case verb == "seek":
		call(w, r, &pubsubpb.SeekRequest{}, func(q *pubsubpb.SeekRequest) { q.Subscription = name }, a.sub.Seek)
	case verb == "detach":
		call(w, r, &pubsubpb.DetachSubscriptionRequest{}, func(q *pubsubpb.DetachSubscriptionRequest) { q.Subscription = name }, a.pub.DetachSubscription)
	default:
		methodNotFound(w, r)
	}
}

func (a *restAPI) snapshot(w http.ResponseWriter, r *http.Request, name, verb string) {
	if verb != "" {
		methodNotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		call(w, r, &pubsubpb.CreateSnapshotRequest{}, func(q *pubsubpb.CreateSnapshotRequest) { q.Name = name }, a.sub.CreateSnapshot)
	case http.MethodGet:
		call(w, r, &pubsubpb.GetSnapshotRequest{}, func(q *pubsubpb.GetSnapshotRequest) { q.Snapshot = name }, a.sub.GetSnapshot)
	case http.MethodPatch:
		call(w, r, &pubsubpb.UpdateSnapshotRequest{}, func(q *pubsubpb.UpdateSnapshotRequest) {
			if q.Snapshot == nil {
				q.Snapshot = &pubsubpb.Snapshot{}
			}
			q.Snapshot.Name = name
		}, a.sub.UpdateSnapshot)
	case http.MethodDelete:
		call(w, r, &pubsubpb.DeleteSnapshotRequest{}, func(q *pubsubpb.DeleteSnapshotRequest) { q.Snapshot = name }, a.sub.DeleteSnapshot)
	default:
		methodNotFound(w, r)
	}
}

func (a *restAPI) schema(w http.ResponseWriter, r *http.Request, name, verb string) {
	m := r.Method
	switch {
	case verb == "" && m == http.MethodGet:
		call(w, r, &pubsubpb.GetSchemaRequest{}, func(q *pubsubpb.GetSchemaRequest) { q.Name = name }, a.sch.GetSchema)
	case verb == "" && m == http.MethodDelete:
		call(w, r, &pubsubpb.DeleteSchemaRequest{}, func(q *pubsubpb.DeleteSchemaRequest) { q.Name = name }, a.sch.DeleteSchema)
	case verb == "listRevisions" && m == http.MethodGet:
		call(w, r, &pubsubpb.ListSchemaRevisionsRequest{}, func(q *pubsubpb.ListSchemaRevisionsRequest) { q.Name = name }, a.sch.ListSchemaRevisions)
	case verb == "commit" && m == http.MethodPost:
		call(w, r, &pubsubpb.CommitSchemaRequest{}, func(q *pubsubpb.CommitSchemaRequest) { q.Name = name }, a.sch.CommitSchema)
	case verb == "rollback" && m == http.MethodPost:
		call(w, r, &pubsubpb.RollbackSchemaRequest{}, func(q *pubsubpb.RollbackSchemaRequest) { q.Name = name }, a.sch.RollbackSchema)
	case verb == "deleteRevision" && m == http.MethodDelete:
		call(w, r, &pubsubpb.DeleteSchemaRevisionRequest{}, func(q *pubsubpb.DeleteSchemaRevisionRequest) {
			if !strings.Contains(name, "@") && q.RevisionId != "" { //nolint:staticcheck
				q.Name = name + "@" + q.RevisionId //nolint:staticcheck
				return
			}
			q.Name = name
		}, a.sch.DeleteSchemaRevision)
	default:
		methodNotFound(w, r)
	}
}

func (a *restAPI) iamCall(w http.ResponseWriter, r *http.Request, verb, resource string) {
	switch {
	case verb == "getIamPolicy" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		call(w, r, &iampb.GetIamPolicyRequest{}, func(q *iampb.GetIamPolicyRequest) { q.Resource = resource }, a.iam.GetIamPolicy)
	case verb == "setIamPolicy" && r.Method == http.MethodPost:
		call(w, r, &iampb.SetIamPolicyRequest{}, func(q *iampb.SetIamPolicyRequest) { q.Resource = resource }, a.iam.SetIamPolicy)
	case verb == "testIamPermissions" && r.Method == http.MethodPost:
		call(w, r, &iampb.TestIamPermissionsRequest{}, func(q *iampb.TestIamPermissionsRequest) { q.Resource = resource }, a.iam.TestIamPermissions)
	default:
		methodNotFound(w, r)
	}
}
