package gcs

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

var _ emu.SeedExporter = (*Service)(nil)

// Seeding (FR-CORE-011): buckets with objects from local paths and
// notification configs. Applying a seed is idempotent. Other bucket keys
// (storageClass, cors, lifecycle, retentionPeriod, ...) take the JSON API
// field names.
//
//	gcs:
//	  buckets:
//	    - name: my-assets
//	      project: my-project
//	      location: US
//	      versioning: true
//	      uniformBucketLevelAccess: true
//	      labels: {env: dev}
//	      website: {mainPageSuffix: index.html}
//	      objects:
//	        - path: ./site          # a directory is uploaded recursively
//	          name: static/         # optional name prefix (or full name for a file)
//	        - name: hello.txt
//	          content: "hi"
//	          contentType: text/plain
//	        - name: logo.png
//	          contentBase64: iVBORw0KGgo=  # binary content
//	      notifications:
//	        - topic: projects/my-project/topics/uploads
//	          eventTypes: [OBJECT_FINALIZE]

type seedSection struct {
	Buckets []map[string]any `yaml:"buckets"`
}

type seedObject struct {
	Name          string            `yaml:"name,omitempty" json:"name"`
	Path          string            `yaml:"path,omitempty" json:"path"`
	Content       *string           `yaml:"content,omitempty" json:"content"`
	ContentBase64 string            `yaml:"contentBase64,omitempty" json:"contentBase64"`
	ContentType   string            `yaml:"contentType,omitempty" json:"contentType"`
	CacheControl  string            `yaml:"cacheControl,omitempty" json:"cacheControl"`
	Metadata      map[string]string `yaml:"metadata,omitempty" json:"metadata"`
}

type seedNotification struct {
	Topic            string            `yaml:"topic" json:"topic"`
	PayloadFormat    string            `yaml:"payloadFormat,omitempty" json:"payloadFormat"`
	EventTypes       []string          `yaml:"eventTypes,omitempty" json:"eventTypes"`
	ObjectNamePrefix string            `yaml:"objectNamePrefix,omitempty" json:"objectNamePrefix"`
	CustomAttributes map[string]string `yaml:"customAttributes,omitempty" json:"customAttributes"`
}

type systemKey struct{}

// system marks a context as an internal caller that skips IAM checks
// (seeding).
func system(ctx context.Context) context.Context { return context.WithValue(ctx, systemKey{}, true) }

// check authorizes the caller unless the context is a system context.
func (s *Service) check(ctx context.Context, permission, resource string) error {
	if v, _ := ctx.Value(systemKey{}).(bool); v {
		return nil
	}
	return s.env.Auth.Check(ctx, permission, resource)
}

// ApplySeed implements emu.Seeder.
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var sec seedSection
	if err := section.Decode(&sec); err != nil {
		return err
	}
	ctx = system(ctx)
	for i, raw := range sec.Buckets {
		if err := s.seedBucket(ctx, raw, baseDir); err != nil {
			return fmt.Errorf("buckets[%d]: %w", i, err)
		}
	}
	return nil
}

// seedBucketOut is one exported bucket. Rest holds the remaining bucket
// fields under their JSON API names; MarshalYAML puts objects and
// notifications after them.
type seedBucketOut struct {
	Name                     string             `yaml:"name"`
	Project                  string             `yaml:"project,omitempty"`
	Location                 string             `yaml:"location,omitempty"`
	StorageClass             string             `yaml:"storageClass,omitempty"`
	Versioning               bool               `yaml:"versioning,omitempty"`
	UniformBucketLevelAccess bool               `yaml:"uniformBucketLevelAccess,omitempty"`
	RetentionPeriod          string             `yaml:"retentionPeriod,omitempty"`
	Labels                   map[string]string  `yaml:"labels,omitempty"`
	Lifecycle                []any              `yaml:"lifecycle,omitempty"`
	Rest                     map[string]any     `yaml:",inline"`
	Objects                  []seedObject       `yaml:"-"`
	Notifications            []seedNotification `yaml:"-"`
}

