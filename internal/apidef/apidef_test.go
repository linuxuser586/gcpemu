package apidef

import (
	"net/url"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	_ "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	_ "cloud.google.com/go/container/apiv1/containerpb"
	_ "cloud.google.com/go/iam/apiv1/iampb"
	_ "cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	_ "cloud.google.com/go/longrunning/autogen/longrunningpb"
	_ "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	_ "google.golang.org/genproto/googleapis/cloud/location"
)

// TestDrift checks the generated tables against the descriptors compiled
// into the Go modules in go.mod: the googleapis pin and the module
// versions must describe the same services, methods and fields, or a
// version bump updated one without the other.
func TestDrift(t *testing.T) {
	for _, s := range protoServices {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(s.Name))
		if err != nil {
			t.Errorf("service %s (googleapis@%s) is not in the Go modules", s.Name, googleapisCommit[:12])
			continue
		}
		sd := d.(protoreflect.ServiceDescriptor)
		for _, m := range s.Methods {
			md := sd.Methods().ByName(protoreflect.Name(m.Name))
			if md == nil {
				t.Errorf("method %s.%s is not in the Go modules", s.Name, m.Name)
				continue
			}
			if string(md.Input().FullName()) != m.Input || string(md.Output().FullName()) != m.Output {
				t.Errorf("method %s.%s: generated %s → %s, Go modules %s → %s", s.Name, m.Name, m.Input, m.Output, md.Input().FullName(), md.Output().FullName())
			}
		}
	}
	for msg, fields := range protoFields {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(msg))
		if err != nil {
			t.Errorf("message %s is not in the Go modules", msg)
			continue
		}
		for f := range fields {
			if d.(protoreflect.MessageDescriptor).Fields().ByName(protoreflect.Name(f)) == nil {
				t.Errorf("field %s.%s is not in the Go modules", msg, f)
			}
		}
	}
}

func TestPins(t *testing.T) {
	if len(GoogleapisCommit()) != 40 {
		t.Errorf("googleapis commit %q", GoogleapisCommit())
	}
	if !strings.HasPrefix(discoveryModule, "google.golang.org/api@v") {
		t.Errorf("discovery module %q", discoveryModule)
	}
	if Revision("compute", "v1") == "" {
		t.Error("no compute revision")
	}
}

func TestCheckProtoRequired(t *testing.T) {
	const create = "/google.pubsub.v1.Subscriber/CreateSubscription"
	err := CheckProto(create, &pubsubpb.Subscription{Name: "projects/p/subscriptions/s"})
	if err == nil || !strings.Contains(err.Error(), "topic") {
		t.Errorf("missing topic: %v", err)
	}
	if err := CheckProto(create, &pubsubpb.Subscription{Name: "projects/p/subscriptions/s", Topic: "projects/p/topics/t"}); err != nil {
		t.Error(err)
	}
	// Nested: a set message's required fields.
	sub := &pubsubpb.Subscription{Name: "projects/p/subscriptions/s", Topic: "projects/p/topics/t", CloudStorageConfig: &pubsubpb.CloudStorageConfig{}}
	if err := CheckProto(create, sub); err == nil || !strings.Contains(err.Error(), "cloud_storage_config.bucket") {
		t.Errorf("nested: %v", err)
	}
	// A nested resource name is not required on create (AIP-133).
	err = CheckProto("/google.pubsub.v1.SchemaService/CreateSchema", &pubsubpb.CreateSchemaRequest{Parent: "projects/p", Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: "{}"}})
	if err != nil {
		t.Error(err)
	}
	// Unknown methods pass.
	if err := CheckProto("/x.Y/Z", &pubsubpb.Topic{}); err != nil {
		t.Error(err)
	}
}

