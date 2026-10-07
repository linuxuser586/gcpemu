package gcs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	storage "google.golang.org/api/storage/v1"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Pub/Sub notifications (FR-GCS-007, FR-INT-010).

// Event types.
const (
	evFinalize       = "OBJECT_FINALIZE"
	evMetadataUpdate = "OBJECT_METADATA_UPDATE"
	evDelete         = "OBJECT_DELETE"
	evArchive        = "OBJECT_ARCHIVE"
)

// Payload formats.
const (
	payloadJSON = "JSON_API_V1"
	payloadNone = "NONE"
)

// event is one object change to notify about.
type event struct {
	typ   string
	rec   *objectRec
	attrs map[string]string
}

// publisher returns the Pub/Sub peer, or nil when pubsub is not running.
func (s *Service) publisher() emu.Publisher {
	svc, ok := s.env.Lookup("pubsub")
	if !ok {
		return nil
	}
	p, _ := svc.(emu.Publisher)
	return p
}

// publish sends events to every matching notification config of bucket.
// Failures are logged: notification delivery never fails the API call.
func (s *Service) publish(ctx context.Context, bucket string, bkt *storage.Bucket, base string, events []event) {
	if len(events) == 0 {
		return
	}
	var cfgs []*storage.Notification
	_ = s.env.Store.View(func(tx store.Tx) error {
		var err error
		cfgs, err = store.ListJSON[*storage.Notification](tx, nsNotifs, objPrefix(bucket, ""))
		return err
	})
	if len(cfgs) == 0 {
		return
	}
	pub := s.publisher()
	if pub == nil {
		s.log.Debug("pubsub not running; skipping bucket notifications", "bucket", bucket, "events", len(events))
		return
	}
	if base == "" {
		base = "https://www.googleapis.com"
	}
	ctx = context.WithoutCancel(ctx)
	eventTime := s.now().Format("2006-01-02T15:04:05.000000Z07:00")
	for _, ev := range events {
		o := ev.rec.Object
		for _, cfg := range cfgs {
			if !strings.HasPrefix(o.Name, cfg.ObjectNamePrefix) {
				continue
			}
			if len(cfg.EventTypes) > 0 && !contains(cfg.EventTypes, ev.typ) {
				continue
			}
			attrs := map[string]string{}
			for k, v := range cfg.CustomAttributes {
				attrs[k] = v
			}
			attrs["notificationConfig"] = fmt.Sprintf("projects/_/buckets/%s/notificationConfigs/%s", bucket, cfg.Id)
			attrs["eventType"] = ev.typ
			attrs["payloadFormat"] = cfg.PayloadFormat
			attrs["bucketId"] = bucket
			attrs["objectId"] = o.Name
			attrs["objectGeneration"] = strconv.FormatInt(o.Generation, 10)
			attrs["eventTime"] = eventTime
			for k, v := range ev.attrs {
				attrs[k] = v
			}
			var data []byte
			if cfg.PayloadFormat == payloadJSON {
				data, _ = json.Marshal(renderObject(ev.rec, bkt, base))
			}
			topic := strings.TrimPrefix(cfg.Topic, "//pubsub.googleapis.com/")
			if _, err := pub.PublishInternal(ctx, topic, data, attrs); err != nil {
				s.log.Warn("bucket notification publish failed", "bucket", bucket, "topic", topic, "err", err)
			}
		}
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// normalizeTopic accepts "projects/P/topics/T" or the full resource name
// and returns "//pubsub.googleapis.com/projects/P/topics/T".
func normalizeTopic(t string) (string, bool) {
	t = strings.TrimPrefix(t, "//pubsub.googleapis.com/")
	parts := strings.Split(t, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "topics" || parts[1] == "" || parts[3] == "" {
		return "", false
	}
	return "//pubsub.googleapis.com/" + t, true
}

func (s *Service) insertNotification(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := s.check(r.Context(), "storage.buckets.update", bucketResource(bucket)); err != nil {
		apierr.Write(w, err)
		return
	}
	var n storage.Notification
	if err := decodeJSON(r, &n); err != nil {
		apierr.Write(w, err)
		return
	}
	out, err := s.addNotification(r.Context(), bucket, &n)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderNotification(out, bucket, baseURL(r)))
}

// addNotification validates and stores a notification config.
func (s *Service) addNotification(ctx context.Context, bucket string, n *storage.Notification) (*storage.Notification, error) {
	if n.Topic == "" {
		return nil, errRequired("topic")
	}
	topic, ok := normalizeTopic(n.Topic)
	if !ok {
		return nil, errInvalid("Invalid Cloud Pub/Sub topic name: %s", n.Topic)
	}
	n.Topic = topic
	switch n.PayloadFormat {
	case "":
		n.PayloadFormat = payloadJSON
	case payloadJSON, payloadNone:
	default:
		return nil, errInvalid("Invalid payload format: %s", n.PayloadFormat)
	}
	for _, et := range n.EventTypes {
		switch et {
		case evFinalize, evMetadataUpdate, evDelete, evArchive:
		default:
			return nil, errInvalid("Invalid event type: %s", et)
		}
	}
	if pub := s.publisher(); pub != nil {
		short := strings.TrimPrefix(topic, "//pubsub.googleapis.com/")
		if !pub.TopicExists(ctx, short) {
			return nil, errInvalid("The Cloud Pub/Sub topic %s does not exist.", short)
		}
	}
	err := s.env.Store.Update(func(tx store.Tx) error {
		rec, err := loadBucket(tx, bucket)
		if err != nil {
			return err
		}
		rec.NextNotification++
		n.Id = strconv.Itoa(rec.NextNotification)
		n.Kind = "storage#notification"
		n.Etag = n.Id
		n.SelfLink = ""
		if err := store.PutJSON(tx, nsBuckets, bucket, rec); err != nil {
			return err
		}
		return store.PutJSON(tx, nsNotifs, objKey(bucket, n.Id), n)
	})
	if err != nil {
		return nil, err
	}
	return n, nil
}

func renderNotification(n *storage.Notification, bucket, base string) *storage.Notification {
	c := *n
	c.SelfLink = base + "/storage/v1/b/" + bucket + "/notificationConfigs/" + n.Id
	return &c
}

func (s *Service) getNotification(w http.ResponseWriter, r *http.Request, bucket, id string) {
	if err := s.check(r.Context(), "storage.buckets.get", bucketResource(bucket)); err != nil {
		apierr.Write(w, err)
		return
	}
	var n storage.Notification
	err := s.env.Store.View(func(tx store.Tx) error {
		if _, err := loadBucket(tx, bucket); err != nil {
			return err
		}
		if err := store.GetJSON(tx, nsNotifs, objKey(bucket, id), &n); err != nil {
			if err == store.ErrNotFound {
				return apierr.NotFound("No such notification configuration: %s", id)
			}
			return err
		}
		return nil
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, renderNotification(&n, bucket, baseURL(r)))
}

func (s *Service) listNotifications(w http.ResponseWriter, r *http.Request, bucket string) {
	if err := s.check(r.Context(), "storage.buckets.get", bucketResource(bucket)); err != nil {
		apierr.Write(w, err)
		return
	}
	out := &storage.Notifications{Kind: "storage#notifications"}
	base := baseURL(r)
	err := s.env.Store.View(func(tx store.Tx) error {
		if _, err := loadBucket(tx, bucket); err != nil {
			return err
		}
		list, err := store.ListJSON[*storage.Notification](tx, nsNotifs, objPrefix(bucket, ""))
		for _, n := range list {
			out.Items = append(out.Items, renderNotification(n, bucket, base))
		}
		return err
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) deleteNotification(w http.ResponseWriter, r *http.Request, bucket, id string) {
	if err := s.check(r.Context(), "storage.buckets.update", bucketResource(bucket)); err != nil {
		apierr.Write(w, err)
		return
	}
	err := s.env.Store.Update(func(tx store.Tx) error {
		if _, err := loadBucket(tx, bucket); err != nil {
			return err
		}
		if !store.Exists(tx, nsNotifs, objKey(bucket, id)) {
			return apierr.NotFound("No such notification configuration: %s", id)
		}
		return tx.Delete(nsNotifs, objKey(bucket, id))
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
