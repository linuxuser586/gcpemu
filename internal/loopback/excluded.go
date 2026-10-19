package loopback

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// PortRange is one of Windows' excluded port ranges, Start to End
// inclusive.
type PortRange struct{ Start, End int }

func (r PortRange) String() string {
	if r.Start == r.End {
		return strconv.Itoa(r.Start)
	}
	return fmt.Sprintf("%d-%d", r.Start, r.End)
}

// ExcludedRanges reads Windows' excluded port ranges of network ("tcp" or
// "udp") with netsh.
func ExcludedRanges(network string) ([]PortRange, error) {
	out, err := exec.Command("netsh", "int", "ipv4", "show", "excludedportrange", "protocol="+network).Output()
	if err != nil {
		return nil, fmt.Errorf("netsh int ipv4 show excludedportrange protocol=%s: %w", network, err)
	}
	return ParseExcludedRanges(out), nil
}

// ParseExcludedRanges reads the output of `netsh int ipv4 show
// excludedportrange`: its rows are a start and an end port, and a "*" on
// administered exclusions. The headings, which Windows translates, are
// skipped.
func ParseExcludedRanges(out []byte) []PortRange {
	var ranges []PortRange
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || len(f) > 3 || len(f) == 3 && f[2] != "*" {
			continue
		}
		start, err1 := strconv.Atoi(f[0])
		end, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			ranges = append(ranges, PortRange{start, end})
		}
	}
	return ranges
}

// ExcludedPort returns the range of ranges that holds port, if any.
func ExcludedPort(ranges []PortRange, port int) (PortRange, bool) {
	for _, r := range ranges {
		if port >= r.Start && port <= r.End {
			return r, true
		}
	}
	return PortRange{}, false
}