// MarshalYAML implements yaml.Marshaler.
func (b seedBucketOut) MarshalYAML() (any, error) {
	type plain seedBucketOut
	var n yaml.Node
	if err := n.Encode(plain(b)); err != nil {
		return nil, err
	}
	for _, kv := range []struct {
		key string
		val any
		ok  bool
	}{{"objects", b.Objects, len(b.Objects) > 0}, {"notifications", b.Notifications, len(b.Notifications) > 0}} {
		if !kv.ok {
			continue
		}
		var v yaml.Node
		if err := v.Encode(kv.val); err != nil {
			return nil, err
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: kv.key}, &v)
	}
	return &n, nil
}

// serverBucketFields are bucket fields the server assigns or seeding
// cannot set.
var serverBucketFields = []string{
	"kind", "id", "selfLink", "projectNumber", "timeCreated", "updated", "metageneration",
	"generation", "etag", "owner", "acl", "defaultObjectAcl", "locationType", "objectRetention",
	"name", "location", "storageClass", "versioning", "iamConfiguration", "retentionPolicy",
	"labels", "lifecycle",
}

// ExportSeed implements emu.SeedExporter: every bucket with the live
// generation of its objects (inline content) and its notification configs.
func (s *Service) ExportSeed(ctx context.Context) (any, error) {
	var out []seedBucketOut
	err := s.env.Store.View(func(tx store.Tx) error {
		recs, err := store.ListJSON[*bucketRec](tx, nsBuckets, "")
		if err != nil {
			return err
		}
		for _, rec := range recs {
			b, err := exportBucket(rec)
			if err != nil {
				return err
			}
			objs, err := store.ListJSON[*objectRec](tx, nsObjects, objPrefix(b.Name, ""))
			if err != nil {
				return err
			}
			for _, o := range objs {
				so, err := s.exportObject(o)
				if err != nil {
					return fmt.Errorf("object %s/%s: %w", b.Name, o.Object.Name, err)
				}
				b.Objects = append(b.Objects, so)
			}
			ns, err := store.ListJSON[*storage.Notification](tx, nsNotifs, objPrefix(b.Name, ""))
			if err != nil {
				return err
			}
			sort.Slice(ns, func(i, j int) bool {
				a, _ := strconv.Atoi(ns[i].Id)
				c, _ := strconv.Atoi(ns[j].Id)
				return a < c
			})
			for _, n := range ns {
				sn := seedNotification{
					Topic:      strings.TrimPrefix(n.Topic, "//pubsub.googleapis.com/"),
					EventTypes: n.EventTypes, ObjectNamePrefix: n.ObjectNamePrefix, CustomAttributes: n.CustomAttributes,
				}
				if n.PayloadFormat != payloadJSON {
					sn.PayloadFormat = n.PayloadFormat
				}
				b.Notifications = append(b.Notifications, sn)
			}
			out = append(out, b)
		}
		return nil
	})
	if err != nil || len(out) == 0 {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Name < out[j].Name
	})
	return map[string]any{"buckets": out}, nil
}

