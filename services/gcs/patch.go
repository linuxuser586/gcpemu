package gcs

import (
	"encoding/json"
)

// applyPatch applies a PATCH (JSON merge patch, RFC 7396: objects merge,
// null deletes, everything else replaces) or, when replace is true, an
// update (PUT: every mutable field takes the body's value or is cleared) to
// cur, restricted to the mutable fields. cur and the result are JSON-encodable
// resources (storage.Bucket / storage.Object).
func applyPatch(cur any, body map[string]json.RawMessage, mutable map[string]bool, replace bool, out any) error {
	b, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	if replace {
		for f := range mutable {
			if _, ok := body[f]; !ok {
				delete(doc, f)
			}
		}
	}
	for k, raw := range body {
		if !mutable[k] {
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return errInvalid("Invalid value for field '%s'.", k)
		}
		if replace {
			if v == nil {
				delete(doc, k)
			} else {
				doc[k] = v
			}
			continue
		}
		doc[k] = mergePatch(doc[k], v)
		if doc[k] == nil {
			delete(doc, k)
		}
	}
	nb, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(nb, out); err != nil {
		return errInvalid("Invalid value: %v", err)
	}
	return nil
}

// mergePatch implements RFC 7396 for one value.
func mergePatch(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
			continue
		}
		tm[k] = mergePatch(tm[k], v)
	}
	return tm
}
