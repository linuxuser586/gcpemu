//go:build !unix && !windows

package mdns

var bindIP = Group

func setReuse(uintptr) error { return nil }
