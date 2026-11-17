package gke

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

// endpointSlices decodes EndpointSlice JSON objects.
func endpointSlices(t *testing.T, objs ...string) []kubeEndpointSlice {
	t.Helper()
	var out []kubeEndpointSlice
	for _, o := range objs {
		var sl kubeEndpointSlice
		if err := json.Unmarshal([]byte(o), &sl); err != nil {
			t.Fatal(err)
		}
		out = append(out, sl)
	}
	return out
}

// hostTestService decodes a Service default/web with the spec JSON.
func hostTestService(t *testing.T, spec string) *kubeService {
	t.Helper()
	var svc kubeService
	if err := json.Unmarshal([]byte(`{"metadata":{"namespace":"default","name":"web"},"spec":`+spec+`}`), &svc); err != nil {
		t.Fatal(err)
	}
	return &svc
}

// A host client's connection goes to an endpoint the emulator chooses as
// kube-proxy would (ADR 0087): a ready one, or a serving terminating one
// when none is ready, on the target port its EndpointSlice names for the
// Service port. Only a pod in the cluster's pod range, on a known node,
// is reached from the Host address; any other endpoint is left to the
// ClusterIP.
func TestChooseHostEndpoint(t *testing.T) {
	_, pods, _ := net.ParseCIDR("10.4.0.0/16")
	svc := hostTestService(t, `{"type":"ClusterIP","clusterIP":"10.8.0.10","ports":[{"name":"grpc","port":9082,"targetPort":"grpc"},{"name":"http","port":80}]}`)
	first := func(int) int { return 0 }
	now := time.Unix(1000, 0)
	for _, c := range []struct {
		name   string
		slices []string
		port   int
		want   hostEndpoint
		ok     bool
	}{
		{
			name: "ready pod, named target port",
			slices: []string{`{"addressType":"IPv4","ports":[{"name":"http","port":8080},{"name":"grpc","port":19082}],
				"endpoints":[{"addresses":["10.4.1.5"],"conditions":{"ready":true},"nodeName":"n2"}]}`},
			port: 9082, want: hostEndpoint{addr: "10.4.1.5:19082", node: "n2"}, ok: true,
		},
		{
			name: "per-slice target ports",
			slices: []string{
				`{"addressType":"IPv4","ports":[{"name":"grpc","port":1}],"endpoints":[{"addresses":["10.4.1.5"],"conditions":{"ready":false},"nodeName":"n2"}]}`,
				`{"addressType":"IPv4","ports":[{"name":"grpc","port":2}],"endpoints":[{"addresses":["10.4.2.5"],"conditions":{"ready":true},"nodeName":"n3"}]}`,
			},
			port: 9082, want: hostEndpoint{addr: "10.4.2.5:2", node: "n3"}, ok: true,
		},
		{
			name: "unknown readiness counts as ready",
			slices: []string{`{"addressType":"IPv4","ports":[{"name":"http","port":8080}],
				"endpoints":[{"addresses":["10.4.1.5"],"conditions":{},"nodeName":"n2"}]}`},
			port: 80, want: hostEndpoint{addr: "10.4.1.5:8080", node: "n2"}, ok: true,
		},
		{
			name: "serving terminating when none is ready",
			slices: []string{`{"addressType":"IPv4","ports":[{"name":"http","port":8080}],"endpoints":[
				{"addresses":["10.4.1.5"],"conditions":{"ready":false,"serving":false,"terminating":true},"nodeName":"n1"},
				{"addresses":["10.4.1.6"],"conditions":{"ready":false,"serving":true,"terminating":true},"nodeName":"n2"}]}`},
			port: 80, want: hostEndpoint{addr: "10.4.1.6:8080", node: "n2"}, ok: true,
		},
		{
			name: "ready preferred over serving terminating",
			slices: []string{`{"addressType":"IPv4","ports":[{"name":"http","port":8080}],"endpoints":[
				{"addresses":["10.4.1.6"],"conditions":{"ready":false,"serving":true,"terminating":true},"nodeName":"n2"},
				{"addresses":["10.4.1.7"],"conditions":{"ready":true},"nodeName":"n3"}]}`},
			port: 80, want: hostEndpoint{addr: "10.4.1.7:8080", node: "n3"}, ok: true,
		},
		{
			name:   "nothing serving",
			slices: []string{`{"addressType":"IPv4","ports":[{"name":"http","port":8080}],"endpoints":[{"addresses":["10.4.1.5"],"conditions":{"ready":false},"nodeName":"n2"}]}`},
			port:   80,
		},
		{
			name: "hostNetwork pod, outside the pod range",
			slices: []string{`{"addressType":"IPv4","ports":[{"name":"http","port":8080}],
				"endpoints":[{"addresses":["10.0.0.5"],"conditions":{"ready":true},"nodeName":"n2"}]}`},
			port: 80,
		},
		{
			name: "selectorless, no node",
			slices: []string{`{"addressType":"IPv4","ports":[{"name":"http","port":8080}],
				"endpoints":[{"addresses":["10.4.1.5"],"conditions":{"ready":true}}]}`},
			port: 80,
		},
		{
			name:   "IPv6 slice",
			slices: []string{`{"addressType":"IPv6","ports":[{"name":"http","port":8080}],"endpoints":[{"addresses":["fd00::5"],"conditions":{"ready":true},"nodeName":"n2"}]}`},
			port:   80,
		},
		{name: "no slices", port: 80},
		{
			name:   "unknown Service port",
			slices: []string{`{"addressType":"IPv4","ports":[{"name":"http","port":8080}],"endpoints":[{"addresses":["10.4.1.5"],"conditions":{"ready":true},"nodeName":"n2"}]}`},
			port:   81,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := chooseHostEndpoint(svc, c.port, endpointSlices(t, c.slices...), pods, &hostAffinity{}, now, first)
			if ok != c.ok || got != c.want {
				t.Errorf("chooseHostEndpoint = %+v, %v; want %+v, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

// Without session affinity each connection draws an endpoint; with
// sessionAffinity ClientIP every host client is one client, and stays on
// its endpoint while it serves and the affinity has not timed out.
func TestChooseHostEndpointAffinity(t *testing.T) {
	_, pods, _ := net.ParseCIDR("10.4.0.0/16")
	two := endpointSlices(t, `{"addressType":"IPv4","ports":[{"name":"","port":8080}],"endpoints":[
		{"addresses":["10.4.1.5"],"conditions":{"ready":true},"nodeName":"n1"},
		{"addresses":["10.4.2.5"],"conditions":{"ready":true},"nodeName":"n2"}]}`)
	one := endpointSlices(t, `{"addressType":"IPv4","ports":[{"name":"","port":8080}],"endpoints":[
		{"addresses":["10.4.2.5"],"conditions":{"ready":true},"nodeName":"n2"}]}`)
	now := time.Unix(1000, 0)
	draw := func(i int) func(int) int { return func(int) int { return i } }

	none := hostTestService(t, `{"type":"ClusterIP","clusterIP":"10.8.0.10","ports":[{"port":80}]}`)
	aff := &hostAffinity{}
	for i, want := range []string{"10.4.1.5:8080", "10.4.2.5:8080"} {
		if got, _ := chooseHostEndpoint(none, 80, two, pods, aff, now, draw(i)); got.addr != want {
			t.Errorf("no affinity, draw %d: %s, want %s", i, got.addr, want)
		}
	}

	sticky := hostTestService(t, `{"type":"ClusterIP","clusterIP":"10.8.0.10","ports":[{"port":80}],
		"sessionAffinity":"ClientIP","sessionAffinityConfig":{"clientIP":{"timeoutSeconds":60}}}`)
	aff = &hostAffinity{}
	if got, _ := chooseHostEndpoint(sticky, 80, two, pods, aff, now, draw(0)); got.addr != "10.4.1.5:8080" {
		t.Fatalf("first: %s", got.addr)
	}
	if got, _ := chooseHostEndpoint(sticky, 80, two, pods, aff, now.Add(59*time.Second), draw(1)); got.addr != "10.4.1.5:8080" {
		t.Errorf("within the timeout: %s, want the same endpoint", got.addr)
	}
	// Each connection renews the affinity.
	if got, _ := chooseHostEndpoint(sticky, 80, two, pods, aff, now.Add(118*time.Second), draw(1)); got.addr != "10.4.1.5:8080" {
		t.Errorf("renewed: %s, want the same endpoint", got.addr)
	}
	if got, _ := chooseHostEndpoint(sticky, 80, two, pods, aff, now.Add(179*time.Second), draw(1)); got.addr != "10.4.2.5:8080" {
		t.Errorf("after the timeout: %s, want a new draw", got.addr)
	}
	// An endpoint that stops serving loses the affinity.
	aff = &hostAffinity{}
	_, _ = chooseHostEndpoint(sticky, 80, two, pods, aff, now, draw(0))
	if got, _ := chooseHostEndpoint(sticky, 80, one, pods, aff, now.Add(time.Second), draw(0)); got.addr != "10.4.2.5:8080" {
		t.Errorf("endpoint gone: %s", got.addr)
	}

	// The default timeout is kube-proxy's, three hours.
	dflt := hostTestService(t, `{"type":"ClusterIP","clusterIP":"10.8.0.10","ports":[{"port":80}],"sessionAffinity":"ClientIP"}`)
	aff = &hostAffinity{}
	_, _ = chooseHostEndpoint(dflt, 80, two, pods, aff, now, draw(0))
	if got, _ := chooseHostEndpoint(dflt, 80, two, pods, aff, now.Add(3*time.Hour-time.Second), draw(1)); got.addr != "10.4.1.5:8080" {
		t.Errorf("default timeout: %s, want the same endpoint", got.addr)
	}
}