// exportBucket converts a bucket to its seed form, leaving out
// server-assigned fields and defaults.
func exportBucket(rec *bucketRec) (seedBucketOut, error) {
	b := rec.Bucket
	out := seedBucketOut{Name: b.Name, Project: rec.Project, Labels: b.Labels}
	if b.Location != "US" {
		out.Location = b.Location
	}
	if b.StorageClass != "STANDARD" {
		out.StorageClass = b.StorageClass
	}
	out.Versioning = b.Versioning != nil && b.Versioning.Enabled
	if b.RetentionPolicy != nil && b.RetentionPolicy.RetentionPeriod > 0 {
		out.RetentionPeriod = strconv.FormatInt(b.RetentionPolicy.RetentionPeriod, 10)
	}
	var rest map[string]any
	if err := remarshal(b, &rest); err != nil {
		return out, err
	}
	if b.Lifecycle != nil && len(b.Lifecycle.Rule) > 0 {
		var lc struct {
			Rule []any `json:"rule"`
		}
		if err := remarshal(b.Lifecycle, &lc); err != nil {
			return out, err
		}
		out.Lifecycle = lc.Rule
	}
	for _, k := range serverBucketFields {
		delete(rest, k)
	}
	if ic := b.IamConfiguration; ic != nil {
		ubla := ic.UniformBucketLevelAccess != nil && ic.UniformBucketLevelAccess.Enabled
		if ic.PublicAccessPrevention != "" && ic.PublicAccessPrevention != "inherited" {
			m := map[string]any{"publicAccessPrevention": ic.PublicAccessPrevention}
			if ubla {
				m["uniformBucketLevelAccess"] = map[string]any{"enabled": true}
			}
			rest["iamConfiguration"] = m
		} else {
			out.UniformBucketLevelAccess = ubla
		}
	}
	if b.Rpo == "DEFAULT" {
		delete(rest, "rpo")
	}
	if sd := b.SoftDeletePolicy; sd != nil {
		if sd.RetentionDurationSeconds == defaultSoftDeleteSeconds {
			delete(rest, "softDeletePolicy")
		} else {
			rest["softDeletePolicy"] = map[string]any{"retentionDurationSeconds": strconv.FormatInt(sd.RetentionDurationSeconds, 10)}
		}
	}
	if len(rest) > 0 {
		out.Rest = rest
	}
	return out, nil
}

// exportObject returns an object's seed form with its content inline:
// content for UTF-8 text, contentBase64 otherwise.
func (s *Service) exportObject(rec *objectRec) (seedObject, error) {
	o := rec.Object
	so := seedObject{Name: o.Name, CacheControl: o.CacheControl, Metadata: o.Metadata}
	if o.ContentType != "" && o.ContentType != mime.TypeByExtension(path.Ext(o.Name)) {
		so.ContentType = o.ContentType
	}
	f, err := s.blobs.open(rec.Blob)
	if err != nil {
		return so, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return so, err
	}
	if utf8.Valid(data) {
		c := string(data)
		so.Content = &c
	} else {
		so.ContentBase64 = base64.StdEncoding.EncodeToString(data)
	}
	return so, nil
}

func popString(m map[string]any, k string) string {
	v, _ := m[k].(string)
	delete(m, k)
	return v
}

func (s *Service) seedBucket(ctx context.Context, raw map[string]any, baseDir string) error {
	m := map[string]any{}
	for k, v := range raw {
		m[k] = v
	}
	proj := popString(m, "project")
	if proj == "" && len(s.env.Config.Projects) > 0 {
		proj = s.env.Config.Projects[0]
	}
	if proj == "" {
		return fmt.Errorf("project is required")
	}
	var objs []seedObject
	var notifs []seedNotification
	if err := remarshal(m["objects"], &objs); err != nil {
		return fmt.Errorf("objects: %w", err)
	}
	if err := remarshal(m["notifications"], &notifs); err != nil {
		return fmt.Errorf("notifications: %w", err)
	}
	delete(m, "objects")
	delete(m, "notifications")
	if v, ok := m["versioning"].(bool); ok {
		m["versioning"] = map[string]any{"enabled": v}
	}
	if v, ok := m["uniformBucketLevelAccess"].(bool); ok {
		delete(m, "uniformBucketLevelAccess")
		m["iamConfiguration"] = map[string]any{"uniformBucketLevelAccess": map[string]any{"enabled": v}}
	}
	if v, ok := m["retentionPeriod"]; ok {
		delete(m, "retentionPeriod")
		m["retentionPolicy"] = map[string]any{"retentionPeriod": fmt.Sprint(v)}
	}
	if v, ok := m["lifecycle"].([]any); ok {
		m["lifecycle"] = map[string]any{"rule": v}
	}
	var b storage.Bucket
	if err := remarshal(m, &b); err != nil {
		return err
	}
	if _, err := s.getBucketRec(b.Name); err != nil {
		if _, err := s.createBucket(ctx, proj, &b, false, nil); err != nil {
			return err
		}
	}
	for _, o := range objs {
		if err := s.seedObjects(ctx, b.Name, o, baseDir); err != nil {
			return fmt.Errorf("object %q: %w", o.Name+o.Path, err)
		}
	}
	for _, n := range notifs {
		if err := s.seedNotification(ctx, b.Name, n); err != nil {
			return fmt.Errorf("notification %s: %w", n.Topic, err)
		}
	}
	return nil
}

