package main

import (
	"strconv"
	"strings"
)

// versionAtLeast reports whether version have is at least need, comparing
// dot-separated fields as numbers. A missing field counts as 0, and a
// field's non-digit suffix (the rc1 of 1.28rc1) is dropped.
func versionAtLeast(have, need string) bool {
	for have != "" || need != "" {
		var hf, nf string
		hf, have, _ = strings.Cut(have, ".")
		nf, need, _ = strings.Cut(need, ".")
		h, n := leadingInt(hf), leadingInt(nf)
		if h != n {
			return h > n
		}
	}
	return true
}

// leadingInt is the number s starts with, or 0.
func leadingInt(s string) int {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}