func TestCheckProtoOutputOnly(t *testing.T) {
	req := &certificatemanagerpb.CreateCertificateRequest{
		Parent:        "projects/p/locations/global",
		CertificateId: "c",
		Certificate: &certificatemanagerpb.Certificate{
			Description:    "d",
			SanDnsnames:    []string{"x.example.com"},
			PemCertificate: "pem",
		},
	}
	if err := CheckProto("/google.cloud.certificatemanager.v1.CertificateManager/CreateCertificate", req); err != nil {
		t.Fatal(err)
	}
	if req.Certificate.SanDnsnames != nil || req.Certificate.PemCertificate != "" {
		t.Errorf("output-only fields kept: %v", req.Certificate)
	}
	if req.Certificate.Description != "d" {
		t.Errorf("input field cleared: %v", req.Certificate)
	}
}

func TestCheckProtoImmutable(t *testing.T) {
	const update = "/google.cloud.certificatemanager.v1.CertificateManager/UpdateDnsAuthorization"
	req := func(paths ...string) proto.Message {
		return &certificatemanagerpb.UpdateDnsAuthorizationRequest{
			DnsAuthorization: &certificatemanagerpb.DnsAuthorization{Name: "projects/p/locations/global/dnsAuthorizations/d", Domain: "example.com"},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: paths},
		}
	}
	if err := CheckProto(update, req("domain")); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("immutable domain: %v", err)
	}
	if err := CheckProto(update, req("description", "labels")); err != nil {
		t.Error(err)
	}
}

func TestTemplateVars(t *testing.T) {
	got := templateVars("/v1/{certificate.name=projects/*/locations/*/certificates/*}:x/{a}")
	if strings.Join(got, ",") != "certificate.name,a" {
		t.Errorf("got %v", got)
	}
}

func TestRESTMatchAndCheck(t *testing.T) {
	a := REST("compute", "v1")
	for _, tc := range []struct{ verb, path, want string }{
		{"POST", "/compute/v1/projects/p/global/networks", "compute.networks.insert"},
		{"GET", "/v1/projects/p/global/networks/n", "compute.networks.get"},
		{"POST", "/compute/v1/projects/p/global/networks/n/addPeering", "compute.networks.addPeering"},
		{"GET", "/compute/v1/projects/p/aggregated/addresses", "compute.addresses.aggregatedList"},
		{"PUT", "/compute/v1/projects/p/global/networks/n", ""},
	} {
		m := a.Match(tc.verb, a.Rel(tc.path))
		got := ""
		if m != nil {
			got = m.ID
		}
		if got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.verb, tc.path, got, tc.want)
		}
	}
	m := a.Match("POST", "projects/p/global/routes")
	err := a.CheckJSON(m, url.Values{}, []byte(`{"name":"r","network":"n"}`))
	if err == nil || !strings.Contains(err.Error(), "'resource.destRange'") {
		t.Errorf("route without destRange: %v", err)
	}
	if err := a.CheckJSON(m, url.Values{}, []byte(`{"name":"r","network":"n","destRange":"0.0.0.0/0","priority":1000}`)); err != nil {
		t.Error(err)
	}
	// Only the methods an annotation names require the field.
	if err := a.CheckJSON(a.Match("PATCH", "projects/p/global/networks/n"), url.Values{}, []byte(`{"mtu":1500}`)); err != nil {
		t.Error(err)
	}
	// A required field of a nested list element.
	m = a.Match("POST", "projects/p/zones/z/instances")
	err = a.CheckJSON(m, url.Values{}, []byte(`{"name":"i","metadata":{"items":[{"key":"a","value":"1"},{"value":"2"}]}}`))
	if err == nil || !strings.Contains(err.Error(), "'resource.metadata.items[1].key'") {
		t.Errorf("metadata item without key: %v", err)
	}
	// A verb suffix (":signBlob") on a variable segment.
	iam := REST("iam", "v1")
	if m := iam.Match("POST", iam.Rel("/v1/projects/p/serviceAccounts/sa:signBlob")); m == nil || m.ID != "iam.projects.serviceAccounts.signBlob" {
		t.Errorf("signBlob: %+v", m)
	}
}
