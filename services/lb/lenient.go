package lb

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
)

// unmarshalLenient decodes compute JSON into a generated struct. The
// generated types encode 64-bit integers as JSON strings (",string"), but
// the API also accepts plain numbers, which clients such as the OpenTofu
// provider send; such numbers are converted before decoding.
func unmarshalLenient(b []byte, v any) error {
	err := json.Unmarshal(b, v)
	if err == nil {
		return nil
	}
	var raw any
	if json.Unmarshal(b, &raw) != nil {
		return err
	}
	fixed := normalizeJSON(raw, reflect.TypeOf(v))
	nb, merr := json.Marshal(fixed)
	if merr != nil {
		return err
	}
	return json.Unmarshal(nb, v)
}

// normalizeJSON rewrites numbers into strings where t expects ",string".
func normalizeJSON(val any, t reflect.Type) any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch v := val.(type) {
	case map[string]any:
		switch t.Kind() {
		case reflect.Struct:
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				tag := f.Tag.Get("json")
				name, opts, _ := strings.Cut(tag, ",")
				if name == "" || name == "-" {
					continue
				}
				fv, ok := v[name]
				if !ok {
					continue
				}
				if strings.Contains(opts, "string") {
					if n, ok := fv.(float64); ok {
						v[name] = strconv.FormatFloat(n, 'f', -1, 64)
						continue
					}
				}
				v[name] = normalizeJSON(fv, f.Type)
			}
		case reflect.Map:
			for k, x := range v {
				v[k] = normalizeJSON(x, t.Elem())
			}
		}
		return v
	case []any:
		if t.Kind() == reflect.Slice {
			for i, x := range v {
				v[i] = normalizeJSON(x, t.Elem())
			}
		}
		return v
	}
	return val
}

// normalizeBody converts plain numbers in a JSON body to the string form
// the type expects (see unmarshalLenient).
func normalizeBody(b []byte, v any) []byte {
	var raw any
	if json.Unmarshal(b, &raw) != nil {
		return b
	}
	nb, err := json.Marshal(normalizeJSON(raw, reflect.TypeOf(v)))
	if err != nil {
		return b
	}
	return nb
}
