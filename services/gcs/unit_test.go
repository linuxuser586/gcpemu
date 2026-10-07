package gcs

import (
	"testing"

	storage "google.golang.org/api/storage/v1"
)

func TestValidBucketName(t *testing.T) {
	for name, want := range map[string]bool{
		"my-bucket": true, "a.b.c": true, "abc": true, "a_b-1": true,
		"ab": false, "Upper": false, "-start": false, "end-": false, "goog-x": false,
		"my-google-bucket": false, "192.168.1.1": false, "a..b": false,
		"a234567890123456789012345678901234567890123456789012345678901234": false,
	} {
		if got := validBucketName(name); got != want {
			t.Errorf("validBucketName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestGlob(t *testing.T) {
	for _, tc := range []struct {
		glob, name string
		want       bool
	}{
		{"*.txt", "a.txt", true},
		{"*.txt", "d/a.txt", false},
		{"**.txt", "d/a.txt", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"?", "a", true},
		{"[ab]c", "bc", true},
		{"[!ab]c", "bc", false},
		{"{foo,bar}.js", "bar.js", true},
		{"{foo,bar}.js", "baz.js", false},
	} {
		re, err := globRegexp(tc.glob)
		if err != nil {
			t.Fatalf("%s: %v", tc.glob, err)
		}
		if got := re.MatchString(tc.name); got != tc.want {
			t.Errorf("glob %q on %q = %v", tc.glob, tc.name, got)
		}
	}
}

func TestParseRange(t *testing.T) {
	for _, tc := range []struct {
		h          string
		start, end int64
		ok, sat    bool
	}{
		{"bytes=0-9", 0, 9, true, true},
		{"bytes=5-", 5, 99, true, true},
		{"bytes=-10", 90, 99, true, true},
		{"bytes=90-200", 90, 99, true, true},
		{"bytes=100-", 0, 0, true, false},
		{"", 0, 0, false, true},
		{"bytes=0-1,4-5", 0, 0, false, true},
	} {
		br, ok, sat := parseRange(tc.h, 100)
		if ok != tc.ok || sat != tc.sat || (ok && sat && (br.start != tc.start || br.end != tc.end)) {
			t.Errorf("parseRange(%q) = %+v %v %v", tc.h, br, ok, sat)
		}
	}
}

func TestEtagAndConds(t *testing.T) {
	if got := bucketEtag(1); got != "CAE=" {
		t.Errorf("bucketEtag(1) = %s", got)
	}
	o := &storage.Object{Generation: 5, Metageneration: 2}
	zero, five, six := int64(0), int64(5), int64(6)
	for _, tc := range []struct {
		c    conds
		cur  *storage.Object
		read bool
		code int
	}{
		{conds{GenMatch: &zero}, nil, false, 0},
		{conds{GenMatch: &zero}, o, false, 412},
		{conds{GenMatch: &five}, o, false, 0},
		{conds{GenNotMatch: &five}, o, true, 304},
		{conds{GenNotMatch: &six}, o, false, 0},
		{conds{MetagenMatch: &six}, o, false, 412},
	} {
		err := tc.c.check(tc.cur, tc.read)
		code := 0
		if err != nil {
			code = err.(interface{ HTTP() int }).HTTP()
		}
		if code != tc.code {
			t.Errorf("%+v: code %d, want %d", tc.c, code, tc.code)
		}
	}
}

func TestContentRange(t *testing.T) {
	cr, err := parseContentRange("bytes 0-99/*")
	if err != nil || cr.first != 0 || cr.last != 99 || cr.total != -1 {
		t.Errorf("%+v %v", cr, err)
	}
	cr, err = parseContentRange("bytes */100")
	if err != nil || !cr.status || cr.total != 100 {
		t.Errorf("%+v %v", cr, err)
	}
	if _, err := parseContentRange("bytes 5-1/10"); err == nil {
		t.Error("expected error")
	}
}

func TestApplyPatch(t *testing.T) {
	cur := &storage.Object{Name: "o", ContentType: "a/b", Metadata: map[string]string{"x": "1", "y": "2"}}
	var out storage.Object
	body := map[string][]byte{"metadata": []byte(`{"x":null,"z":"3"}`), "name": []byte(`"evil"`)}
	raw := map[string]jsonRaw{}
	for k, v := range body {
		raw[k] = v
	}
	if err := applyPatch(cur, raw, mutableObjectFields, false, &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "o" || out.Metadata["x"] != "" || out.Metadata["y"] != "2" || out.Metadata["z"] != "3" || out.ContentType != "a/b" {
		t.Errorf("patched = %+v", out)
	}
}
