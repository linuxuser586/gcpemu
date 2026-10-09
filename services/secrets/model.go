package secrets

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Store namespaces.
const (
	nsSecrets  = "secrets/secrets"  // P/L/S → secretRec
	nsVersions = "secrets/versions" // P/L/S/NNNNNNNNNN → versionRec
	nsIAM      = "secrets/iam"      // resource → protojson Policy (without the iam service)
)

// secretRec is a stored secret.
type secretRec struct {
	Secret json.RawMessage `json:"secret"` // protojson Secret
	// Last is the highest version number added so far.
	Last int64 `json:"last"`
	// Rev increases on every change and is the basis of the etag.
	Rev int64 `json:"rev"`
	// Managed holds the Cloud SQL credentials of a secret under managed
	// rotation.
	Managed *managedCreds `json:"managed,omitempty"`

	pb *secretmanagerpb.Secret
}

type managedCreds struct {
	Instance string `json:"instance"`
	Username string `json:"username"`
}

// versionRec is a stored secret version.
type versionRec struct {
	Version json.RawMessage `json:"version"` // protojson SecretVersion
	Payload []byte          `json:"payload,omitempty"`
	CRC     int64           `json:"crc"`
	Rev     int64           `json:"rev"`

	pb *secretmanagerpb.SecretVersion
}

func versionKey(r secretRef, n int64) string { return fmt.Sprintf("%s/%010d", r.key(), n) }

func notFound(r secretRef) error {
	return apierr.NotFound("Secret [%s] not found or has no versions.", r.name())
}

func versionNotFound(name string) error {
	return apierr.NotFound("Secret Version [%s] not found.", name)
}

// etag derives an etag from a revision, quoted as GCP returns it.
func etag(rev int64) string { return fmt.Sprintf("\"%013x\"", rev) }

// nextRev returns a revision greater than prev, based on the clock.
func nextRev(prev int64, now time.Time) int64 { return max(prev+1, now.UnixMicro()) }

// checkEtag compares a request etag (quoted or not) with the current one.
func checkEtag(req, cur string) error {
	if req == "" {
		return nil
	}
	if strings.Trim(req, `"`) != strings.Trim(cur, `"`) {
		return apierr.FailedPrecondition("The etag provided in the request does not match the current etag.").WithReason(errDom, "ETAG_MISMATCH")
	}
	return nil
}

func getSecret(tx store.Tx, r secretRef) (*secretRec, error) {
	var rec secretRec
	if err := store.GetJSON(tx, nsSecrets, r.key(), &rec); err != nil {
		return nil, notFound(r)
	}
	rec.pb = &secretmanagerpb.Secret{}
	if err := protojson.Unmarshal(rec.Secret, rec.pb); err != nil {
		return nil, apierr.Internal("decode secret: %v", err)
	}
	return &rec, nil
}

// putSecret stores rec with rec.pb, bumping the revision and etag.
func putSecret(tx store.Tx, r secretRef, rec *secretRec, now time.Time) error {
	rec.Rev = nextRev(rec.Rev, now)
	rec.pb.Etag = etag(rec.Rev)
	b, err := protojson.Marshal(rec.pb)
	if err != nil {
		return err
	}
	rec.Secret = b
	return store.PutJSON(tx, nsSecrets, r.key(), rec)
}

func getVersion(tx store.Tx, r secretRef, n int64) (*versionRec, bool) {
	var rec versionRec
	if store.GetJSON(tx, nsVersions, versionKey(r, n), &rec) != nil {
		return nil, false
	}
	rec.pb = &secretmanagerpb.SecretVersion{}
	if protojson.Unmarshal(rec.Version, rec.pb) != nil {
		return nil, false
	}
	return &rec, true
}

func putVersion(tx store.Tx, r secretRef, n int64, rec *versionRec, now time.Time) error {
	rec.Rev = nextRev(rec.Rev, now)
	rec.pb.Etag = etag(rec.Rev)
	b, err := protojson.Marshal(rec.pb)
	if err != nil {
		return err
	}
	rec.Version = b
	return store.PutJSON(tx, nsVersions, versionKey(r, n), rec)
}

// listVersions returns a secret's versions, newest first.
func listVersions(tx store.Tx, r secretRef) []*versionRec {
	var out []*versionRec
	tx.Scan(nsVersions, r.key()+"/", func(_ string, b []byte) bool {
		var rec versionRec
		if json.Unmarshal(b, &rec) == nil {
			rec.pb = &secretmanagerpb.SecretVersion{}
			if protojson.Unmarshal(rec.Version, rec.pb) == nil {
				out = append(out, &rec)
			}
		}
		return true
	})
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// deleteSecretData removes a secret and its versions.
func deleteSecretData(tx store.Tx, r secretRef) error {
	var keys []string
	tx.Scan(nsVersions, r.key()+"/", func(k string, _ []byte) bool { keys = append(keys, k); return true })
	for _, k := range keys {
		if err := tx.Delete(nsVersions, k); err != nil {
			return err
		}
	}
	return tx.Delete(nsSecrets, r.key())
}

// resolveVersion maps a version ID, "latest" or an alias to a number.
// latest is the most recently created version.
func resolveVersion(tx store.Tx, r secretRef, rec *secretRec, id string) (int64, bool) {
	if id == "latest" {
		vs := listVersions(tx, r)
		if len(vs) == 0 {
			return 0, false
		}
		return versionNumber(vs[0].pb.GetName()), true
	}
	if n, ok := rec.pb.GetVersionAliases()[id]; ok {
		return n, true
	}
	var n int64
	if _, err := fmt.Sscanf(id, "%d", &n); err != nil || fmt.Sprint(n) != id || n <= 0 {
		return 0, false
	}
	return n, true
}

func versionNumber(name string) int64 {
	var n int64
	_, _ = fmt.Sscanf(name[strings.LastIndexByte(name, '/')+1:], "%d", &n)
	return n
}

// clone returns a deep copy of a message.
func clone[T proto.Message](m T) T { return proto.Clone(m).(T) }
