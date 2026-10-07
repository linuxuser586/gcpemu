package certs_test

import (
	"crypto/x509"
	"testing"

	"cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// createSelfManaged creates a self-managed certificate for names.
func (e *env) createSelfManaged(id string, ca *testCA, names ...string) *certificatemanagerpb.Certificate {
	e.t.Helper()
	chain, key := ca.leaf(e.t, x509.ExtKeyUsageServerAuth, names...)
	op, err := e.cm.CreateCertificate(e.ctx, &certificatemanagerpb.CreateCertificateRequest{
		Parent: globalLoc, CertificateId: id,
		Certificate: &certificatemanagerpb.Certificate{
			Description: "test",
			Type: &certificatemanagerpb.Certificate_SelfManaged{SelfManaged: &certificatemanagerpb.Certificate_SelfManagedCertificate{
				PemCertificate: chain, PemPrivateKey: key,
			}},
		},
	})
	if err != nil {
		e.t.Fatalf("CreateCertificate: %v", err)
	}
	c, err := op.Wait(e.ctx)
	if err != nil {
		e.t.Fatalf("CreateCertificate wait: %v", err)
	}
	return c
}

func (e *env) createEntry(mapID, id, hostname string, primary bool, certs ...string) {
	e.t.Helper()
	ent := &certificatemanagerpb.CertificateMapEntry{Certificates: certs}
	if primary {
		ent.Match = &certificatemanagerpb.CertificateMapEntry_Matcher_{Matcher: certificatemanagerpb.CertificateMapEntry_PRIMARY}
	} else {
		ent.Match = &certificatemanagerpb.CertificateMapEntry_Hostname{Hostname: hostname}
	}
	op, err := e.cm.CreateCertificateMapEntry(e.ctx, &certificatemanagerpb.CreateCertificateMapEntryRequest{
		Parent: globalLoc + "/certificateMaps/" + mapID, CertificateMapEntryId: id, CertificateMapEntry: ent,
	})
	if err != nil {
		e.t.Fatalf("CreateCertificateMapEntry %s: %v", id, err)
	}
	if _, err := op.Wait(e.ctx); err != nil {
		e.t.Fatalf("CreateCertificateMapEntry wait: %v", err)
	}
}

func TestCertificateMapGRPC(t *testing.T) {
	e := start(t)
	ca := newTestCA(t, "test root")
	c := e.createSelfManaged("exact", ca, "www.example.com")
	if c.GetName() != globalLoc+"/certificates/exact" || c.GetPemCertificate() == "" || len(c.GetSanDnsnames()) != 1 ||
		c.GetExpireTime() == nil || c.GetSelfManaged() != nil || c.GetCreateTime() == nil {
		t.Fatalf("certificate: %v", c)
	}
	e.createSelfManaged("wild", ca, "*.example.com")
	e.createSelfManaged("prim", ca, "primary.test")

	if _, err := e.cm.CreateCertificate(e.ctx, &certificatemanagerpb.CreateCertificateRequest{
		Parent: globalLoc, CertificateId: "bad",
		Certificate: &certificatemanagerpb.Certificate{Type: &certificatemanagerpb.Certificate_SelfManaged{SelfManaged: &certificatemanagerpb.Certificate_SelfManagedCertificate{PemCertificate: "x", PemPrivateKey: "y"}}},
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad PEM: %v", err)
	}

	op, err := e.cm.CreateCertificateMap(e.ctx, &certificatemanagerpb.CreateCertificateMapRequest{
		Parent: globalLoc, CertificateMapId: "m", CertificateMap: &certificatemanagerpb.CertificateMap{Labels: map[string]string{"a": "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.createEntry("m", "e-exact", "www.example.com", false, globalLoc+"/certificates/exact")
	e.createEntry("m", "e-wild", "*.example.com", false, globalLoc+"/certificates/wild")
	e.createEntry("m", "e-prim", "", true, "//certificatemanager.googleapis.com/"+globalLoc+"/certificates/prim")

	// Duplicate hostname in the same map.
	if _, err := e.cm.CreateCertificateMapEntry(e.ctx, &certificatemanagerpb.CreateCertificateMapEntryRequest{
		Parent: globalLoc + "/certificateMaps/m", CertificateMapEntryId: "dup",
		CertificateMapEntry: &certificatemanagerpb.CertificateMapEntry{Match: &certificatemanagerpb.CertificateMapEntry_Hostname{Hostname: "www.example.com"}, Certificates: []string{globalLoc + "/certificates/exact"}},
	}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate hostname: %v", err)
	}

	ent, err := e.cm.GetCertificateMapEntry(e.ctx, &certificatemanagerpb.GetCertificateMapEntryRequest{Name: globalLoc + "/certificateMaps/m/certificateMapEntries/e-prim"})
	if err != nil || ent.GetState() != certificatemanagerpb.ServingState_ACTIVE || ent.GetMatcher() != certificatemanagerpb.CertificateMapEntry_PRIMARY ||
		ent.GetCertificates()[0] != globalLoc+"/certificates/prim" {
		t.Fatalf("entry: %v %v", ent, err)
	}

	mapName := "//certificatemanager.googleapis.com/" + globalLoc + "/certificateMaps/m"
	for sni, want := range map[string]string{
		"www.example.com": "www.example.com",
		"WWW.Example.COM": "www.example.com",
		"api.example.com": "*.example.com",
		"a.b.example.com": "primary.test",
		"other.test":      "primary.test",
		"":                "primary.test",
	} {
		got, err := e.mgr().MapCertificate(e.ctx, mapName, sni)
		if err != nil || got == nil || got.Leaf.DNSNames[0] != want {
			t.Errorf("MapCertificate(%q) = %v, %v; want %s", sni, got, err, want)
		}
	}
	if _, err := e.mgr().MapCertificate(e.ctx, globalLoc+"/certificateMaps/nope", "x"); status.Code(err) != codes.NotFound {
		t.Fatalf("missing map: %v", err)
	}

	// In use: the certificate and the map cannot be deleted.
	if _, err := e.cm.DeleteCertificate(e.ctx, &certificatemanagerpb.DeleteCertificateRequest{Name: globalLoc + "/certificates/exact"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete used cert: %v", err)
	}
	if _, err := e.cm.DeleteCertificateMap(e.ctx, &certificatemanagerpb.DeleteCertificateMapRequest{Name: globalLoc + "/certificateMaps/m"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete non-empty map: %v", err)
	}

	// Update the PRIMARY entry to another certificate; the cache follows.
	uop, err := e.cm.UpdateCertificateMapEntry(e.ctx, &certificatemanagerpb.UpdateCertificateMapEntryRequest{
		CertificateMapEntry: &certificatemanagerpb.CertificateMapEntry{Name: globalLoc + "/certificateMaps/m/certificateMapEntries/e-prim", Certificates: []string{globalLoc + "/certificates/exact"}},
		UpdateMask:          &fieldmaskpb.FieldMask{Paths: []string{"certificates"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uop.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.mgr().MapCertificate(e.ctx, mapName, "other.test"); got == nil || got.Leaf.DNSNames[0] != "www.example.com" {
		t.Fatalf("after update: %v", got)
	}

	var n int
	it := e.cm.ListCertificateMapEntries(e.ctx, &certificatemanagerpb.ListCertificateMapEntriesRequest{Parent: globalLoc + "/certificateMaps/m", PageSize: 2})
	for {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("listed %d entries", n)
	}

	// Delete everything in dependency order.
	for _, id := range []string{"e-exact", "e-wild", "e-prim"} {
		dop, err := e.cm.DeleteCertificateMapEntry(e.ctx, &certificatemanagerpb.DeleteCertificateMapEntryRequest{Name: globalLoc + "/certificateMaps/m/certificateMapEntries/" + id})
		if err != nil {
			t.Fatal(err)
		}
		if err := dop.Wait(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	dop, err := e.cm.DeleteCertificateMap(e.ctx, &certificatemanagerpb.DeleteCertificateMapRequest{Name: globalLoc + "/certificateMaps/m"})
	if err != nil {
		t.Fatal(err)
	}
	if err := dop.Wait(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cm.GetCertificateMap(e.ctx, &certificatemanagerpb.GetCertificateMapRequest{Name: globalLoc + "/certificateMaps/m"}); status.Code(err) != codes.NotFound {
		t.Fatalf("deleted map: %v", err)
	}
}
