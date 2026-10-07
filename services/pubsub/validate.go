package pubsub

import (
	"regexp"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
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

// applyMask copies the fields named by paths (snake_case, dotted for nested
// fields) from src to dst; allowed lists the permitted top-level fields.
func applyMask(dst, src proto.Message, paths []string, allowed map[string]bool, kind string) error {
	if len(paths) == 0 {
		return apierr.InvalidArgument("The update_mask in the Update%sRequest must be set, and must be non-empty.", kind)
	}
	src = proto.Clone(src)
	for _, p := range paths {
		top, _, _ := strings.Cut(p, ".")
		if !allowed[top] {
			return apierr.InvalidArgument("Invalid update_mask provided in the Update%sRequest: the '%s' field cannot be updated.", kind, p)
		}
		if err := copyPath(dst.ProtoReflect(), src.ProtoReflect(), strings.Split(p, ".")); err != nil {
			return apierr.InvalidArgument("Invalid update_mask provided in the Update%sRequest: %s", kind, err.Error())
		}
	}
	return nil
}

func copyPath(dst, src protoreflect.Message, path []string) error {
	fd := dst.Descriptor().Fields().ByName(protoreflect.Name(path[0]))
	if fd == nil {
		return &maskError{path[0]}
	}
	if len(path) == 1 {
		if src.Has(fd) {
			dst.Set(fd, src.Get(fd))
		} else {
			dst.Clear(fd)
		}
		return nil
	}
	if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
		return &maskError{strings.Join(path, ".")}
	}
	if !src.Has(fd) {
		if dst.Has(fd) {
			return copyPath(dst.Mutable(fd).Message(), src.Get(fd).Message(), path[1:])
		}
		return nil
	}
	return copyPath(dst.Mutable(fd).Message(), src.Get(fd).Message(), path[1:])
}

type maskError struct{ path string }

func (e *maskError) Error() string { return "unknown field '" + e.path + "'" }
