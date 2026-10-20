package build

import (
	"encoding/json"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// nsBuilds holds every Build, keyed by buildRef.key.
const nsBuilds = "build/builds"

// record is a stored Build.
type record struct {
	// Build is the Build as the API returns it (protojson).
	Build json.RawMessage `json:"build"`
	// Request is the Build as it was submitted, which retry submits again.
	Request json.RawMessage `json:"request"`
	// Op names the Operation that ends with the Build.
	Op string `json:"op"`
	// Substitutions are the values the Build's substitutions resolved to
	// when it was created, for its automapped steps.
	Substitutions map[string]string `json:"substitutions,omitempty"`
}

// stored is a decoded record.
type stored struct {
	build         *cloudbuildpb.Build
	request       *cloudbuildpb.Build
	op            string
	substitutions map[string]string
}

func decode(b []byte) (*stored, error) {
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	st := &stored{build: &cloudbuildpb.Build{}, request: &cloudbuildpb.Build{}, op: rec.Op, substitutions: rec.Substitutions}
	if err := protojson.Unmarshal(rec.Build, st.build); err != nil {
		return nil, err
	}
	if err := protojson.Unmarshal(rec.Request, st.request); err != nil {
		return nil, err
	}
	return st, nil
}

func put(tx store.Tx, st *stored) error {
	b, err := protojson.Marshal(st.build)
	if err != nil {
		return err
	}
	req, err := protojson.Marshal(st.request)
	if err != nil {
		return err
	}
	return store.PutJSON(tx, nsBuilds, refOf(st.build).key(), record{Build: b, Request: req, Op: st.op, Substitutions: st.substitutions})
}

func get(tx store.Tx, r buildRef) (*stored, error) {
	b, ok := tx.Get(nsBuilds, r.key())
	if !ok {
		return nil, apierr.NotFound("Build %q not found.", r.name())
	}
	return decode(b)
}

// scan calls fn with each Build under the key prefix.
func scan(tx store.Tx, prefix string, fn func(*stored)) error {
	var err error
	tx.Scan(nsBuilds, prefix, func(_ string, b []byte) bool {
		var st *stored
		if st, err = decode(b); err != nil {
			return false
		}
		fn(st)
		return true
	})
	return err
}

// refOf returns the buildRef of a stored Build.
func refOf(b *cloudbuildpb.Build) buildRef {
	r, _ := parseBuild("", "", b.GetName())
	return r
}

// terminal reports whether a Build status is final.
func terminal(s cloudbuildpb.Build_Status) bool {
	switch s {
	case cloudbuildpb.Build_STATUS_UNKNOWN, cloudbuildpb.Build_PENDING, cloudbuildpb.Build_QUEUED, cloudbuildpb.Build_WORKING:
		return false
	}
	return true
}
