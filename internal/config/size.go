package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseSize parses a size such as "1GiB", "512MiB", "100MB", "64k" or a
// plain byte count, which may be 0.
func ParseSize(v string) (int64, error) {
	v = strings.TrimSpace(v)
	i := len(v)
	for i > 0 && (v[i-1] < '0' || v[i-1] > '9') {
		i--
	}
	num, unit := strings.TrimSpace(v[:i]), strings.ToLower(strings.TrimSpace(v[i:]))
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", v)
	}
	mult := map[string]int64{
		"": 1, "b": 1,
		"k": 1 << 10, "kb": 1000, "kib": 1 << 10,
		"m": 1 << 20, "mb": 1000 * 1000, "mib": 1 << 20,
		"g": 1 << 30, "gb": 1000 * 1000 * 1000, "gib": 1 << 30,
		"t": 1 << 40, "tb": 1000 * 1000 * 1000 * 1000, "tib": 1 << 40,
	}[unit]
	if mult == 0 {
		return 0, fmt.Errorf("invalid size unit in %q", v)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q is too large", v)
	}
	return n * mult, nil
}
