package runtime

import (
	"context"
	"encoding/json"
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
			"4562/tcp":[{"HostIp":"0.0.0.0","HostPort":"4562"},{"HostIp":"::","HostPort":"4562"}],
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
	want := map[int]string{4562: "127.0.0.1:4562", 20000: "127.0.0.1:30000", 20001: "192.168.1.20:20001"}
	if !maps.Equal(got, want) {
		t.Errorf("SelfPorts = %v, want %v", got, want)
	}
}

// detectSelf tells the emulator's own container on a bridge network and
// on the host's network, both found, from a container it can't find
// (#502).
func TestDetectSelfFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/bridged/json"):
			_, _ = io.WriteString(w, `{"Id":"bridged1","HostConfig":{"NetworkMode":"bridge"}}`)
		case strings.HasSuffix(r.URL.Path, "/containers/hostnet/json"):
			_, _ = io.WriteString(w, `{"Id":"hostnet1","HostConfig":{"NetworkMode":"host"}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	for _, tc := range []struct {
		name, self string
		want       string
		found      bool
	}{
		{"bridge network", "bridged", "bridged1", true},
		{"host network", "hostnet", "", true},
		{"not found", "gone", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(SelfEnv, tc.self)
			cl, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			cl.detectSelf(context.Background())
			if cl.Self() != tc.want || cl.SelfFound() != tc.found {
				t.Errorf("Self, SelfFound = %q, %v; want %q, %v", cl.Self(), cl.SelfFound(), tc.want, tc.found)
			}
		})
	}
}

// On a VPC container network the emulator's own container takes the
// highest free address below the second-to-last one, which the NAT egress
// proxy holds; elsewhere it takes the second-to-last (#806).
func TestAttachSelfVPC(t *testing.T) {
	asked := map[string]string{}
	cl := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/networks/vpc"):
			_, _ = io.WriteString(w, `{"Id":"v","Name":"vpc","Labels":{"gcpemu.role":"vpc"},
				"IPAM":{"Config":[{"Subnet":"10.89.9.0/24","Gateway":"10.89.9.1"}]},
				"Containers":{"a":{"IPv4Address":"10.89.9.253/24"}}}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/networks/svc"):
			_, _ = io.WriteString(w, `{"Id":"s","Name":"svc","Labels":{"gcpemu.role":"services"},
				"IPAM":{"Config":[{"Subnet":"10.9.0.0/24","Gateway":"10.9.0.1"}]}}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/connect"):
			var body struct {
				EndpointConfig struct{ IPAMConfig struct{ IPv4Address string } }
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			parts := strings.Split(r.URL.Path, "/")
			asked[parts[len(parts)-2]] = body.EndpointConfig.IPAMConfig.IPv4Address
		default:
			w.WriteHeader(404)
		}
	})
	cl.self = "me"
	for _, n := range []string{"vpc", "svc"} {
		if err := cl.AttachSelf(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{"vpc": "10.89.9.252", "svc": "10.9.0.254"}
	if !maps.Equal(asked, want) {
		t.Errorf("AttachSelf asked for %v; want %v (10.89.9.254 is the NAT egress proxy's, .253 is held)", asked, want)
	}
}
