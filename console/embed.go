// Package console embeds the Web console's built bundle (FR-UI-001).
//
// dist/bundle/ holds the output of `pnpm build` (run by `just build` when
// pnpm is on PATH) and is never committed; placeholder.html is, so that
// plain `go build` works without Node and serves "console not built"
// (ADR 0060).
package console

import (
	"embed"
	"io/fs"
	"testing/fstest"
)

//go:embed all:dist
var dist embed.FS

//go:embed placeholder.html
var placeholder []byte

// FS is the bundle, rooted at dist/bundle/, or the placeholder page as
// index.html when no bundle was built.
var FS fs.FS

// Built reports whether FS is a real bundle rather than the placeholder.
var Built bool

func init() {
	root, _ := fs.Sub(dist, "dist")
	FS, Built = choose(root, placeholder)
}

// choose returns the bundle under bundle/ in dist when it has an
// index.html, and otherwise an FS holding only page, as index.html.
func choose(dist fs.FS, page []byte) (fs.FS, bool) {
	if _, err := fs.Stat(dist, "bundle/index.html"); err != nil {
		return fstest.MapFS{"index.html": {Data: page}}, false
	}
	bundle, err := fs.Sub(dist, "bundle")
	if err != nil {
		panic(err) // "bundle" is a valid path; fs.Sub fails on nothing else
	}
	return bundle, true
}
