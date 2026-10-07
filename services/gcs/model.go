package gcs

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	storage "google.golang.org/api/storage/v1"
)

// Store namespaces. Object keys are "<bucket>\x00<name>" so a prefix scan
// over "<bucket>\x00<prefix>" yields a bucket's objects in name order.
const (
	nsBuckets  = "gcs/buckets"       // name → bucketRec
	nsObjects  = "gcs/objects"       // bucket\x00name → objectRec (live)
	nsVersions = "gcs/versions"      // bucket\x00name\x00gen → objectRec (noncurrent)
	nsUploads  = "gcs/uploads"       // upload id → uploadRec (resumable sessions)
	nsNotifs   = "gcs/notifications" // bucket\x00id → storage.Notification
	nsIAM      = "gcs/iam"           // bucket → policy JSON (fallback when iam is absent)
	nsHMAC     = "gcs/hmac"          // accessId → hmacRec
	nsMPU      = "gcs/mpu"           // XML multipart upload id → mpuRec
)

// sep separates key components; it cannot occur in bucket or object names.
const sep = "\x00"

func objKey(bucket, name string) string { return bucket + sep + name }

func objPrefix(bucket, prefix string) string { return bucket + sep + prefix }

func verKey(bucket, name string, gen int64) string {
	return fmt.Sprintf("%s%s%s%s%020d", bucket, sep, name, sep, gen)
}

func verPrefix(bucket, name string) string { return bucket + sep + name + sep }

// bucketRec is the persisted form of a bucket.
type bucketRec struct {
	Bucket *storage.Bucket `json:"bucket"`
	// Project is the project ID the bucket was created in.
	Project string `json:"project"`
	// PolicySet records whether setIamPolicy was ever called; until then the
	// default legacy-role policy is returned.
	PolicySet bool `json:"policySet,omitempty"`
	// NextNotification is the next notification config ID.
	NextNotification int `json:"nextNotification,omitempty"`
}

// objectRec is the persisted form of one object generation.
type objectRec struct {
	Object *storage.Object `json:"object"`
	// Blob is the data file ID under the blobs directory.
	Blob string `json:"blob"`
	// RetentionStart is when the bucket retention period started counting
	// (creation, or release of an event-based hold).
	RetentionStart string `json:"retentionStart,omitempty"`
}

// Timestamp format GCS uses (RFC 3339, millisecond precision, UTC).
const tsLayout = "2006-01-02T15:04:05.000Z07:00"

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// etagFor encodes generation/metageneration the way GCS does: the base64 of
// a protobuf message with field 1 = generation and field 2 = metageneration.
func etagFor(gen, metagen int64) string {
	var b []byte
	if gen != 0 {
		b = append(b, 0x08)
		b = binary.AppendUvarint(b, uint64(gen))
	}
	b = append(b, 0x10)
	b = binary.AppendUvarint(b, uint64(metagen))
	return base64.StdEncoding.EncodeToString(b)
}

// bucketEtag is the etag of a bucket at a metageneration.
func bucketEtag(metagen int64) string {
	b := binary.AppendUvarint([]byte{0x08}, uint64(metagen))
	return base64.StdEncoding.EncodeToString(b)
}

// bucketResource is the IAM full resource name of a bucket.
func bucketResource(bucket string) string {
	return "//storage.googleapis.com/projects/_/buckets/" + bucket
}

// objectResource is the IAM full resource name of an object.
func objectResource(bucket, name string) string {
	return bucketResource(bucket) + "/objects/" + name
}

func projectResource(project string) string {
	return "//cloudresourcemanager.googleapis.com/projects/" + project
}

// baseURL returns scheme://host[/prefix] for the request, where prefix is
// the gateway mount prefix stripped before the handler saw the request.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host + mountPrefix(r)
}

// mountPrefix recovers a path prefix removed by http.StripPrefix.
func mountPrefix(r *http.Request) string {
	orig := r.RequestURI
	if i := strings.IndexByte(orig, '?'); i >= 0 {
		orig = orig[:i]
	}
	if u, err := url.Parse(orig); err == nil && u.Path != "" {
		orig = u.EscapedPath()
	}
	cur := r.URL.EscapedPath()
	if strings.HasSuffix(orig, cur) {
		return strings.TrimSuffix(orig, cur)
	}
	return ""
}

// renderBucket returns a copy of b ready for output.
func renderBucket(rec *bucketRec, base string) *storage.Bucket {
	b := *rec.Bucket
	b.SelfLink = base + "/storage/v1/b/" + b.Name
	b.Etag = bucketEtag(b.Metageneration)
	return &b
}

// renderObject returns a copy of rec's object with links, etag and computed
// retention fields filled in.
func renderObject(rec *objectRec, bkt *storage.Bucket, base string) *storage.Object {
	o := *rec.Object
	esc := url.PathEscape(o.Name)
	o.SelfLink = base + "/storage/v1/b/" + o.Bucket + "/o/" + esc
	o.MediaLink = fmt.Sprintf("%s/download/storage/v1/b/%s/o/%s?generation=%d&alt=media", base, o.Bucket, esc, o.Generation)
	o.Etag = etagFor(o.Generation, o.Metageneration)
	o.ForceSendFields = []string{"Size"}
	if t, ok := retentionExpiry(rec, bkt); ok {
		o.RetentionExpirationTime = ts(t)
	}
	return &o
}

// retentionExpiry computes when the bucket retention policy releases rec.
func retentionExpiry(rec *objectRec, bkt *storage.Bucket) (time.Time, bool) {
	if bkt == nil || bkt.RetentionPolicy == nil || bkt.RetentionPolicy.RetentionPeriod <= 0 || rec.Object.EventBasedHold {
		return time.Time{}, false
	}
	start := rec.RetentionStart
	if start == "" {
		start = rec.Object.TimeCreated
	}
	return parseTS(start).Add(time.Duration(bkt.RetentionPolicy.RetentionPeriod) * time.Second), true
}
