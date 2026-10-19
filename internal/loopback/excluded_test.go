package loopback

import (
	"reflect"
	"strings"
	"testing"
)

// netshTCP is `netsh int ipv4 show excludedportrange protocol=tcp` on a
// Windows 11 host with Hyper-V and Docker Desktop.
const netshTCP = `
Protocol tcp Port Exclusion Ranges

Start Port    End Port      
----------    --------      
      8033        8033      
     20250       20250      
     50000       50059     *
     54640       54739      

* - Administered port exclusions.

`

func TestParseExcludedRanges(t *testing.T) {
	want := []PortRange{{8033, 8033}, {20250, 20250}, {50000, 50059}, {54640, 54739}}
	for name, out := range map[string]string{"LF": netshTCP, "CRLF": strings.ReplaceAll(netshTCP, "\n", "\r\n")} {
		if got := ParseExcludedRanges([]byte(out)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ParseExcludedRanges = %v, want %v", name, got, want)
		}
	}
	if got := ParseExcludedRanges([]byte("\nProtocol udp Port Exclusion Ranges\n\nStart Port    End Port\n----------    --------\n\n* - Administered port exclusions.\n")); len(got) != 0 {
		t.Errorf("no ranges: ParseExcludedRanges = %v, want none", got)
	}
}

func TestExcludedPort(t *testing.T) {
	ranges := ParseExcludedRanges([]byte(netshTCP))
	for port, want := range map[int]bool{8033: true, 8034: false, 49999: false, 50000: true, 50030: true, 50059: true, 50060: false, 80: false} {
		r, got := ExcludedPort(ranges, port)
		if got != want {
			t.Errorf("ExcludedPort(%d) = %v (%v), want %v", port, got, r, want)
		}
		if got && (port < r.Start || port > r.End) {
			t.Errorf("ExcludedPort(%d) = %v, which does not hold it", port, r)
		}
	}
}
