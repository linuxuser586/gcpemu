package listpage

import (
	"slices"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// TestOffsetWalk: following next tokens visits every item once, in order,
// and the last page has no next token.
func TestOffsetWalk(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	var got []int
	tok := ""
	for range 10 {
		page, next, err := Offset(items, tok, 2, Options{Max: 1000})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, page...)
		if tok = next; tok == "" {
			break
		}
	}
	if !slices.Equal(got, items) {
		t.Fatalf("walked %v", got)
	}
}

// TestOffsetEdges pins both modes at the edges Services differ on.
func TestOffsetEdges(t *testing.T) {
	items := []int{0, 1, 2, 3, 4}
	strict, lenient := Options{Max: 3, Strict: true}, Options{Max: 3}
	cases := []struct {
		name  string
		token string
		size  int32
		o     Options
		page  []int
		next  string
		err   string
	}{
		{"first page", "", 2, lenient, []int{0, 1}, "Mg", ""},
		{"older run's token", "Mg", 2, strict, []int{2, 3}, "NA", ""},
		{"last page", "NA", 2, strict, []int{4}, "", ""},
		{"zero size is max", "", 0, strict, []int{0, 1, 2}, "Mw", ""},
		{"oversized is max", "", 100, lenient, []int{0, 1, 2}, "Mw", ""},
		{"offset at end", "NQ", 2, strict, []int{}, "", ""},
		{"strict negative size", "", -1, strict, nil, "", "page_size must not be negative."},
		{"lenient negative size is max", "", -1, lenient, []int{0, 1, 2}, "Mw", ""},
		{"strict past end", "Ng", 2, strict, nil, "", "Invalid page token."},
		{"lenient past end", "Ng", 2, lenient, []int{}, "", ""},
		{"negative offset", "LTE", 2, lenient, nil, "", "Invalid page token."},
		{"not base64", "!!", 2, lenient, nil, "", "Invalid page token."},
		{"not a number", "eA", 2, strict, nil, "", "Invalid page token."},
	}
	for _, c := range cases {
		page, next, err := Offset(items, c.token, c.size, c.o)
		if c.err != "" {
			if e := apierr.From(err); err == nil || e.Code != codes.InvalidArgument || e.Message != c.err {
				t.Errorf("%s: err = %v, want INVALID_ARGUMENT %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil || !slices.Equal(page, c.page) || next != c.next {
			t.Errorf("%s: = %v, %q, %v; want %v, %q", c.name, page, next, err, c.page, c.next)
		}
	}
}
