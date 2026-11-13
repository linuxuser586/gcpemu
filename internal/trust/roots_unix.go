//go:build !windows && !darwin

package trust

// hostRoots returns the host's roots: its system bundle file.
func hostRoots() ([]byte, Source) {
	b, where := fileRoots()
	return b, Source{Where: where}
}
