package netplane

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// wildcardHolder names the wildcard bind ("0.0.0.0:P" or "[::]:P") that a
// socket on the host holds on port, from /proc/net, or "".
func wildcardHolder(port int, udp bool) string {
	proto, state := "tcp", "0A" // LISTEN
	if udp {
		proto, state = "udp", "07" // unconnected
	}
	for _, t := range []struct{ file, any, name string }{
		{proto, "00000000", "0.0.0.0"},
		{proto + "6", "00000000000000000000000000000000", "[::]"},
	} {
		if holds(t.file, t.any+fmt.Sprintf(":%04X", port), state) {
			return fmt.Sprintf("%s:%d", t.name, port)
		}
	}
	return ""
}

// holds reports whether /proc/net/<file> has a socket at local in state.
func holds(file, local, state string) bool {
	f, err := os.Open("/proc/net/" + file)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) > 3 && fs[1] == local && fs[3] == state {
			return true
		}
	}
	return false
}
