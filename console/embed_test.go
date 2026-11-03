package console

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestChoose(t *testing.T) {
	page := []byte("<h1>Console not built</h1>")
	tests := []struct {
		name      string
		dist      fstest.MapFS
		wantBuilt bool
		wantIndex string
		wantAsset bool
	}{
		{
			name:      "fresh clone",
			dist:      fstest.MapFS{".keep": {}},
			wantIndex: string(page),
		},
		{
			name: "built",
			dist: fstest.MapFS{
				".keep":                {},
				"bundle/index.html":    {Data: []byte("<div id=root>")},
				"bundle/assets/app.js": {Data: []byte("app")},
			},
			wantBuilt: true,
			wantIndex: "<div id=root>",
			wantAsset: true,
		},
		{
			// A build that died after emptying bundle/, or an old
			// checkout's leftovers beside it, is not a bundle.
			name: "no bundle index",
			dist: fstest.MapFS{
				"bundle/assets/app.js": {Data: []byte("app")},
				"index.html":           {Data: []byte("<div id=root>")},
				"assets/app.js":        {Data: []byte("app")},
			},
			wantIndex: string(page),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys, built := choose(tt.dist, page)
			if built != tt.wantBuilt {
				t.Errorf("built = %v, want %v", built, tt.wantBuilt)
			}
			index, err := fs.ReadFile(fsys, "index.html")
			if err != nil || string(index) != tt.wantIndex {
				t.Errorf("index.html = %q, %v; want %q", index, err, tt.wantIndex)
			}
			if _, err := fs.Stat(fsys, "assets/app.js"); (err == nil) != tt.wantAsset {
				t.Errorf("assets/app.js: err = %v, want found %v", err, tt.wantAsset)
			}
		})
	}
}

// The embedded tree is what a fresh clone or a build left: Built must agree
// with whether bundle/index.html is there.
func TestEmbedded(t *testing.T) {
	_, err := fs.Stat(dist, "dist/bundle/index.html")
	if Built != (err == nil) {
		t.Errorf("Built = %v, but dist/bundle/index.html: %v", Built, err)
	}
	if _, err := fs.Stat(FS, "index.html"); err != nil {
		t.Errorf("FS has no index.html: %v", err)
	}
}
