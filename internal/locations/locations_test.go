package locations

import "testing"

func TestCatalogue(t *testing.T) {
	if !IsRegion("us-central1") || IsRegion("us-central9") || !IsZone("us-central1-f") || IsZone("us-central1-d") || !IsZone("europe-west1-b") || IsZone("europe-west1-a") {
		t.Fatal("catalogue lookups")
	}
	if r, ok := ZoneRegion("asia-east1-a"); !ok || r != "asia-east1" {
		t.Fatal(r, ok)
	}
	if r, ok := Lookup("us-central1"); !ok || r.AutoCIDR != "10.128.0.0/20" || len(r.ZoneNames()) != 4 {
		t.Fatalf("%+v", r)
	}
	if r, ok := RegionOf("us-east1-b"); !ok || r != "us-east1" {
		t.Fatal(r, ok)
	}
	seen := map[string]string{}
	for _, r := range Regions() {
		if o, dup := seen[r.AutoCIDR]; dup {
			t.Fatalf("auto range %s shared by %s and %s", r.AutoCIDR, o, r.Name)
		}
		seen[r.AutoCIDR] = r.Name
	}
}
