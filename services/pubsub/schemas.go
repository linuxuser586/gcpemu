package pubsub

import (
	"context"
	"sort"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// schemaServer implements google.pubsub.v1.SchemaService (FR-PS-009).
type schemaServer struct {
	pubsubpb.UnimplementedSchemaServiceServer
	s *Service
}

// deletedSchema is the schema value of topics whose schema was deleted.
const deletedSchema = "_deleted-schema_"

// schemaHistory holds the revisions of one schema, oldest first.
type schemaHistory struct {
	revs       []*pubsubpb.Schema
	validators map[string]validator
}

func (h *schemaHistory) latest() *pubsubpb.Schema { return h.revs[len(h.revs)-1] }

func (h *schemaHistory) find(rev string) *pubsubpb.Schema {
	for _, r := range h.revs {
		if r.RevisionId == rev {
			return r
		}
	}
	return nil
}

// addSchemaRevision records a revision (s.mu held or during load).
func (s *Service) addSchemaRevision(sc *pubsubpb.Schema) {
	h := s.schemas[sc.Name]
	if h == nil {
		h = &schemaHistory{validators: map[string]validator{}}
		s.schemas[sc.Name] = h
	}
	h.revs = append(h.revs, sc)
	sort.SliceStable(h.revs, func(i, j int) bool {
		return h.revs[i].GetRevisionCreateTime().AsTime().Before(h.revs[j].GetRevisionCreateTime().AsTime())
	})
	if v, err := compileSchema(sc.Type, sc.Definition); err == nil {
		h.validators[sc.RevisionId] = v
	}
}

// compiledSchema validates messages published to a topic: a message must
// match at least one revision in the topic's allowed range.
type compiledSchema struct {
	encoding   pubsubpb.Encoding
	validators []validator
}

func (c *compiledSchema) validateMessage(data []byte) error {
	var first error
	for _, v := range c.validators {
		err := v.validate(data, c.encoding)
		if err == nil {
			return nil
		}
		if first == nil {
			first = err
		}
	}
	if first == nil {
		return nil
	}
	return first
}

// topicSchema resolves a topic's schema settings (s.mu held).
func (s *Service) topicSchema(cfg *pubsubpb.Topic) *compiledSchema {
	ss := cfg.GetSchemaSettings()
	if ss == nil || ss.GetSchema() == "" {
		return nil
	}
	h := s.schemas[ss.GetSchema()]
	if h == nil {
		return nil
	}
	c := &compiledSchema{encoding: ss.GetEncoding()}
	in := ss.GetFirstRevisionId() == ""
	for _, r := range h.revs {
		if r.RevisionId == ss.GetFirstRevisionId() {
			in = true
		}
		if in {
			if v := h.validators[r.RevisionId]; v != nil {
				c.validators = append(c.validators, v)
			}
		}
		if r.RevisionId == ss.GetLastRevisionId() {
			break
		}
	}
	if len(c.validators) == 0 {
		return nil
	}
	return c
}

// refreshTopicSchemas recompiles schemas of topics using name (s.mu held).
func (s *Service) refreshTopicSchemas(name string) {
	for _, t := range s.topics {
		if t.cfg.GetSchemaSettings().GetSchema() == name {
			t.schema = s.topicSchema(t.cfg)
		}
	}
}

// splitRevision splits "name@revision".
func splitRevision(name string) (string, string) {
	base, rev, _ := strings.Cut(name, "@")
	return base, rev
}

func schemaView(sc *pubsubpb.Schema, view pubsubpb.SchemaView, def pubsubpb.SchemaView) *pubsubpb.Schema {
	out := proto.Clone(sc).(*pubsubpb.Schema)
	if view == pubsubpb.SchemaView_SCHEMA_VIEW_UNSPECIFIED {
		view = def
	}
	if view == pubsubpb.SchemaView_BASIC {
		out.Definition = ""
	}
	return out
}

func (s *Service) newRevision(name string, typ pubsubpb.Schema_Type, def string) (*pubsubpb.Schema, error) {
	if _, err := compileSchema(typ, def); err != nil {
		return nil, apierr.InvalidArgument("Invalid schema definition: %v.", err)
	}
	return &pubsubpb.Schema{
		Name: name, Type: typ, Definition: def,
		RevisionId:         s.env.IDs.Hex(4),
		RevisionCreateTime: timestamppb.New(s.env.Clock.Now()),
	}, nil
}

func (p *schemaServer) CreateSchema(ctx context.Context, req *pubsubpb.CreateSchemaRequest) (*pubsubpb.Schema, error) {
	return p.s.createSchema(ctx, req, true)
}

// createSchema creates a schema; check=false skips IAM (seeding).
func (s *Service) createSchema(ctx context.Context, req *pubsubpb.CreateSchemaRequest, check bool) (*pubsubpb.Schema, error) {
	project, err := parseProject(req.GetParent())
	if err != nil {
		return nil, err
	}
	id := req.GetSchemaId()
	if id == "" {
		id = lastSegment(req.GetSchema().GetName())
	}
	name := req.GetParent() + "/schemas/" + id
	if _, _, err := parseName(name, kindSchemas); err != nil {
		return nil, err
	}
	if err := s.env.EnsureProject(project); err != nil {
		return nil, err
	}
	if check {
		if err := s.env.Auth.Check(ctx, "pubsub.schemas.create", projectResource(project)); err != nil {
			return nil, err
		}
	}
	rev, err := s.newRevision(name, req.GetSchema().GetType(), req.GetSchema().GetDefinition())
	if err != nil {
		return nil, err
	}
	var w writes
	s.mu.Lock()
	if _, ok := s.schemas[name]; ok {
		s.mu.Unlock()
		return nil, alreadyExists(name)
	}
	s.addSchemaRevision(rev)
	w.putProto(nsSchemas, name+"@"+rev.RevisionId, rev)
	s.mu.Unlock()
	return proto.Clone(rev).(*pubsubpb.Schema), s.commit(&w)
}

// schemaRevision looks up name or name@revision (s.mu held).
func (s *Service) schemaRevision(full string) (*schemaHistory, *pubsubpb.Schema, error) {
	name, rev := splitRevision(full)
	if _, _, err := parseName(name, kindSchemas); err != nil {
		return nil, nil, err
	}
	h := s.schemas[name]
	if h == nil {
		return nil, nil, notFound(name)
	}
	if rev == "" {
		return h, h.latest(), nil
	}
	r := h.find(rev)
	if r == nil {
		return nil, nil, apierr.NotFound("Schema revision %s not found.", full)
	}
	return h, r, nil
}

func (p *schemaServer) GetSchema(ctx context.Context, req *pubsubpb.GetSchemaRequest) (*pubsubpb.Schema, error) {
	s := p.s
	base, _ := splitRevision(req.GetName())
	if _, _, err := parseName(base, kindSchemas); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.schemas.get", fullName(base)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, r, err := s.schemaRevision(req.GetName())
	if err != nil {
		return nil, err
	}
	return schemaView(r, req.GetView(), pubsubpb.SchemaView_FULL), nil
}

func (p *schemaServer) ListSchemas(ctx context.Context, req *pubsubpb.ListSchemasRequest) (*pubsubpb.ListSchemasResponse, error) {
	s := p.s
	project, err := parseProject(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.env.EnsureProject(project); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.schemas.list", projectResource(project)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for n := range s.schemas {
		if projectOf(n) == project {
			names = append(names, n)
		}
	}
	page, next, err := paginate(names, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	resp := &pubsubpb.ListSchemasResponse{NextPageToken: next}
	for _, n := range page {
		resp.Schemas = append(resp.Schemas, schemaView(s.schemas[n].latest(), req.GetView(), pubsubpb.SchemaView_BASIC))
	}
	return resp, nil
}

func (p *schemaServer) ListSchemaRevisions(ctx context.Context, req *pubsubpb.ListSchemaRevisionsRequest) (*pubsubpb.ListSchemaRevisionsResponse, error) {
	s := p.s
	if _, _, err := parseName(req.GetName(), kindSchemas); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.schemas.listRevisions", fullName(req.GetName())); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.schemas[req.GetName()]
	if h == nil {
		return nil, notFound(req.GetName())
	}
	// Newest first; the page token is the index of the next revision.
	ids := make([]string, len(h.revs))
	for i := range h.revs {
		ids[i] = h.revs[len(h.revs)-1-i].RevisionId
	}
	start := 0
	if tok := req.GetPageToken(); tok != "" {
		start = -1
		for i, id := range ids {
			if id == tok {
				start = i
			}
		}
		if start < 0 {
			return nil, apierr.InvalidArgument("Invalid page token: %s", tok)
		}
	}
	size := int(req.GetPageSize())
	if size <= 0 || size > 1000 {
		size = 100
	}
	end := min(start+size, len(ids))
	resp := &pubsubpb.ListSchemaRevisionsResponse{}
	for _, id := range ids[start:end] {
		resp.Schemas = append(resp.Schemas, schemaView(h.find(id), req.GetView(), pubsubpb.SchemaView_FULL))
	}
	if end < len(ids) {
		resp.NextPageToken = ids[end]
	}
	return resp, nil
}

func (p *schemaServer) CommitSchema(ctx context.Context, req *pubsubpb.CommitSchemaRequest) (*pubsubpb.Schema, error) {
	s := p.s
	if _, _, err := parseName(req.GetName(), kindSchemas); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.schemas.commit", fullName(req.GetName())); err != nil {
		return nil, err
	}
	s.mu.Lock()
	h := s.schemas[req.GetName()]
	var typ pubsubpb.Schema_Type
	if h != nil {
		typ = h.latest().Type
	}
	s.mu.Unlock()
	if h == nil {
		return nil, notFound(req.GetName())
	}
	if t := req.GetSchema().GetType(); t != pubsubpb.Schema_TYPE_UNSPECIFIED && t != typ {
		return nil, apierr.InvalidArgument("The schema type cannot be changed by a commit.")
	}
	rev, err := s.newRevision(req.GetName(), typ, req.GetSchema().GetDefinition())
	if err != nil {
		return nil, err
	}
	return s.storeRevision(rev)
}

func (s *Service) storeRevision(rev *pubsubpb.Schema) (*pubsubpb.Schema, error) {
	var w writes
	s.mu.Lock()
	if s.schemas[rev.Name] == nil {
		s.mu.Unlock()
		return nil, notFound(rev.Name)
	}
	s.addSchemaRevision(rev)
	s.refreshTopicSchemas(rev.Name)
	w.putProto(nsSchemas, rev.Name+"@"+rev.RevisionId, rev)
	s.mu.Unlock()
	return proto.Clone(rev).(*pubsubpb.Schema), s.commit(&w)
}

func (p *schemaServer) RollbackSchema(ctx context.Context, req *pubsubpb.RollbackSchemaRequest) (*pubsubpb.Schema, error) {
	s := p.s
	if _, _, err := parseName(req.GetName(), kindSchemas); err != nil {
		return nil, err
	}
	if req.GetRevisionId() == "" {
		return nil, apierr.InvalidArgument("The revision_id field must be set.")
	}
	if err := s.env.Auth.Check(ctx, "pubsub.schemas.rollback", fullName(req.GetName())); err != nil {
		return nil, err
	}
	s.mu.Lock()
	_, r, err := s.schemaRevision(req.GetName() + "@" + req.GetRevisionId())
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	rev, err := s.newRevision(req.GetName(), r.Type, r.Definition)
	if err != nil {
		return nil, err
	}
	return s.storeRevision(rev)
}

func (p *schemaServer) DeleteSchemaRevision(ctx context.Context, req *pubsubpb.DeleteSchemaRevisionRequest) (*pubsubpb.Schema, error) {
	s := p.s
	name, rev := splitRevision(req.GetName())
	if rev == "" {
		rev = req.GetRevisionId() //nolint:staticcheck // deprecated but still accepted
	}
	if _, _, err := parseName(name, kindSchemas); err != nil {
		return nil, err
	}
	if rev == "" {
		return nil, apierr.InvalidArgument("The schema revision to delete must be specified as name@revision_id.")
	}
	if err := s.env.Auth.Check(ctx, "pubsub.schemas.deleteRevision", fullName(name)); err != nil {
		return nil, err
	}
	var w writes
	s.mu.Lock()
	defer func() { s.mu.Unlock() }()
	h, r, err := s.schemaRevision(name + "@" + rev)
	if err != nil {
		return nil, err
	}
	if len(h.revs) == 1 {
		return nil, apierr.FailedPrecondition("Cannot delete the only revision of schema %s; delete the schema instead.", name)
	}
	for i, x := range h.revs {
		if x == r {
			h.revs = append(h.revs[:i], h.revs[i+1:]...)
			break
		}
	}
	delete(h.validators, rev)
	s.refreshTopicSchemas(name)
	w.del(nsSchemas, name+"@"+rev)
	s.mu.Unlock()
	err = s.commit(&w)
	s.mu.Lock()
	return proto.Clone(r).(*pubsubpb.Schema), err
}

func (p *schemaServer) DeleteSchema(ctx context.Context, req *pubsubpb.DeleteSchemaRequest) (*emptypb.Empty, error) {
	s := p.s
	if _, _, err := parseName(req.GetName(), kindSchemas); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.schemas.delete", fullName(req.GetName())); err != nil {
		return nil, err
	}
	var w writes
	s.mu.Lock()
	h := s.schemas[req.GetName()]
	if h == nil {
		s.mu.Unlock()
		return nil, notFound(req.GetName())
	}
	delete(s.schemas, req.GetName())
	for _, r := range h.revs {
		w.del(nsSchemas, r.Name+"@"+r.RevisionId)
	}
	for _, t := range s.topics {
		if t.cfg.GetSchemaSettings().GetSchema() == req.GetName() {
			t.cfg.SchemaSettings.Schema = deletedSchema
			t.schema = nil
			w.putProto(nsTopics, t.name, t.cfg)
		}
	}
	s.mu.Unlock()
	return &emptypb.Empty{}, s.commit(&w)
}

