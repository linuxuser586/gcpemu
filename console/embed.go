// Package console embeds the Web console's built bundle (FR-UI-001).
//
// dist/ holds the output of `pnpm build` (run by `make build` when pnpm
// is on PATH). Only dist/index.html is committed: a placeholder page, so
// that plain `go build` works without Node and serves "console not built".
package console

import (
	"bytes"
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS is the bundle, rooted at dist/.
var FS fs.FS

// Built reports whether FS holds a real bundle rather than the placeholder.
var Built bool

// placeholder marks the committed dist/index.html.
var placeholder = []byte(`<meta name="gcpemu-console" content="placeholder"`)

func init() {
	FS, _ = fs.Sub(dist, "dist")
	index, err := fs.ReadFile(FS, "index.html")
	Built = err == nil && !bytes.Contains(index, placeholder)
}