// remarshal converts YAML-decoded values into a JSON-tagged type.
func remarshal(in, out any) error {
	if in == nil {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (s *Service) seedObjects(ctx context.Context, bucket string, o seedObject, baseDir string) error {
	if o.Content != nil {
		return s.seedOne(ctx, bucket, o, o.Name, []byte(*o.Content))
	}
	if o.ContentBase64 != "" {
		data, err := base64.StdEncoding.DecodeString(o.ContentBase64)
		if err != nil {
			return fmt.Errorf("contentBase64: %w", err)
		}
		return s.seedOne(ctx, bucket, o, o.Name, data)
	}
	if o.Path == "" {
		return fmt.Errorf("one of path, content or contentBase64 is required")
	}
	p := o.Path
	if !filepath.IsAbs(p) {
		p = filepath.Join(baseDir, p)
	}
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		name := o.Name
		if name == "" || strings.HasSuffix(name, "/") {
			name += filepath.Base(p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return s.seedOne(ctx, bucket, o, name, data)
	}
	return filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(p, fp)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(fp)
		if err != nil {
			return err
		}
		return s.seedOne(ctx, bucket, o, path.Join(strings.TrimSuffix(o.Name, "/"), filepath.ToSlash(rel)), data)
	})
}

// seedOne uploads data unless the object already has identical content.
func (s *Service) seedOne(ctx context.Context, bucket string, o seedObject, name string, data []byte) error {
	name = strings.TrimPrefix(name, "/")
	sum := md5.Sum(data)
	want := base64.StdEncoding.EncodeToString(sum[:])
	var cur *objectRec
	_ = s.env.Store.View(func(tx store.Tx) error {
		cur, _, _ = loadObject(tx, bucket, name, 0)
		return nil
	})
	if cur != nil && cur.Object.Md5Hash == want {
		return nil
	}
	ct := o.ContentType
	if ct == "" {
		ct = mime.TypeByExtension(path.Ext(name))
	}
	meta := &storage.Object{Name: name, ContentType: ct, CacheControl: o.CacheControl, Metadata: o.Metadata}
	_, _, err := s.putObject(ctx, bucket, meta, io.Reader(bytes.NewReader(data)), "", nil, "")
	return err
}

func (s *Service) seedNotification(ctx context.Context, bucket string, n seedNotification) error {
	topic, ok := normalizeTopic(n.Topic)
	if !ok {
		return fmt.Errorf("invalid topic")
	}
	var exists bool
	_ = s.env.Store.View(func(tx store.Tx) error {
		list, _ := store.ListJSON[*storage.Notification](tx, nsNotifs, objPrefix(bucket, ""))
		for _, c := range list {
			if c.Topic == topic && c.ObjectNamePrefix == n.ObjectNamePrefix && strings.Join(c.EventTypes, ",") == strings.Join(n.EventTypes, ",") {
				exists = true
			}
		}
		return nil
	})
	if exists {
		return nil
	}
	_, err := s.addNotification(ctx, bucket, &storage.Notification{
		Topic: topic, PayloadFormat: n.PayloadFormat, EventTypes: n.EventTypes,
		ObjectNamePrefix: n.ObjectNamePrefix, CustomAttributes: n.CustomAttributes,
	})
	return err
}
