// Package listpage pages a list call's results by offset. The page token
// is the unpadded base64url encoding of the decimal offset of the page's
// first item, so a token from an older run still decodes.
package listpage

import (
	"encoding/base64"
	"strconv"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// Options sets how a Service pages.
type Options struct {
	// Max is the largest page size, and the size when page_size is 0.
	Max int
	// Strict refuses a negative page_size and a token past the end of the
	// list as INVALID_ARGUMENT. Otherwise a negative page_size is Max and a
	// token past the end gives an empty last page.
	Strict bool
}

// Offset returns the page of items that token starts and size bounds, and
// the token for the next page ("" on the last page).
func Offset[T any](items []T, token string, size int32, o Options) ([]T, string, error) {
	start, err := DecodeOffset(token)
	if err != nil {
		return nil, "", err
	}
	if start > len(items) {
		if o.Strict {
			return nil, "", errInvalidToken()
		}
		start = len(items)
	}
	if size < 0 && o.Strict {
		return nil, "", apierr.InvalidArgument("page_size must not be negative.")
	}
	n := int(size)
	if n <= 0 || n > o.Max {
		n = o.Max
	}
	end := min(start+n, len(items))
	next := ""
	if end < len(items) {
		next = EncodeOffset(end)
	}
	return items[start:end], next, nil
}

// EncodeOffset returns the page token for offset n.
func EncodeOffset(n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(n)))
}

// DecodeOffset returns the offset token holds, 0 for "". A token that is
// not one EncodeOffset made, or holds a negative offset, is
// INVALID_ARGUMENT.
func DecodeOffset(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, errInvalidToken()
	}
	n, err := strconv.Atoi(string(b))
	if err != nil || n < 0 {
		return 0, errInvalidToken()
	}
	return n, nil
}

func errInvalidToken() error { return apierr.InvalidArgument("Invalid page token.") }
