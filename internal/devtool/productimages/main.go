// Command productimages prints every Product image (CONTEXT.md), one
// reference per line, sorted, from the values the emulator runs: Cloud
// SQL's postgres images, GKE's k3s images, the system images they preload
// and the add-on manifests' images, and the Managed emulator images. The
// weekly scan (#1165) reads its output, so no workflow keeps a list.
//
// It is its own package, not part of devtool, because it imports the
// emulator: devtool uses only the standard library (ADR 0061), and `go
// run ./internal/devtool product-images` runs this.
package main

import (
	"fmt"
	"os"
	"slices"

	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/services/gke"
	"github.com/linuxuser586/gcpemu/services/sql"
)

func main() {
	images, err := productImages()
	if err != nil {
		fmt.Fprintln(os.Stderr, "product-images:", err)
		os.Exit(1)
	}
	for _, i := range images {
		fmt.Println(i)
	}
}

// productImages returns the Product images, sorted and without repeats.
func productImages() ([]string, error) {
	g, err := gke.ProductImages()
	if err != nil {
		return nil, err
	}
	out := slices.Concat(sql.ProductImages(), g, instance.ProductImages())
	slices.Sort(out)
	return slices.Compact(out), nil
}
