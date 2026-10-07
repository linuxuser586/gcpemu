package gke

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
)

func TestInjectPatch(t *testing.T) {
	parse := func(s string) *admitPod {
		var p admitPod
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			t.Fatal(err)
		}
		return &p
	}
	render := func(ops []patchOp) string {
		b, _ := json.Marshal(ops)
		return string(b)
	}

	// A bare pod gets the volume, both mounts and every variable.
	ops := injectPatch(parse(`{"spec":{"containers":[{"name":"a"}],"initContainers":[{"name":"i","env":[{"name":"X"}]}]}}`))
	got := render(ops)
	for _, want := range []string{
		`{"op":"add","path":"/spec/volumes","value":[{"configMap":{"items":[{"key":"ca-certificates.crt","path":"ca-certificates.crt"}],"name":"gcpemu-ca-bundle","optional":true},"name":"gcpemu-ca-bundle"}]}`,
		`"path":"/spec/containers/0/volumeMounts","value":[{"mountPath":"/etc/gcpemu/certs","name":"gcpemu-ca-bundle","readOnly":true},{"mountPath":"/etc/ssl/certs/ca-certificates.crt","name":"gcpemu-ca-bundle","readOnly":true,"subPath":"ca-certificates.crt"}]`,
		`"path":"/spec/containers/0/env","value":[{"name":"SSL_CERT_FILE","value":"/etc/gcpemu/certs/ca-certificates.crt"}`,
		`{"name":"GCE_METADATA_HOST","value":"169.254.169.254"}`,
		`{"op":"add","path":"/spec/initContainers/0/env/-","value":{"name":"SSL_CERT_FILE"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("patch lacks %s\n%s", want, got)
		}
	}

	// Existing mounts and variables are respected.
	got = render(injectPatch(parse(`{"spec":{"volumes":[{"name":"v"}],"containers":[{"name":"a",
		"volumeMounts":[{"name":"v","mountPath":"/etc/ssl/certs"}],"env":[{"name":"SSL_CERT_FILE"}]}]}}`)))
	if strings.Contains(got, `"/etc/ssl/certs/ca-certificates.crt"`) || strings.Contains(got, `"SSL_CERT_FILE"`) ||
		!strings.Contains(got, `"path":"/spec/volumes/-"`) || !strings.Contains(got, `"REQUESTS_CA_BUNDLE"`) {
		t.Errorf("patch = %s", got)
	}

	// Opt-out and idempotence.
	if ops := injectPatch(parse(`{"metadata":{"labels":{"gcpemu.dev/inject":"disabled"}},"spec":{"containers":[{}]}}`)); ops != nil {
		t.Errorf("opted-out pod patched: %v", ops)
	}
	if ops := injectPatch(parse(`{"spec":{"volumes":[{"name":"gcpemu-ca-bundle"}],"containers":[{}]}}`)); ops != nil {
		t.Errorf("injected pod patched again: %v", ops)
	}
}

func TestNodeDNSAnswer(t *testing.T) {
	ip := net.ParseIP(metadataIP)
	ans := nodeDNSAnswer(nodeConfig{Frontend: "10.0.0.1:443", FrontendHosts: []string{"storage.googleapis.com"}}, ip)
	for name, want := range map[string]bool{
		"metadata.google.internal": true, "storage.googleapis.com": true, "us-docker.pkg.dev": true,
		"logging.googleapis.com": false, "example.com": false,
	} {
		if got := ans(name) != nil; got != want {
			t.Errorf("%s answered = %v, want %v", name, got, want)
		}
	}
	// Without a frontend only the metadata names are answered.
	ans = nodeDNSAnswer(nodeConfig{FrontendHosts: []string{"storage.googleapis.com"}}, ip)
	if ans("storage.googleapis.com") != nil || ans("metadata.google.internal") == nil {
		t.Error("frontend names answered without a frontend")
	}
}
