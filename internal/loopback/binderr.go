package loopback

import "errors"

// AddrInUse reports whether err is a bind failing because another socket
// holds the address.
func AddrInUse(err error) bool { return isAny(err, addrInUseErrnos) }

// BindDenied reports whether err is a bind refused for lack of permission,
// such as a privileged port.
func BindDenied(err error) bool { return isAny(err, bindDeniedErrnos) }

func isAny(err error, targets []error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}
