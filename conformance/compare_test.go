//go:build conformance

package conformance

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Mismatch is one field where the emulator's response differs from the
// Recording.
type Mismatch struct {
	Path   string // e.g. exchanges[2].response.body.items[0].name
	Detail string // e.g. `want "a", got "b"`
}

func (m Mismatch) String() string { return m.Path + ": " + m.Detail }

// mismatchText is ms one per line.
func mismatchText(ms []Mismatch) string {
	var b strings.Builder
	for _, m := range ms {
		b.WriteString(m.String() + "\n")
	}
	return b.String()
}

// compare compares got with the Recording want, both normalised, Exchange
// by Exchange, and stops at the first that differs. If its request differs
// (method or path), or one side has no Exchange there, want is a Stale
// Recording and the error names the Exchange; later differences follow
// from the test changing. Otherwise the Mismatches are that Exchange's
// response's; later Exchanges' differences follow from it. Request bodies
// are not compared. An error response is compared by its essence
// (errorEssence); messages adds its message.
func compare(want, got Recording, messages bool) ([]Mismatch, error) {
	for i := range max(len(want.Exchanges), len(got.Exchanges)) {
		if i >= len(got.Exchanges) {
			w := want.Exchanges[i].Request
			return nil, fmt.Errorf("exchanges[%d]: the Recording has %s %s, the test made no request", i, w.Method, w.Path)
		}
		if i >= len(want.Exchanges) {
			g := got.Exchanges[i].Request
			return nil, fmt.Errorf("exchanges[%d]: the Recording ends, the test made %s %s", i, g.Method, g.Path)
		}
		w, g := want.Exchanges[i].Request, got.Exchanges[i].Request
		if w.Method != g.Method || w.Path != g.Path {
			return nil, fmt.Errorf("exchanges[%d]: the Recording has %s %s, the test made %s %s", i, w.Method, w.Path, g.Method, g.Path)
		}
		if ms := compareResponse(fmt.Sprintf("exchanges[%d].response", i), want.Exchanges[i].Response, got.Exchanges[i].Response, messages); len(ms) > 0 {
			return ms, nil
		}
	}
	return nil, nil
}

// compareResponse returns the Mismatches of got from want at path.
func compareResponse(path string, want, got Response, messages bool) []Mismatch {
	var ms []Mismatch
	if want.Status != got.Status {
		ms = append(ms, Mismatch{path + ".status", fmt.Sprintf("want %d, got %d", want.Status, got.Status)})
	}
	if want.Code != got.Code {
		ms = append(ms, Mismatch{path + ".code", fmt.Sprintf("want %s, got %s", want.Code, got.Code)})
	}
	wb, gb := want.Body, got.Body
	if want.failed() || got.failed() {
		wb, gb = errorEssence(wb, messages), errorEssence(gb, messages)
	}
	return compareValue(ms, path+".body", wb, gb)
}

// errorEssence is what is compared of an error body: the reasons (a REST
// error's errors[].reason and any ErrorInfo detail's reason) and the
// fields of BadRequest field violations, and with messages the message.
// body is a REST {"error": {...}} or a gRPC google.rpc.Status.
func errorEssence(body any, messages bool) any {
	e, _ := body.(map[string]any)
	if inner, ok := e["error"].(map[string]any); ok {
		e = inner
	}
	reasons, fields := []any{}, []any{}
	for _, x := range list(e["errors"]) {
		if r, ok := x["reason"]; ok {
			reasons = append(reasons, r)
		}
	}
	for _, d := range list(e["details"]) {
		if r, ok := d["reason"]; ok {
			reasons = append(reasons, r)
		}
		for _, v := range list(d["fieldViolations"]) {
			fields = append(fields, v["field"])
		}
	}
	out := map[string]any{"reasons": reasons, "fieldViolations": fields}
	if messages {
		out["message"] = e["message"]
	}
	return out
}

// list is v's elements that are objects.
func list(v any) []map[string]any {
	var out []map[string]any
	a, _ := v.([]any)
	for _, x := range a {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// compareValue appends to ms the Mismatches of got from want at path.
func compareValue(ms []Mismatch, path string, want, got any) []Mismatch {
	wm, wok := want.(map[string]any)
	gm, gok := got.(map[string]any)
	if wok && gok {
		union := maps.Clone(wm)
		maps.Copy(union, gm)
		keys := slices.Sorted(maps.Keys(union))
		for _, k := range keys {
			w, inW := wm[k]
			g, inG := gm[k]
			p := path + "." + k
			switch {
			case !inG:
				ms = append(ms, Mismatch{p, "missing, want " + show(w)})
			case !inW:
				ms = append(ms, Mismatch{p, "unexpected field, got " + show(g)})
			default:
				ms = compareValue(ms, p, w, g)
			}
		}
		return ms
	}
	wa, wok := want.([]any)
	ga, gok := got.([]any)
	if wok && gok {
		for i := range max(len(wa), len(ga)) {
			p := fmt.Sprintf("%s[%d]", path, i)
			switch {
			case i >= len(ga):
				ms = append(ms, Mismatch{p, "missing, want " + show(wa[i])})
			case i >= len(wa):
				ms = append(ms, Mismatch{p, "unexpected element, got " + show(ga[i])})
			default:
				ms = compareValue(ms, p, wa[i], ga[i])
			}
		}
		return ms
	}
	if show(want) != show(got) {
		ms = append(ms, Mismatch{path, "want " + show(want) + ", got " + show(got)})
	}
	return ms
}

// show is v as compact JSON, with placeholders' angle brackets as is.
func show(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
