package runtime

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecondToLast(t *testing.T) {
	for in, want := range map[string]string{
		"10.90.0.0/24":  "10.90.0.254",
		"172.24.0.0/16": "172.24.255.254",
		"10.0.0.8/29":   "10.0.0.14",
		"10.0.0.0/30":   "",
		"fd00::/64":     "",
		"not-a-network": "",
	} {
		if got := secondToLast(in); got != want {
			t.Errorf("secondToLast(%s) = %q, want %q", in, got, want)
		}
	}
}

// SelfPorts lists the TCP ports the emulator's own container publishes, at
// the host address a host client dials: 127.0.0.1 for the wildcard.
func TestSelfPorts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/containers/self1/json") {
			w.WriteHeader(404)
			return
		}
		_, _ = io.WriteString(w, `{"Id":"self1","NetworkSettings":{"Ports":{
			"4510/tcp":[{"HostIp":"0.0.0.0","HostPort":"4510"},{"HostIp":"::","HostPort":"4510"}],
			"20000/tcp":[{"HostIp":"127.0.0.1","HostPort":"30000"}],
			"20001/tcp":[{"HostIp":"192.168.1.20","HostPort":"20001"}],
			"20002/tcp":null,
			"5353/udp":[{"HostIp":"127.0.0.1","HostPort":"5353"}]}}}`)
	}))
	defer srv.Close()
	cl, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cl.SelfPorts(context.Background()); err != nil || got != nil {
		t.Errorf("on the host: SelfPorts = %v, %v; want none", got, err)
	}
	cl.self = "self1"
	got, err := cl.SelfPorts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{4510: "127.0.0.1:4510", 20000: "127.0.0.1:30000", 20001: "192.168.1.20:20001"}
	if !maps.Equal(got, want) {
		t.Errorf("SelfPorts = %v, want %v", got, want)
	}
}
