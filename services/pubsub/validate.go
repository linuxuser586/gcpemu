package pubsub

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/maskpath"
)

// GCP limits and defaults (https://cloud.google.com/pubsub/quotas).
const (
	maxPublishMessages   = 1000
	maxPublishBytes      = 10_000_000
	maxAttributes        = 100
	maxAttrKeyBytes      = 256
	maxAttrValueBytes    = 1024
	maxOrderingKeyBytes  = 1024
	maxPullMessages      = 1000
	minAckDeadline       = 10
	maxAckDeadline       = 600
	defaultAckDeadline   = 10
	minRetention         = 10 * time.Minute
	maxRetention         = 31 * 24 * time.Hour
	defaultSubRetention  = 7 * 24 * time.Hour
	defaultExpirationTTL = 31 * 24 * time.Hour
	minExpirationTTL     = 24 * time.Hour
	defaultMinBackoff    = 10 * time.Second
	defaultMaxBackoff    = 600 * time.Second
	maxBackoff           = 600 * time.Second
	minDeliveryAttempts  = 5
	maxDeliveryAttempts  = 100

	defaultMaxDeliveryAttempts = 5
)

var (
	labelKey   = regexp.MustCompile(`^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_-]{0,62}$`)
	labelValue = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$`)
)

func validateLabels(labels map[string]string) error {
	if len(labels) > 64 {
		return apierr.InvalidArgument("Too many labels: %d (maximum 64).", len(labels))
	}
	for k, v := range labels {
		if !labelKey.MatchString(k) {
			return apierr.InvalidArgument("Invalid label key: %q.", k)
		}
		if !labelValue.MatchString(v) {
			return apierr.InvalidArgument("Invalid label value: %q.", v)
		}
	}
	return nil
}

func validateRetention(field string, d *durationpb.Duration) error {
	if d == nil {
		return nil
	}
	if err := d.CheckValid(); err != nil {
		return apierr.InvalidArgument("Invalid %s: %v.", field, err)
	}
	v := d.AsDuration()
	if v < minRetention || v > maxRetention {
		return apierr.InvalidArgument("Invalid %s: must be between 10 minutes and 31 days, got %s.", field, v)
	}
	return nil
}

func tooLarge(field string, got, max int) error {
	return apierr.InvalidArgument("The value for %s is too large. You passed %d in the request, but the maximum value is %d.", field, got, max)
}

// applyMask copies the fields named by paths from src to dst; allowed lists
// the permitted top-level fields. A path is dotted for nested message
// fields, and its field segments may be snake_case or lowerCamel. A path
// into a map field names one key, bare or backtick-quoted, used as written
// (ADR 0075).
func applyMask(dst, src proto.Message, paths []string, allowed map[string]bool, kind string) error {
	if len(paths) == 0 {
		return apierr.InvalidArgument("The update_mask in the Update%sRequest must be set, and must be non-empty.", kind)
	}
	src = proto.Clone(src)
	for _, p := range paths {
		top, _, _ := strings.Cut(p, ".")
		if !allowed[maskpath.SnakeCase(top)] {
			return apierr.InvalidArgument("Invalid update_mask provided in the Update%sRequest: the '%s' field cannot be updated.", kind, p)
		}
		if err := copyPath(dst.ProtoReflect(), src.ProtoReflect(), p); err != nil {
			return apierr.InvalidArgument("Invalid update_mask provided in the Update%sRequest: %s", kind, err.Error())
		}
	}
	return nil
}

// copyPath copies the field path p names from src to dst. A path into a
// map field sets the key if src holds it and removes it otherwise, keeping
// dst's other keys; a whole-field path replaces the whole map.
func copyPath(dst, src protoreflect.Message, p string) error {
	seg, rest, more := strings.Cut(p, ".")
	fd := dst.Descriptor().Fields().ByName(protoreflect.Name(maskpath.SnakeCase(seg)))
	if fd == nil {
		return &maskError{seg}
	}
	if !more {
		if src.Has(fd) {
			dst.Set(fd, src.Get(fd))
		} else {
			dst.Clear(fd)
		}
		return nil
	}
	if fd.IsMap() {
		return copyKey(dst, src, fd, p)
	}
	if fd.Kind() != protoreflect.MessageKind || fd.IsList() {
		return &maskError{p}
	}
	if !src.Has(fd) && !dst.Has(fd) {
		return nil
	}
	return copyPath(dst.Mutable(fd).Message(), src.Get(fd).Message(), rest)
}

// copyKey copies the one key p names in map field fd from src to dst.
func copyKey(dst, src protoreflect.Message, fd protoreflect.FieldDescriptor, p string) error {
	mp, ok := maskpath.Parse(p)
	if !ok || fd.MapKey().Kind() != protoreflect.StringKind {
		return &keyError{p}
	}
	k := protoreflect.ValueOfString(mp.Key).MapKey()
	if sm := src.Get(fd).Map(); sm.Has(k) {
		dst.Mutable(fd).Map().Set(k, sm.Get(k))
	} else if dst.Has(fd) {
		dst.Mutable(fd).Map().Clear(k)
	}
	return nil
}

// errStale reports that a config changed after an update read it; the
// update merges its mask into the new config and tries again.
var errStale = errors.New("pubsub: config changed during update")

type keyError struct{ path string }

func (e *keyError) Error() string {
	return "'" + e.path + "' must name one map key; quote a key that holds dots with backticks"
}

type maskError struct{ path string }

func (e *maskError) Error() string { return "unknown field '" + e.path + "'" }
