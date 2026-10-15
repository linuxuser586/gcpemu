//go:build !linux

package netplane

// wildcardHolder names a wildcard bind on port where the platform reports
// one; elsewhere the error of the bind itself is reported.
func wildcardHolder(port int, udp bool) string { return "" }
