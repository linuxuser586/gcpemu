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
	"strings"

	storage "google.golang.org/api/storage/v1"
	"gopkg.in/yaml.v3"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// Seeding (FR-CORE-011): buckets with objects from local paths and
// notification configs. Applying a seed is idempotent.
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
//	      notifications:
//	        - topic: projects/my-project/topics/uploads
//	          eventTypes: [OBJECT_FINALIZE]

type seedSection struct {
	Buckets []map[string]any `yaml:"buckets"`
}

type seedObject struct {
	Name         string            `yaml:"name" json:"name"`
	Path         string            `yaml:"path" json:"path"`
	Content      *string           `yaml:"content" json:"content"`
	ContentType  string            `yaml:"contentType" json:"contentType"`
	CacheControl string            `yaml:"cacheControl" json:"cacheControl"`
	Metadata     map[string]string `yaml:"metadata" json:"metadata"`
}

type seedNotification struct {
	Topic            string            `json:"topic"`
	PayloadFormat    string            `json:"payloadFormat"`
	EventTypes       []string          `json:"eventTypes"`
	ObjectNamePrefix string            `json:"objectNamePrefix"`
	CustomAttributes map[string]string `json:"customAttributes"`
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
		if _, err := s.createBucket(ctx, proj, &b, false); err != nil {
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
	if o.Path == "" {
		return fmt.Errorf("one of path or content is required")
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
