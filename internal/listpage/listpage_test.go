package listpage

import (
	"encoding/base64"
	"slices"
	"strconv"
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

// result is what Offset should return: a page and next token, or an
// INVALID_ARGUMENT with message err.
type result struct {
	page []int
	next string
	err  string
}

// TestOffsetEdges: both modes at the edges Services differ on, and at the
// ones they agree on.
func TestOffsetEdges(t *testing.T) {
	items := []int{0, 1, 2, 3, 4}
	firstMax := result{[]int{0, 1, 2}, "Mw", ""}
	badToken := result{err: "Invalid page token."}
	cases := []struct {
		name            string
		token           string
		size            int32
		strict, lenient result
	}{
		{"first page", "", 2, result{[]int{0, 1}, "Mg", ""}, result{[]int{0, 1}, "Mg", ""}},
		{"valid token", "Mg", 2, result{[]int{2, 3}, "NA", ""}, result{[]int{2, 3}, "NA", ""}},
		{"last page", "NA", 2, result{[]int{4}, "", ""}, result{[]int{4}, "", ""}},
		{"offset at end", "NQ", 2, result{[]int{}, "", ""}, result{[]int{}, "", ""}},
		{"zero size", "", 0, firstMax, firstMax},
		{"oversized", "", 100, firstMax, firstMax},
		{"negative size", "", -1, result{err: "page_size must not be negative."}, firstMax},
		{"past end", "Ng", 2, badToken, result{[]int{}, "", ""}},
		{"negative offset", "LTE", 2, badToken, badToken},
		{"not base64", "!!", 2, badToken, badToken},
		{"not a number", "eA", 2, badToken, badToken},
	}
	for _, c := range cases {
		for mode, want := range map[string]result{"strict": c.strict, "lenient": c.lenient} {
			page, next, err := Offset(items, c.token, c.size, Options{Max: 3, Strict: mode == "strict"})
			if want.err != "" {
				if e := apierr.From(err); err == nil || e.Code != codes.InvalidArgument || e.Message != want.err {
					t.Errorf("%s, %s: err = %v, want INVALID_ARGUMENT %q", c.name, mode, err, want.err)
				}
				continue
			}
			if err != nil || !slices.Equal(page, want.page) || next != want.next {
				t.Errorf("%s, %s: = %v, %q, %v; want %v, %q", c.name, mode, page, next, err, want.page, want.next)
			}
		}
	}
}

// TestDecodeOldTokens: tokens the Services' own pagers encoded before
// listpage decode to the same offset.
func TestDecodeOldTokens(t *testing.T) {
	for _, n := range []int{0, 1, 999, 25000} {
		old := base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(n)))
		if got, err := DecodeOffset(old); err != nil || got != n {
			t.Errorf("DecodeOffset(%q) = %d, %v; want %d", old, got, err, n)
		}
		if got := EncodeOffset(n); got != old {
			t.Errorf("EncodeOffset(%d) = %q, want %q", n, got, old)
		}
	}
}
