//go:build conformance

package conformance

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ids are the run identifiers the fixtures use.
var ids = runIDs{project: "my-proj", number: "123456789012", suffix: "k2x9ab"}

// body decodes a JSON fixture as the capture does.
func body(t *testing.T, s string) any {
	t.Helper()
	v, err := decodeJSON([]byte(s))
	if err != nil {
		t.Fatalf("fixture %s: %v", s, err)
	}
	return v
}

// ok is a 200 exchange answering a GET of path with a JSON body.
func ok(t *testing.T, path, resp string) Exchange {
	return Exchange{Request: Request{Method: "GET", Path: path}, Response: Response{Status: 200, Body: body(t, resp)}}
}

// normaliseAll normalises raw as one test's exchanges, failing on a bad
// option.
func normaliseAll(t *testing.T, raw []Exchange, opts ...Option) Recording {
	t.Helper()
	n, err := newNormaliser(ids, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return n.recording(raw)
}

// encode is r as written to a Recording file.
func encode(t *testing.T, r Recording) string {
	t.Helper()
	b, err := r.marshal()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPlaceholdersStayConsistentWithinATest(t *testing.T) {
	got := normaliseAll(t, []Exchange{
		ok(t, "/b", `{"id": "8812", "generation": "1700000000123456", "etag": "CAE="}`),
		ok(t, "/b/x", `{"id": "8812", "generation": "1700000000999999", "etag": "CAE="}`),
	})
	want := `{
  "exchanges": [
    {
      "request": {
        "method": "GET",
        "path": "/b"
      },
      "response": {
        "status": 200,
        "body": {
          "etag": "<etag:1>",
          "generation": "<generation:1>",
          "id": "<id:1>"
        }
      }
    },
    {
      "request": {
        "method": "GET",
        "path": "/b/x"
      },
      "response": {
        "status": 200,
        "body": {
          "etag": "<etag:1>",
          "generation": "<generation:2>",
          "id": "<id:1>"
        }
      }
    }
  ]
}
`
	if s := encode(t, got); s != want {
		t.Errorf("got\n%s\nwant\n%s", s, want)
	}
}

func TestRunIdentifiersAndHostsArePlaceholders(t *testing.T) {
	got := normaliseAll(t, []Exchange{
		ok(t, "/b?project=my-proj", `{
			"name": "gcpemu-k2x9ab-b",
			"projectNumber": "123456789012",
			"selfLink": "https://storage.googleapis.com/storage/v1/b/gcpemu-k2x9ab-b",
			"topic": "projects/my-proj/topics/t"}`),
	})
	r := got.Exchanges[0]
	if r.Request.Path != "/b?project=<project>" {
		t.Errorf("path = %q", r.Request.Path)
	}
	want := map[string]any{
		"name":          "gcpemu-<suffix>-b",
		"projectNumber": "<project-number>",
		"selfLink":      "<host>/storage/v1/b/gcpemu-<suffix>-b",
		"topic":         "projects/<project>/topics/t",
	}
	assertJSON(t, r.Response.Body, want)
}

func TestTimestampsAndEtagsKeepTheirShape(t *testing.T) {
	got := normaliseAll(t, []Exchange{
		ok(t, "/s", `{
			"createTime": "2024-05-01T10:11:12.123456Z",
			"updateTime": "2024-05-01T25:11:12Z",
			"etag": "\"16f2a\"",
			"other": {"etag": "not an etag"}}`),
	})
	want := map[string]any{
		"createTime": "<timestamp>",
		"updateTime": "2024-05-01T25:11:12Z", // left as is, so it mismatches
		"etag":       `"<etag:1>"`,
		"other":      map[string]any{"etag": "not an etag"},
	}
	assertJSON(t, got.Exchanges[0].Response.Body, want)
}

func TestGeneratedValuesAreReplacedInsideOtherStrings(t *testing.T) {
	got := normaliseAll(t, []Exchange{
		ok(t, "/o", `{"generation": "1700000000123456", "mediaLink": "https://h/download/o?generation=1700000000123456&alt=media"}`),
		ok(t, "/o?ifGenerationMatch=1700000000123456", `{}`),
	})
	assertJSON(t, got.Exchanges[0].Response.Body, map[string]any{
		"generation": "<generation:1>",
		"mediaLink":  "<host>/download/o?generation=<generation:1>&alt=media",
	})
	if p := got.Exchanges[1].Request.Path; p != "/o?ifGenerationMatch=<generation:1>" {
		t.Errorf("path = %q", p)
	}
}

func TestAReusedIDThatChangesIsAMismatch(t *testing.T) {
	want := normaliseAll(t, []Exchange{ok(t, "/t", `{"id": "aaaa1"}`), ok(t, "/t/1", `{"id": "aaaa1"}`)})
	got := normaliseAll(t, []Exchange{ok(t, "/t", `{"id": "bbbb1"}`), ok(t, "/t/1", `{"id": "cccc1"}`)})
	assertMismatches(t, compare(want, got, false), "exchanges[1].response.body.id")
}

func TestAMalformedTimestampIsAMismatch(t *testing.T) {
	want := normaliseAll(t, []Exchange{ok(t, "/s", `{"createTime": "2024-05-01T10:11:12Z"}`)})
	got := normaliseAll(t, []Exchange{ok(t, "/s", `{"createTime": "2024-05-01 10:11"}`)})
	assertMismatches(t, compare(want, got, false), "exchanges[0].response.body.createTime")
}

func TestExtraMissingAndChangedFieldsAreMismatches(t *testing.T) {
	want := normaliseAll(t, []Exchange{ok(t, "/b", `{"name": "b", "labels": {"a": "1"}, "acl": [{"role": "OWNER"}], "kind": "storage#bucket"}`)})
	got := normaliseAll(t, []Exchange{ok(t, "/b", `{"name": "b", "labels": {"a": "2"}, "acl": [{"role": "OWNER", "extra": true}], "location": "US"}`)})
	ms := compare(want, got, false)
	assertMismatches(t, ms,
		"exchanges[0].response.body.acl[0].extra",
		"exchanges[0].response.body.kind",
		"exchanges[0].response.body.labels.a",
		"exchanges[0].response.body.location",
	)
	text := mismatchText(ms)
	for _, s := range []string{
		`exchanges[0].response.body.acl[0].extra: unexpected field, got true`,
		`exchanges[0].response.body.kind: missing, want "storage#bucket"`,
		`exchanges[0].response.body.labels.a: want "1", got "2"`,
	} {
		if !strings.Contains(text, s) {
			t.Errorf("%q not in\n%s", s, text)
		}
	}
}

func TestStatusAndExchangeCountAreCompared(t *testing.T) {
	want := normaliseAll(t, []Exchange{ok(t, "/a", `{}`), ok(t, "/b", `{}`)})
	got := normaliseAll(t, []Exchange{{Request: Request{Method: "GET", Path: "/a"}, Response: Response{Status: 201, Body: map[string]any{}}}})
	assertMismatches(t, compare(want, got, false), "exchanges", "exchanges[0].response.status")
}

func TestUnorderedFieldsCompareInAnyOrder(t *testing.T) {
	a := `{"items": [{"name": "x", "v": 1}, {"name": "y", "v": 2}], "ordered": [1, 2]}`
	b := `{"items": [{"name": "y", "v": 2}, {"name": "x", "v": 1}], "ordered": [2, 1]}`
	opt := Unordered("items", "")
	want := normaliseAll(t, []Exchange{ok(t, "/l", a)}, opt)
	got := normaliseAll(t, []Exchange{ok(t, "/l", b)}, opt)
	assertMismatches(t, compare(want, got, false), "exchanges[0].response.body.ordered[0]", "exchanges[0].response.body.ordered[1]")

	// Another sort key, inside a nested array.
	opt = Unordered("bindings[].members", "")
	c := `{"bindings": [{"members": ["b", "a"]}]}`
	d := `{"bindings": [{"members": ["a", "b"]}]}`
	assertMismatches(t, compare(normaliseAll(t, []Exchange{ok(t, "/p", c)}, opt), normaliseAll(t, []Exchange{ok(t, "/p", d)}, opt), false))
	opt = Unordered("bindings", "role")
	e := `{"bindings": [{"role": "r1"}, {"role": "r0"}]}`
	f := `{"bindings": [{"role": "r0"}, {"role": "r1"}]}`
	assertMismatches(t, compare(normaliseAll(t, []Exchange{ok(t, "/p", e)}, opt), normaliseAll(t, []Exchange{ok(t, "/p", f)}, opt), false))
}

func TestAnIgnoredFieldNeedsAReason(t *testing.T) {
	if _, err := newNormaliser(ids, Ignore("storageClass", "")); err == nil {
		t.Error("Ignore with no reason: no error")
	}
	opt := Ignore("acl[].etag", "ACL etags are per entity and not part of this check")
	want := normaliseAll(t, []Exchange{ok(t, "/b", `{"acl": [{"etag": "CAE=", "role": "OWNER"}]}`)}, opt)
	got := normaliseAll(t, []Exchange{ok(t, "/b", `{"acl": [{"etag": "CAI=", "role": "OWNER"}]}`)}, opt)
	assertMismatches(t, compare(want, got, false))
	assertJSON(t, got.Exchanges[0].Response.Body, map[string]any{"acl": []any{map[string]any{"role": "OWNER"}}})
}

func TestErrorsCompareCodeAndReasonNotMessage(t *testing.T) {
	restErr := func(status int, reason, msg string) Exchange {
		return Exchange{Request: Request{Method: "GET", Path: "/b"}, Response: Response{Status: status, Body: body(t, `{"error": {"code": 404, "message": "`+msg+`", "errors": [{"reason": "`+reason+`", "message": "`+msg+`"}]}}`)}}
	}
	grpcErr := func(code, reason, field, msg string) Exchange {
		return Exchange{Request: Request{Method: "GRPC", Path: "/google.pubsub.v1.Publisher/GetTopic"}, Response: Response{Code: code, Body: body(t, `{"code": 5, "message": "`+msg+`", "details": [
			{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "`+reason+`"},
			{"@type": "type.googleapis.com/google.rpc.BadRequest", "fieldViolations": [{"field": "`+field+`", "description": "`+msg+`"}]}]}`)}}
	}
	norm := func(e Exchange) Recording { return normaliseAll(t, []Exchange{e}) }

	assertMismatches(t, compare(norm(restErr(404, "notFound", "No such bucket")), norm(restErr(404, "notFound", "bucket missing")), false))
	assertMismatches(t, compare(norm(restErr(404, "notFound", "a")), norm(restErr(404, "required", "a")), false), "exchanges[0].response.body.reasons[0]")
	assertMismatches(t, compare(norm(restErr(404, "notFound", "a")), norm(restErr(400, "notFound", "a")), false), "exchanges[0].response.status")
	assertMismatches(t, compare(norm(restErr(404, "notFound", "a")), norm(restErr(404, "notFound", "b")), true), "exchanges[0].response.body.message")

	assertMismatches(t, compare(norm(grpcErr("NOT_FOUND", "R", "f", "x")), norm(grpcErr("NOT_FOUND", "R", "f", "y")), false))
	assertMismatches(t, compare(norm(grpcErr("NOT_FOUND", "R", "f", "x")), norm(grpcErr("ALREADY_EXISTS", "R", "f", "x")), false), "exchanges[0].response.code")
	assertMismatches(t, compare(norm(grpcErr("NOT_FOUND", "R", "f", "x")), norm(grpcErr("NOT_FOUND", "R", "g", "x")), false), "exchanges[0].response.body.fieldViolations[0]")
}

func TestRecordingFilesAreDeterministic(t *testing.T) {
	raw := []Exchange{ok(t, "/b", `{"z": 1, "a": {"y": "<&>", "b": 2.50}}`)}
	first := encode(t, normaliseAll(t, raw))
	if first != encode(t, normaliseAll(t, raw)) {
		t.Error("encoding differs between runs")
	}
	if !strings.HasSuffix(first, "}\n") || !strings.Contains(first, `"<&>"`) || !strings.Contains(first, `2.50`) {
		t.Errorf("encoding:\n%s", first)
	}
	r, err := parseRecording([]byte(first))
	if err != nil {
		t.Fatal(err)
	}
	if encode(t, r) != first {
		t.Error("a parsed Recording does not encode to the same bytes")
	}
}

// assertJSON fails unless got encodes as want does.
func assertJSON(t *testing.T, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("got  %s\nwant %s", g, w)
	}
}

// assertMismatches fails unless ms has exactly paths, in order.
func assertMismatches(t *testing.T, ms []Mismatch, paths ...string) {
	t.Helper()
	var got []string
	for _, m := range ms {
		got = append(got, m.Path)
	}
	if strings.Join(got, "\n") != strings.Join(paths, "\n") {
		t.Errorf("mismatch paths:\n%s\nwant:\n%s\n(%s)", strings.Join(got, "\n"), strings.Join(paths, "\n"), mismatchText(ms))
	}
}

func TestRunOnlyKeepsThisRunsElements(t *testing.T) {
	got := normaliseAll(t, []Exchange{
		ok(t, "/topics", `{"topics": [{"name": "projects/my-proj/topics/gcpemu-old000-t"}, {"name": "projects/my-proj/topics/gcpemu-k2x9ab-t"}]}`),
	}, RunOnly("topics"))
	assertJSON(t, got.Exchanges[0].Response.Body, map[string]any{
		"topics": []any{map[string]any{"name": "projects/<project>/topics/gcpemu-<suffix>-t"}},
	})
}

func TestOperationIDsArePlaceholders(t *testing.T) {
	got := normaliseAll(t, []Exchange{
		ok(t, "/r", `{"name": "projects/my-proj/locations/us/operations/0f3a-77b1"}`),
		ok(t, "/projects/my-proj/locations/us/operations/0f3a-77b1", `{"name": "projects/my-proj/locations/us/operations/9e9e"}`),
	})
	assertJSON(t, got.Exchanges[0].Response.Body, map[string]any{"name": "projects/<project>/locations/us/operations/<operation:1>"})
	assertJSON(t, got.Exchanges[1].Response.Body, map[string]any{"name": "projects/<project>/locations/us/operations/<operation:2>"})
	if p := got.Exchanges[1].Request.Path; p != "/projects/<project>/locations/us/operations/<operation:1>" {
		t.Errorf("path = %q", p)
	}
}

func TestPluralGeneratedKeysShareTheSingularsPlaceholders(t *testing.T) {
	got := normaliseAll(t, []Exchange{
		ok(t, "/publish", `{"messageIds": ["7"]}`),
		ok(t, "/pull", `{"receivedMessages": [{"ackId": "2-2", "message": {"messageId": "7"}}]}`),
		{Request: Request{Method: "POST", Path: "/ack", Body: body(t, `{"ackIds": ["2-2"]}`)}, Response: Response{Status: 200}},
	})
	assertJSON(t, got.Exchanges[0].Response.Body, map[string]any{"messageIds": []any{"<messageId:1>"}})
	assertJSON(t, got.Exchanges[1].Response.Body, map[string]any{"receivedMessages": []any{map[string]any{"ackId": "<ackId:1>", "message": map[string]any{"messageId": "<messageId:1>"}}}})
	assertJSON(t, got.Exchanges[2].Request.Body, map[string]any{"ackIds": []any{"<ackId:1>"}})
}

func TestAnIDMadeFromTheTestsNameIsKept(t *testing.T) {
	got := normaliseAll(t, []Exchange{
		ok(t, "/b", `{"id": "gcpemu-k2x9ab-b", "name": "gcpemu-k2x9ab-b"}`),
		ok(t, "/o", `{"id": "gcpemu-k2x9ab-b/o/1700000000123456", "generation": "1700000000123456"}`),
	})
	assertJSON(t, got.Exchanges[0].Response.Body, map[string]any{"id": "gcpemu-<suffix>-b", "name": "gcpemu-<suffix>-b"})
	assertJSON(t, got.Exchanges[1].Response.Body, map[string]any{"generation": "<generation:1>", "id": "gcpemu-<suffix>-b/o/<generation:1>"})
}

func TestSuffixDatesTheRun(t *testing.T) {
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	s := newSuffix(at)
	if len(s) != 10 || strings.Trim(s, "0123456789abcdefghijklmnopqrstuvwxyz") != "" {
		t.Fatalf("suffix %q", s)
	}
	if got, ok := suffixTime(s); !ok || !got.Equal(at) {
		t.Errorf("suffixTime(%q) = %v, %v", s, got, ok)
	}
	if _, ok := suffixTime("k2x9ab"); ok {
		t.Error("a short suffix parsed")
	}
}
