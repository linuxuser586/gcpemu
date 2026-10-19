//go:build !unix && !windows

package netplane

func setReuseAddr(uintptr) error { return nil }
