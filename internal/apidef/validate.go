package apidef

import (
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// CheckProto applies the field behaviours (AIP-203) of a gRPC method to
// its request, before the service sees it:
//
//   - OUTPUT_ONLY fields are cleared, as the server must ignore them;
//   - REQUIRED fields of the request must be set (see observable), and so must the REQUIRED
//     fields of every message set within it, except in a request carrying
//     an update mask (partial resources are expected there) and except a
//     nested resource's name (AIP-133: create takes the ID separately);
//   - an update mask may not name an IMMUTABLE field.
//
// Methods without a generated definition pass unchecked.
func CheckProto(fullMethod string, req proto.Message) error {
	m := MethodByRPC(fullMethod)
	if m == nil || req == nil {
		return nil
	}
	pr := req.ProtoReflect()
	if string(pr.Descriptor().FullName()) != m.Input {
		return nil
	}
	bound := map[string]bool{}
	for _, b := range m.HTTP {
		for _, v := range templateVars(b.Path) {
			bound[v] = true
		}
	}
	clearOutputOnly(pr, "", bound)
	mask, resource := updateMask(pr)
	if err := checkRequired(pr, "", len(mask) == 0, true); err != nil {
		return err
	}
	if resource != nil {
		for _, p := range mask {
			if err := checkImmutable(resource.Message(), p); err != nil {
				return err
			}
		}
	}
	return nil
}

// templateVars returns the field paths bound by a path template's
// variables ("/v1/{topic.name=projects/*/topics/*}" → topic.name).
func templateVars(tmpl string) []string {
	var out []string
	for {
		i := strings.IndexByte(tmpl, '{')
		if i < 0 {
			return out
		}
		tmpl = tmpl[i+1:]
		j := strings.IndexAny(tmpl, "=}")
		if j < 0 {
			return out
		}
		out = append(out, tmpl[:j])
	}
}

func join(prefix string, fd protoreflect.FieldDescriptor) string {
	if prefix == "" {
		return string(fd.Name())
	}
	return prefix + "." + string(fd.Name())
}

func clearOutputOnly(m protoreflect.Message, prefix string, bound map[string]bool) {
	bs := FieldBehaviors(string(m.Descriptor().FullName()))
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		path := join(prefix, fd)
		b := bs[string(fd.Name())]
		if b&OutputOnly != 0 && b&(Identifier|Required) == 0 && !bound[path] {
			m.Clear(fd)
			return true
		}
		forEachMessage(fd, v, func(sub protoreflect.Message, elem string) {
			clearOutputOnly(sub, path+elem, bound)
		})
		return true
	})
}

// forEachMessage calls fn for each message held by a field value: the value
// itself, each list element or each map value.
func forEachMessage(fd protoreflect.FieldDescriptor, v protoreflect.Value, fn func(m protoreflect.Message, elem string)) {
	switch {
	case fd.IsMap():
		if fd.MapValue().Message() == nil {
			return
		}
		v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
			fn(mv.Message(), "["+k.String()+"]")
			return true
		})
	case fd.IsList():
		if fd.Message() == nil {
			return
		}
		l := v.List()
		for i := 0; i < l.Len(); i++ {
			fn(l.Get(i).Message(), "")
		}
	case fd.Message() != nil:
		fn(v.Message(), "")
	}
}

func checkRequired(m protoreflect.Message, prefix string, nested, top bool) error {
	bs := FieldBehaviors(string(m.Descriptor().FullName()))
	fs := m.Descriptor().Fields()
	for i := 0; i < fs.Len(); i++ {
		fd := fs.Get(i)
		if bs[string(fd.Name())]&Required == 0 || m.Has(fd) || !observable(fd) {
			continue
		}
		if !top && fd.Name() == "name" {
			continue
		}
		return apierr.InvalidArgument("Missing required field: %s.", join(prefix, fd))
	}
	if !nested {
		return nil
	}
	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		forEachMessage(fd, v, func(sub protoreflect.Message, elem string) {
			if err == nil {
				err = checkRequired(sub, join(prefix, fd)+elem, true, false)
			}
		})
		return err == nil
	})
	return err
}

// observable reports whether an unset field can be told from one set to
// its zero value: a proto3 number or bool without presence (a nack's
// ack_deadline_seconds of 0) can't, so REQUIRED is not enforced on it.
// Empty strings, bytes, lists and maps and zero (UNSPECIFIED) enums count
// as missing.
func observable(fd protoreflect.FieldDescriptor) bool {
	if fd.HasPresence() || fd.IsList() || fd.IsMap() {
		return true
	}
	switch fd.Kind() {
	case protoreflect.StringKind, protoreflect.BytesKind, protoreflect.EnumKind:
		return true
	}
	return false
}

// updateMask returns the paths of a request's google.protobuf.FieldMask
// field and the resource it applies to (the request's only other message
// field that has field behaviours).
func updateMask(m protoreflect.Message) ([]string, *protoreflect.Value) {
	var paths []string
	var resource *protoreflect.Value
	fs := m.Descriptor().Fields()
	for i := 0; i < fs.Len(); i++ {
		fd := fs.Get(i)
		if fd.Message() == nil || fd.IsList() || fd.IsMap() {
			continue
		}
		if fd.Message().FullName() == "google.protobuf.FieldMask" {
			if m.Has(fd) {
				pf := fd.Message().Fields().ByName("paths")
				l := m.Get(fd).Message().Get(pf).List()
				for j := 0; j < l.Len(); j++ {
					paths = append(paths, l.Get(j).String())
				}
			}
			continue
		}
		if FieldBehaviors(string(fd.Message().FullName())) != nil && m.Has(fd) {
			if resource != nil {
				return paths, nil // ambiguous
			}
			v := m.Get(fd)
			resource = &v
		}
	}
	return paths, resource
}

// checkImmutable rejects a mask path that names, or descends through, an
// IMMUTABLE field.
func checkImmutable(m protoreflect.Message, path string) error {
	md := m.Descriptor()
	for _, seg := range strings.Split(path, ".") {
		fd := md.Fields().ByName(protoreflect.Name(seg))
		if fd == nil {
			fd = md.Fields().ByJSONName(seg)
		}
		if fd == nil {
			return nil // unknown paths are the service's to report
		}
		if FieldBehaviors(string(md.FullName()))[string(fd.Name())]&Immutable != 0 {
			return apierr.InvalidArgument("Invalid update_mask: field %s is immutable.", path)
		}
		if fd.Message() == nil || fd.IsList() || fd.IsMap() {
			return nil
		}
		md = fd.Message()
	}
	return nil
}
