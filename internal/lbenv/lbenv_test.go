package lbenv_test

import (
	"maps"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/lbenv"
)

// A forwarding rule gets its qualified variable, and the short one while
// its name is unique (ADR 0013).
func TestForwardingRuleVars(t *testing.T) {
	got, withheld := lbenv.Vars(map[string]string{
		"lb:p/us-central1/web": "127.0.0.2:8080",
		"gateway":              "127.0.0.1:4000",
	})
	want := map[string]string{"GCPEMU_LB_P_US_CENTRAL1_WEB": "127.0.0.2:8080", "GCPEMU_LB_WEB": "127.0.0.2:8080"}
	if !maps.Equal(got, want) || len(withheld) != 0 {
		t.Errorf("Vars = %v, withheld %v; want %v", got, withheld, want)
	}
}

// A Linked Instance's rule named like one of this Instance's withholds the
// short variable, never pointing it at either, and both qualified ones
// are printed (#715).
func TestLinkedRuleSharingAName(t *testing.T) {
	got, withheld := lbenv.Vars(map[string]string{
		"lb:p/us-central1/web":                         "127.0.0.2:8080",
		lbenv.Linked("lb:q/us-central1/web", "office"): "192.168.1.5:8080",
	})
	want := map[string]string{"GCPEMU_LB_P_US_CENTRAL1_WEB": "127.0.0.2:8080", "GCPEMU_LB_Q_US_CENTRAL1_WEB": "192.168.1.5:8080"}
	if !maps.Equal(got, want) {
		t.Errorf("Vars = %v, want %v", got, want)
	}
	if len(withheld) != 1 || withheld[0].Name != "GCPEMU_LB_WEB" || len(withheld[0].Keys) != 2 {
		t.Errorf("withheld = %+v, want GCPEMU_LB_WEB from both rules", withheld)
	}
}

// A LoadBalancer Service port gets GCPEMU_GKE_<CLUSTER>_<NAMESPACE>_<SERVICE>_<PORT>,
// withheld when a cluster of the same name on another Instance would print
// it too (#715).
func TestLoadBalancerServiceVars(t *testing.T) {
	got, _ := lbenv.Vars(map[string]string{lbenv.Linked("gke:dev/default/web/80", "office"): "192.168.1.5:80"})
	if want := map[string]string{"GCPEMU_GKE_DEV_DEFAULT_WEB_80": "192.168.1.5:80"}; !maps.Equal(got, want) {
		t.Errorf("Vars of a linked Service = %v, want %v", got, want)
	}
	got, withheld := lbenv.Vars(map[string]string{
		"gke:dev/default/web/80":                         "127.0.0.3:80",
		lbenv.Linked("gke:dev/default/web/80", "office"): "192.168.1.5:80",
		"gke:dev/default/api/81":                         "127.0.0.3:81",
	})
	if want := map[string]string{"GCPEMU_GKE_DEV_DEFAULT_API_81": "127.0.0.3:81"}; !maps.Equal(got, want) {
		t.Errorf("Vars of a cluster name on two Instances = %v, want %v", got, want)
	}
	if len(withheld) != 1 || withheld[0].Name != "GCPEMU_GKE_DEV_DEFAULT_WEB_80" {
		t.Errorf("withheld = %+v, want GCPEMU_GKE_DEV_DEFAULT_WEB_80", withheld)
	}
}