func (p *schemaServer) ValidateSchema(ctx context.Context, req *pubsubpb.ValidateSchemaRequest) (*pubsubpb.ValidateSchemaResponse, error) {
	s := p.s
	project, err := parseProject(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.schemas.validate", projectResource(project)); err != nil {
		return nil, err
	}
	if _, err := compileSchema(req.GetSchema().GetType(), req.GetSchema().GetDefinition()); err != nil {
		return nil, apierr.InvalidArgument("Invalid schema definition: %v.", err)
	}
	return &pubsubpb.ValidateSchemaResponse{}, nil
}

func (p *schemaServer) ValidateMessage(ctx context.Context, req *pubsubpb.ValidateMessageRequest) (*pubsubpb.ValidateMessageResponse, error) {
	s := p.s
	project, err := parseProject(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.schemas.validate", projectResource(project)); err != nil {
		return nil, err
	}
	var v validator
	if name := req.GetName(); name != "" {
		full := name
		if !strings.HasPrefix(name, "projects/") {
			full = req.GetParent() + "/schemas/" + name
		}
		s.mu.Lock()
		h, r, err := s.schemaRevision(full)
		if err == nil {
			v = h.validators[r.RevisionId]
		}
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
	} else {
		v, err = compileSchema(req.GetSchema().GetType(), req.GetSchema().GetDefinition())
		if err != nil {
			return nil, apierr.InvalidArgument("Invalid schema definition: %v.", err)
		}
	}
	enc := req.GetEncoding()
	if enc == pubsubpb.Encoding_ENCODING_UNSPECIFIED {
		enc = pubsubpb.Encoding_JSON
	}
	if v != nil {
		if err := v.validate(req.GetMessage(), enc); err != nil {
			return nil, apierr.InvalidArgument("Message failed schema validation: %v.", err)
		}
	}
	return &pubsubpb.ValidateMessageResponse{}, nil
}
