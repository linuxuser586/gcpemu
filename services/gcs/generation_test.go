package gcs_test

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/linuxuser586/gcpemu/emutest"
)

// A bucket deleted and recreated within one request, here a JSON batch,
// gets a new, higher generation although the deterministic clock does not
// move within the request (ADR 0020).
func TestBucketRecreatedInBatchGetsHigherGeneration(t *testing.T) {
	inst := start(t, emutest.WithDeterministic())
	base := inst.GatewayURL()
	code, _, b := do(t, "POST", base+"/storage/v1/b?project="+testProject, strings.NewReader(`{"name":"regen"}`), nil)
	if code != 200 {
		t.Fatalf("create = %d %s", code, b)
	}
	var first struct{ Generation string }
	if err := json.Unmarshal(b, &first); err != nil {
		t.Fatal(err)
	}
	body := "--B\r\nContent-Type: application/http\r\nContent-ID: <1>\r\n\r\nDELETE /storage/v1/b/regen HTTP/1.1\r\n\r\n\r\n" +
		"--B\r\nContent-Type: application/http\r\nContent-ID: <2>\r\n\r\nPOST /storage/v1/b?project=" + testProject + " HTTP/1.1\r\nContent-Type: application/json\r\nContent-Length: 16\r\n\r\n{\"name\":\"regen\"}\r\n" +
		"--B\r\nContent-Type: application/http\r\nContent-ID: <3>\r\n\r\nDELETE /storage/v1/b/regen HTTP/1.1\r\n\r\n\r\n" +
		"--B\r\nContent-Type: application/http\r\nContent-ID: <4>\r\n\r\nPOST /storage/v1/b?project=" + testProject + " HTTP/1.1\r\nContent-Type: application/json\r\nContent-Length: 16\r\n\r\n{\"name\":\"regen\"}\r\n--B--\r\n"
	code, _, b = do(t, "POST", base+"/batch/storage/v1", strings.NewReader(body), map[string]string{"Content-Type": "multipart/mixed; boundary=B"})
	gens := regexp.MustCompile(`"generation": ?"(\d+)"`).FindAllStringSubmatch(string(b), -1)
	if code != 200 || len(gens) != 2 {
		t.Fatalf("batch = %d %s", code, b)
	}
	prev, _ := strconv.ParseInt(first.Generation, 10, 64)
	for _, g := range gens {
		n, _ := strconv.ParseInt(g[1], 10, 64)
		if n <= prev {
			t.Fatalf("generations %s then %v, want each higher than the last", first.Generation, gens)
		}
		prev = n
	}
}
