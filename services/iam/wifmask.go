package iam

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/maskpath"
)

// parseWIFMask splits a pool or provider updateMask into whole lowerCamel
// fields and the map keys named per field (ADR 0075). Only the field
// segment is converted from snake_case; a key is used as written. A path
// into a nested message field (oidc.issuer_uri) names the whole field, as
// before. v is the resource the mask applies to, whose field types say
// which fields are maps.
func parseWIFMask(mask string, v any) (fields []string, keys map[string][]string, err error) {
	keys = map[string][]string{}
	for _, p := range strings.Split(mask, ",") {
		p = strings.TrimSpace(p)
		mp, ok := maskpath.Parse(p)
		field := maskpath.LowerCamel(mp.Field)
		kind := jsonFieldKind(v, field)
		if isMessage := kind == reflect.Pointer; isMessage {
			fields = append(fields, field)
			continue
		}
		if !ok || mp.HasKey {
			if !ok || kind != reflect.Map {
				return nil, nil, apierr.InvalidArgument("Invalid updateMask path %q: only a map field may name a key, and a key is one segment (quote it with backticks if it holds dots).", p)
			}
			keys[field] = append(keys[field], mp.Key)
			continue
		}
		fields = append(fields, field)
	}
	return fields, keys, nil
}

// jsonFieldKind returns the kind of the field of the struct v points to
// whose JSON name is name, or reflect.Invalid if it has none.
func jsonFieldKind(v any, name string) reflect.Kind {
	t := reflect.TypeOf(v).Elem()
	for i := range t.NumField() {
		f := t.Field(i)
		if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag == name {
			return f.Type.Kind()
		}
	}
	return reflect.Invalid
}

// applyWIFMask applies patch to cur: each whole field is copied from
// patch, or removed when patch lacks it, and each named map key is set to
// its value in patch's map, or removed when that map lacks it, keeping the
// map's other keys. A whole-field path wins over key paths of its field.
func applyWIFMask(cur, patch map[string]json.RawMessage, fields []string, keys map[string][]string) error {
	for _, f := range fields {
		if val, ok := patch[f]; ok {
			cur[f] = val
		} else {
			delete(cur, f)
		}
	}
	for f, ks := range keys {
		if slices.Contains(fields, f) {
			continue
		}
		var dst, src map[string]json.RawMessage
		if err := json.Unmarshal(orNull(cur[f]), &dst); err != nil {
			return err
		}
		if err := json.Unmarshal(orNull(patch[f]), &src); err != nil {
			return apierr.InvalidArgument("Invalid %s: %v", f, err)
		}
		if dst == nil {
			dst = map[string]json.RawMessage{}
		}
		for _, k := range ks {
			if val, ok := src[k]; ok {
				dst[k] = val
			} else {
				delete(dst, k)
			}
		}
		if len(dst) == 0 {
			delete(cur, f)
			continue
		}
		cur[f], _ = json.Marshal(dst)
	}
	return nil
}

// orNull returns b, or JSON null when b is empty.
func orNull(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}
