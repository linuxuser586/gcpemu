package build

import (
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/maskpath"
)

// applyMask copies the fields an update_mask names from src to dst. A
// path is dotted for nested fields, and each field segment is a field's
// proto (snake_case) or JSON (camelCase) name. A path into a map field
// names one key, bare or backtick-quoted, used as written (ADR 0075).
func applyMask(dst, src proto.Message, paths []string) error {
	for _, p := range paths {
		for _, p := range strings.Split(p, ",") {
			if err := copyPath(dst.ProtoReflect(), src.ProtoReflect(), strings.TrimSpace(p)); err != nil {
				return apierr.InvalidArgument("Invalid update_mask path %q.", p)
			}
		}
	}
	return nil
}

func copyPath(dst, src protoreflect.Message, path string) error {
	name, rest, nested := strings.Cut(path, ".")
	fs := dst.Descriptor().Fields()
	fd := fs.ByName(protoreflect.Name(name))
	if fd == nil {
		fd = fs.ByJSONName(name)
	}
	if fd == nil {
		return apierr.InvalidArgument("unknown field %q", name)
	}
	if !nested {
		if src.Has(fd) {
			dst.Set(fd, src.Get(fd))
		} else {
			dst.Clear(fd)
		}
		return nil
	}
	if fd.IsMap() {
		return copyKey(dst, src, fd, path)
	}
	if fd.Kind() != protoreflect.MessageKind || fd.IsList() {
		return apierr.InvalidArgument("field %q has no subfields", name)
	}
	if !src.Has(fd) && !dst.Has(fd) {
		return nil
	}
	return copyPath(dst.Mutable(fd).Message(), src.Get(fd).Message(), rest)
}

// copyKey sets the one key path names in map field fd of dst to its value
// in src, or removes it when src lacks it, and keeps dst's other keys.
func copyKey(dst, src protoreflect.Message, fd protoreflect.FieldDescriptor, path string) error {
	mp, ok := maskpath.Parse(path)
	if !ok || fd.MapKey().Kind() != protoreflect.StringKind {
		return apierr.InvalidArgument("invalid map key path %q", path)
	}
	k := protoreflect.ValueOfString(mp.Key).MapKey()
	if v := src.Get(fd).Map(); v.Has(k) {
		dst.Mutable(fd).Map().Set(k, v.Get(k))
	} else if dst.Has(fd) {
		dst.Mutable(fd).Map().Clear(k)
	}
	return nil
}
