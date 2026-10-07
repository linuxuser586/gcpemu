//go:build !unix

package dns

func setReuseAddr(uintptr) error { return nil }
