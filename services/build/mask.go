package build

import (
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// applyMask copies the fields an update_mask names from src to dst. A
// path is dotted for nested fields, and each segment is a field's proto
// (snake_case) or JSON (camelCase) name.
func applyMask(dst, src proto.Message, paths []string) error {
	for _, p := range paths {
		for _, p := range strings.Split(p, ",") {
			if err := copyPath(dst.ProtoReflect(), src.ProtoReflect(), strings.Split(strings.TrimSpace(p), ".")); err != nil {
				return apierr.InvalidArgument("Invalid update_mask path %q.", p)
			}
		}
	}
	return nil
}

func copyPath(dst, src protoreflect.Message, path []string) error {
	fs := dst.Descriptor().Fields()
	fd := fs.ByName(protoreflect.Name(path[0]))
	if fd == nil {
		fd = fs.ByJSONName(path[0])
	}
	if fd == nil {
		return apierr.InvalidArgument("unknown field %q", path[0])
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
		return apierr.InvalidArgument("field %q has no subfields", path[0])
	}
	if !src.Has(fd) && !dst.Has(fd) {
		return nil
	}
	return copyPath(dst.Mutable(fd).Message(), src.Get(fd).Message(), path[1:])
}
