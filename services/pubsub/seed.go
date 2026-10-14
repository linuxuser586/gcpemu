package pubsub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// seedFile is the "pubsub" section of a seed file (FR-CORE-011). Resources
// use the REST JSON field names; names may be full resource names or bare
// IDs combined with project (per item or section-wide):
//
//	pubsub:
//	  project: my-project
//	  schemas:
//	    - name: events
//	      type: AVRO
//	      definitionFile: schemas/event.avsc  # or an inline definition
//	  topics:
//	    - name: events
//	      labels: {env: dev}
//	      schemaSettings: {schema: events, encoding: JSON}
//	  subscriptions:
//	    - name: events-worker
//	      topic: events
//	      ackDeadlineSeconds: 30
//	      filter: attributes.type = "order"
//	      deadLetterPolicy: {deadLetterTopic: events-dlq, maxDeliveryAttempts: 5}
//	      pushConfig: {pushEndpoint: http://localhost:8080/push}
type seedFile struct {
	Project       string           `yaml:"project"`
	Schemas       []map[string]any `yaml:"schemas"`
	Topics        []map[string]any `yaml:"topics"`
	Subscriptions []map[string]any `yaml:"subscriptions"`
}

// ApplySeed creates (or updates) the seeded resources idempotently.
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var sf seedFile
	if err := section.Decode(&sf); err != nil {
		return err
	}
	for i, item := range sf.Schemas {
		if err := s.seedSchema(ctx, sf.Project, item, baseDir); err != nil {
			return fmt.Errorf("schemas[%d]: %w", i, err)
		}
	}
	for i, item := range sf.Topics {
		if err := s.seedTopic(ctx, sf.Project, item); err != nil {
			return fmt.Errorf("topics[%d]: %w", i, err)
		}
	}
	for i, item := range sf.Subscriptions {
		if err := s.seedSubscription(ctx, sf.Project, item); err != nil {
			return fmt.Errorf("subscriptions[%d]: %w", i, err)
		}
	}
	return nil
}

// qualify turns an ID into a full resource name.
func qualify(v any, project, kind string) (string, error) {
	s, _ := v.(string)
	if s == "" || strings.HasPrefix(s, "projects/") {
		return s, nil
	}
	if project == "" {
		return "", fmt.Errorf("%q is not a full resource name and no project is set", s)
	}
	return "projects/" + project + "/" + kind + "/" + s, nil
}

func itemProject(item map[string]any, def string) string {
	if p, ok := item["project"].(string); ok && p != "" {
		delete(item, "project")
		return p
	}
	delete(item, "project")
	return def
}

func toProto(item map[string]any, m proto.Message) error {
	b, err := json.Marshal(item)
	if err != nil {
		return err
	}
	return unmarshalOpts.Unmarshal(b, m)
}

func (s *Service) seedSchema(ctx context.Context, defProject string, item map[string]any, baseDir string) error {
	project := itemProject(item, defProject)
	name, err := qualify(item["name"], project, kindSchemas)
	if err != nil {
		return err
	}
	if f, ok := item["definitionFile"].(string); ok {
		if !filepath.IsAbs(f) {
			f = filepath.Join(baseDir, f)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		item["definition"] = string(b)
		delete(item, "definitionFile")
	}
	item["name"] = name
	sc := &pubsubpb.Schema{}
	if err := toProto(item, sc); err != nil {
		return err
	}
	s.mu.Lock()
	h := s.schemas[name]
	same := h != nil && h.latest().Definition == sc.Definition && h.latest().Type == sc.Type
	s.mu.Unlock()
	if same {
		return nil
	}
	if h != nil {
		rev, err := s.newRevision(name, sc.Type, sc.Definition)
		if err != nil {
			return err
		}
		_, err = s.storeRevision(rev)
		return err
	}
	parent := "projects/" + projectOf(name)
	_, err = s.createSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, Schema: sc, SchemaId: lastSegment(name)}, false)
	return err
}

func (s *Service) seedTopic(ctx context.Context, defProject string, item map[string]any) error {
	project := itemProject(item, defProject)
	name, err := qualify(item["name"], project, kindTopics)
	if err != nil {
		return err
	}
	item["name"] = name
	if ss, ok := item["schemaSettings"].(map[string]any); ok {
		if ss["schema"], err = qualify(ss["schema"], projectOf(name), kindSchemas); err != nil {
			return err
		}
	}
	t := &pubsubpb.Topic{}
	if err := toProto(item, t); err != nil {
		return err
	}
	_, err = s.createTopic(ctx, t, false)
	if e := apierr.From(err); err != nil && e.Code == codes.AlreadyExists {
		if err := s.validateTopic(t); err != nil {
			return err
		}
		var w writes
		s.mu.Lock()
		if cur := s.topics[name]; cur != nil {
			cur.cfg = t
			cur.schema = s.topicSchema(t)
			w.putProto(nsTopics, name, t)
		}
		s.mu.Unlock()
		return s.commit(&w)
	}
	return err
}

func (s *Service) seedSubscription(ctx context.Context, defProject string, item map[string]any) error {
	project := itemProject(item, defProject)
	name, err := qualify(item["name"], project, kindSubscriptions)
	if err != nil {
		return err
	}
	item["name"] = name
	if item["topic"], err = qualify(item["topic"], projectOf(name), kindTopics); err != nil {
		return err
	}
	if dl, ok := item["deadLetterPolicy"].(map[string]any); ok {
		if dl["deadLetterTopic"], err = qualify(dl["deadLetterTopic"], projectOf(name), kindTopics); err != nil {
			return err
		}
	}
	cfg := &pubsubpb.Subscription{}
	if err := toProto(item, cfg); err != nil {
		return err
	}
	_, err = s.createSubscription(ctx, cfg, false)
	if e := apierr.From(err); err != nil && e.Code == codes.AlreadyExists {
		s.mu.Lock()
		sb := s.subs[name]
		s.mu.Unlock()
		if sb == nil {
			return nil
		}
		_, err = s.replaceSubscription(sb, cfg)
	}
	return err
}

