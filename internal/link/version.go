package link

import (
	"fmt"
	"strconv"
	"strings"
)

// Versions is the range of link protocol versions an Instance speaks.
type Versions struct{ Min, Max int }

// Current is the range this build speaks.
var Current = Versions{Min: 1, Max: 1}

// String is "1", or "1-2" for a range.
func (v Versions) String() string {
	if v.Min == v.Max {
		return strconv.Itoa(v.Max)
	}
	return fmt.Sprintf("%d-%d", v.Min, v.Max)
}

// parseVersions parses String's form; "" and anything malformed is the
// zero Versions, which no Instance speaks.
func parseVersions(s string) Versions {
	lo, hi, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		hi = lo
	}
	a, err1 := strconv.Atoi(lo)
	b, err2 := strconv.Atoi(hi)
	if err1 != nil || err2 != nil || a < 1 || a > b {
		return Versions{}
	}
	return Versions{a, b}
}

// Negotiate returns the highest version that ours and theirs, those of the
// Instance named who, both speak, or an error naming both.
func Negotiate(ours, theirs Versions, who string) (int, error) {
	v := min(ours.Max, theirs.Max)
	if theirs.Min < 1 || v < max(ours.Min, theirs.Min) {
		return 0, fmt.Errorf("incompatible link protocol: this Instance speaks version %s and %s speaks version %s; upgrade the older gcpemu", ours, who, theirs)
	}
	return v, nil
}
