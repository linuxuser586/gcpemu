//go:build !unix

package netplane

func setReuseAddr(uintptr) error { return nil }