// seedItem is one exported resource: its ID, its project, a schema's
// type or a subscription's topic, and the rest of its REST JSON fields.
type seedItem struct {
	Name    string         `yaml:"name"`
	Project string         `yaml:"project"`
	Type    string         `yaml:"type,omitempty"`
	Topic   string         `yaml:"topic,omitempty"`
	Rest    map[string]any `yaml:",inline"`
}

// seedOut is the exported section.
type seedOut struct {
	Schemas       []seedItem `yaml:"schemas,omitempty"`
	Topics        []seedItem `yaml:"topics,omitempty"`
	Subscriptions []seedItem `yaml:"subscriptions,omitempty"`
}

// ExportSeed implements emu.SeedExporter: the latest revision of each
// schema (with an inline definition), topics and subscriptions. Messages,
// snapshots, older schema revisions and pinned revision IDs are not
// exported.
func (s *Service) ExportSeed(ctx context.Context) (any, error) {
	s.mu.Lock()
	schemas := make([]proto.Message, 0, len(s.schemas))
	for _, h := range s.schemas {
		schemas = append(schemas, proto.Clone(h.latest()))
	}
	topics := make([]proto.Message, 0, len(s.topics))
	for _, t := range s.topics {
		topics = append(topics, proto.Clone(t.cfg))
	}
	subs := make([]proto.Message, 0, len(s.subs))
	for _, sb := range s.subs {
		subs = append(subs, proto.Clone(sb.cfg))
	}
	s.mu.Unlock()

	var out seedOut
	var err error
	if out.Schemas, err = exportItems(schemas, func(m map[string]any) {
		delete(m, "revisionId")
		delete(m, "revisionCreateTime")
	}); err != nil {
		return nil, err
	}
	if out.Topics, err = exportItems(topics, func(m map[string]any) {
		delete(m, "state")
		if ss, ok := m["schemaSettings"].(map[string]any); ok {
			// Revision IDs are random, so a pinned range cannot be
			// recreated.
			delete(ss, "firstRevisionId")
			delete(ss, "lastRevisionId")
			if sc, _ := ss["schema"].(string); sc == deletedSchema {
				delete(m, "schemaSettings")
			}
		}
	}); err != nil {
		return nil, err
	}
	out.Subscriptions, err = exportItems(subs, func(m map[string]any) {
		for _, k := range []string{"state", "detached", "topicMessageRetentionDuration"} {
			delete(m, k)
		}
		dropDefault(m, "ackDeadlineSeconds", float64(defaultAckDeadline))
		dropDefault(m, "messageRetentionDuration", durationJSON(defaultSubRetention))
		if ep, ok := m["expirationPolicy"].(map[string]any); ok && len(ep) == 1 && ep["ttl"] == durationJSON(defaultExpirationTTL) {
			delete(m, "expirationPolicy")
		}
		if pc, ok := m["pushConfig"].(map[string]any); ok && len(pc) == 0 {
			delete(m, "pushConfig")
		}
		if dl, ok := m["deadLetterPolicy"].(map[string]any); ok {
			dropDefault(dl, "maxDeliveryAttempts", float64(defaultMaxDeliveryAttempts))
		}
	})
	if err != nil {
		return nil, err
	}
	if len(out.Schemas)+len(out.Topics)+len(out.Subscriptions) == 0 {
		return nil, nil
	}
	return out, nil
}

// exportItems converts resources to seed items sorted by project then ID.
// References to resources in the same project are shortened to IDs.
func exportItems(msgs []proto.Message, clean func(map[string]any)) ([]seedItem, error) {
	items := make([]seedItem, 0, len(msgs))
	for _, msg := range msgs {
		b, err := marshalOpts.Marshal(msg)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		name, _ := m["name"].(string)
		delete(m, "name")
		clean(m)
		project := projectOf(name)
		shorten(m, "topic", project)
		if ss, ok := m["schemaSettings"].(map[string]any); ok {
			shorten(ss, "schema", project)
		}
		if dl, ok := m["deadLetterPolicy"].(map[string]any); ok {
			shorten(dl, "deadLetterTopic", project)
		}
		it := seedItem{Name: lastSegment(name), Project: project}
		it.Type, _ = m["type"].(string)
		it.Topic, _ = m["topic"].(string)
		delete(m, "type")
		delete(m, "topic")
		if len(m) > 0 {
			it.Rest = m
		}
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Project != items[j].Project {
			return items[i].Project < items[j].Project
		}
		return items[i].Name < items[j].Name
	})
	if len(items) == 0 {
		return nil, nil
	}
	return items, nil
}

// shorten replaces m[k], a resource name, with its ID when it is in project.
func shorten(m map[string]any, k, project string) {
	if v, ok := m[k].(string); ok && projectOf(v) == project {
		m[k] = lastSegment(v)
	}
}

func dropDefault(m map[string]any, k string, def any) {
	if m[k] == def {
		delete(m, k)
	}
}

// durationJSON is d in protojson's Duration form.
func durationJSON(d time.Duration) string {
	b, _ := marshalOpts.Marshal(durationpb.New(d))
	var s string
	_ = json.Unmarshal(b, &s)
	return s
}
