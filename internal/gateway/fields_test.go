package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestParseFields(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want fieldMask
	}{
		{"name", fieldMask{"name": nil}},
		{"items(name,id),nextPageToken", fieldMask{"items": {"name": nil, "id": nil}, "nextPageToken": nil}},
		{"a/b/c,a/d", fieldMask{"a": {"b": {"c": nil}, "d": nil}}},
		{"a,a/b", fieldMask{"a": nil}},
		{"a/b,a", fieldMask{"a": nil}},
		{"items(metadata/labels, name)", fieldMask{"items": {"metadata": {"labels": nil}, "name": nil}}},
		{"items/*", fieldMask{"items": {"*": nil}}},
	} {
		got, err := parseFields(tc.in)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseFields(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "a,", "a(b", "a()", "a)b", "/a"} {
		if _, err := parseFields(bad); err == nil {
			t.Errorf("parseFields(%q) accepted", bad)
		}
	}
}

func TestApplyFields(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(`{"kind":"k","items":[{"name":"a","id":"1","meta":{"x":1,"y":2}},{"name":"b"}],"nextPageToken":"t"}`), &v)
	m, _ := parseFields("items(name,meta/y),nextPageToken")
	got, _ := json.Marshal(m.apply(v))
	if want := `{"items":[{"meta":{"y":2},"name":"a"},{"name":"b"}],"nextPageToken":"t"}`; string(got) != want {
		t.Errorf("apply = %s, want %s", got, want)
	}
	m, _ = parseFields("*")
	if got, _ := json.Marshal(m.apply(v)); !strings.Contains(string(got), `"kind":"k"`) {
		t.Errorf("* = %s", got)
	}
}

// TestFormatWriter: JSON responses are filtered and re-indented, errors
// keep their envelope, and other content passes through unbuffered.
func TestFormatWriter(t *testing.T) {
	serve := func(target string, h http.HandlerFunc) *http.Response {
		r := httptest.NewRequest("GET", target, nil)
		w := httptest.NewRecorder()
		f, err := formatFor(r)
		if err != nil {
			t.Fatal(err)
		}
		fw := newFormatWriter(w, f)
		h(fw, r)
		fw.finish()
		return w.Result()
	}
	body := func(resp *http.Response) string { b, _ := io.ReadAll(resp.Body); return string(b) }
	jsonH := func(code int, s string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=UTF-8")
			w.WriteHeader(code)
			_, _ = io.WriteString(w, s)
		}
	}
	if got := body(serve("/x?fields=name&prettyPrint=false", jsonH(200, `{"name":"a", "id":"1"}`))); got != `{"name":"a"}`+"\n" {
		t.Errorf("filtered compact = %q", got)
	}
	if got := body(serve("/x?fields=name", jsonH(200, `{"name":"a","id":"1"}`))); got != "{\n  \"name\": \"a\"\n}\n" {
		t.Errorf("filtered pretty = %q", got)
	}
	if got := body(serve("/x?prettyPrint=false", jsonH(200, "{\n  \"id\": 12345678901234567890\n}"))); got != `{"id":12345678901234567890}`+"\n" {
		t.Errorf("compact keeps numbers = %q", got)
	}
	resp := serve("/x?fields=name", jsonH(404, `{"error":{"code":404}}`))
	if got := body(resp); resp.StatusCode != 404 || !strings.Contains(got, `"error"`) {
		t.Errorf("error = %d %q", resp.StatusCode, got)
	}
	resp = serve("/x?fields=name", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, `{"name":"a","id":"1"}`)
	})
	if got := body(resp); got != `{"name":"a","id":"1"}` || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("media = %q %v", got, resp.Header)
	}
	if _, err := formatFor(httptest.NewRequest("GET", "/x?prettyPrint=maybe", nil)); err == nil {
		t.Error("bad prettyPrint accepted")
	}
	if f, _ := formatFor(httptest.NewRequest("GET", "/x?alt=json", nil)); f != nil {
		t.Error("format without fields or prettyPrint")
	}
}
