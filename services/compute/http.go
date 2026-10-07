package compute

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// reply writes v, or err as a GCP error envelope.
func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// readBody returns the raw request body (required unless optional).
func readBody(r *http.Request, optional bool) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, apierr.InvalidArgument("Failed to read request body: %v", err).WithLegacy("parseError")
	}
	if len(b) == 0 && !optional {
		return nil, apierr.InvalidArgument("Required request body is missing.").WithLegacy("required")
	}
	return b, nil
}

// decode reads a JSON request body into v.
func decode(r *http.Request, v any) ([]byte, error) {
	b, err := readBody(r, false)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return nil, apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError")
	}
	return b, nil
}

// listItem pairs a list element with its page key.
type listItem struct {
	key string
	v   any
}

// toMap converts a resource to its JSON object form (for filters and patches).
func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// page applies filter, maxResults and pageToken (FR-CORE-024) to items.
// Page tokens are the opaque encoding of the last returned key.
func page(r *http.Request, items []listItem) ([]any, string, error) {
	q := r.URL.Query()
	f, err := parseFilter(q.Get("filter"))
	if err != nil {
		return nil, "", err
	}
	max := 500
	if v := q.Get("maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, "", errInvalidField("maxResults", v, "")
		}
		if n > 0 && n < 500 {
			max = n
		}
	}
	after := ""
	if tok := q.Get("pageToken"); tok != "" {
		b, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			return nil, "", errInvalidField("pageToken", tok, "")
		}
		after = string(b)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].key < items[j].key })
	out := []any{}
	next := ""
	for _, it := range items {
		if after != "" && it.key <= after {
			continue
		}
		if f != nil && !f.match(toMap(it.v)) {
			continue
		}
		if len(out) == max {
			next = base64.RawURLEncoding.EncodeToString([]byte(lastKeyOf(items, out, after, f)))
			break
		}
		out = append(out, it.v)
	}
	return out, next, nil
}

// lastKeyOf finds the key of the last element placed on the page.
func lastKeyOf(items []listItem, out []any, after string, f filterExpr) string {
	n := 0
	for _, it := range items {
		if after != "" && it.key <= after {
			continue
		}
		if f != nil && !f.match(toMap(it.v)) {
			continue
		}
		n++
		if n == len(out) {
			return it.key
		}
	}
	return ""
}

// listResponse renders a compute list ("compute#networkList").
func listResponse(kind, id string, items []any, next string) map[string]any {
	out := map[string]any{"kind": kind, "id": id, "selfLink": link(id)}
	if len(items) > 0 {
		out["items"] = items
	}
	if next != "" {
		out["nextPageToken"] = next
	}
	return out
}

// writeList pages items and writes a compute list response.
func writeList(w http.ResponseWriter, r *http.Request, kind, id string, items []listItem) {
	pg, next, err := page(r, items)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse(kind, id, pg, next))
}

// writeAggregated renders an aggregatedList: items keyed by scope
// ("regions/us-central1") holding {field: [...]}.
func writeAggregated(w http.ResponseWriter, r *http.Request, kind, id, field string, scoped map[string][]listItem) {
	var all []listItem
	for sc, items := range scoped {
		for _, it := range items {
			all = append(all, listItem{key: sc + "\x00" + it.key, v: scopedItem{scope: sc, v: it.v}})
		}
	}
	// Filter against the inner value.
	q := r.URL.Query()
	f, err := parseFilter(q.Get("filter"))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if f != nil {
		kept := all[:0]
		for _, it := range all {
			if f.match(toMap(it.v.(scopedItem).v)) {
				kept = append(kept, it)
			}
		}
		all = kept
	}
	r2 := r.Clone(r.Context())
	qq := r2.URL.Query()
	qq.Del("filter")
	r2.URL.RawQuery = qq.Encode()
	pg, next, err := page(r2, all)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	items := map[string]map[string][]any{}
	for _, v := range pg {
		si := v.(scopedItem)
		if items[si.scope] == nil {
			items[si.scope] = map[string][]any{}
		}
		items[si.scope][field] = append(items[si.scope][field], si.v)
	}
	out := map[string]any{"kind": kind, "id": id, "selfLink": link(id), "items": items}
	if next != "" {
		out["nextPageToken"] = next
	}
	writeJSON(w, http.StatusOK, out)
}

type scopedItem struct {
	scope string
	v     any
}

// MarshalJSON lets filters see the wrapped value.
func (s scopedItem) MarshalJSON() ([]byte, error) { return json.Marshal(s.v) }

// --- store helpers ---

// get loads a resource; ok is false when absent.
func get[T any](tx store.Tx, ns, key string) (*T, bool) {
	var v T
	if err := store.GetJSON(tx, ns, key, &v); err != nil {
		return nil, false
	}
	return &v, true
}

// list loads every resource under prefix.
func list[T any](tx store.Tx, ns, prefix string) []*T {
	var out []*T
	tx.Scan(ns, prefix, func(_ string, b []byte) bool {
		var v T
		if json.Unmarshal(b, &v) == nil {
			out = append(out, &v)
		}
		return true
	})
	return out
}

// mergePatch applies a JSON merge patch (RFC 7386: objects merge
// recursively, arrays and scalars replace, null deletes) to the JSON form
// of cur and decodes the result into out. Fields in keep retain cur's value.
func mergePatch(cur any, patch []byte, out any, keep ...string) error {
	base := toMap(cur)
	var p map[string]any
	if err := json.Unmarshal(patch, &p); err != nil {
		return apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError")
	}
	saved := map[string]any{}
	for _, k := range keep {
		if v, ok := base[k]; ok {
			saved[k] = v
		}
	}
	merged := mergeValue(base, p).(map[string]any)
	for _, k := range keep {
		if v, ok := saved[k]; ok {
			merged[k] = v
		} else {
			delete(merged, k)
		}
	}
	b, _ := json.Marshal(merged)
	return json.Unmarshal(b, out)
}

func mergeValue(base, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	bm, ok := base.(map[string]any)
	if !ok {
		bm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(bm, k)
			continue
		}
		bm[k] = mergeValue(bm[k], v)
	}
	return bm
}
