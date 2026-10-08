package lb_test

import (
	"fmt"
	"sync"
	"testing"

	computev1 "google.golang.org/api/compute/v1"
)

// TestAddressUsersConcurrent: forwarding rules sharing a reserved address
// are created and deleted in parallel, as OpenTofu does; afterwards the
// address lists exactly the remaining users and can be deleted.
func TestAddressUsersConcurrent(t *testing.T) {
	e := start(t)
	c := e.c
	e.do(c.HealthChecks.Insert(proj, &computev1.HealthCheck{Name: "tcp", TcpHealthCheck: &computev1.TCPHealthCheck{Port: 5432}}).Do())
	e.do(c.BackendServices.Insert(proj, &computev1.BackendService{Name: "tcp", LoadBalancingScheme: "EXTERNAL_MANAGED", Protocol: "TCP",
		HealthChecks: []string{"global/healthChecks/tcp"}}).Do())
	e.do(c.TargetTcpProxies.Insert(proj, &computev1.TargetTcpProxy{Name: "tcp", Service: "global/backendServices/tcp"}).Do())
	e.do(c.GlobalAddresses.Insert(proj, &computev1.Address{Name: "shared"}).Do())

	const n = 8
	parallel := func(fn func(i int)) {
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { fn(i) })
		}
		wg.Wait()
	}
	for round := range 5 {
		parallel(func(i int) {
			e.do(c.GlobalForwardingRules.Insert(proj, &computev1.ForwardingRule{Name: fmt.Sprintf("fr-%d", i),
				LoadBalancingScheme: "EXTERNAL_MANAGED", IPAddress: "global/addresses/shared",
				PortRange: fmt.Sprint(5000 + i), Target: "global/targetTcpProxies/tcp"}).Do())
		})
		if a, err := c.GlobalAddresses.Get(proj, "shared").Do(); err != nil || len(a.Users) != n || a.Status != "IN_USE" {
			t.Fatalf("round %d after inserts: users=%d status=%s err=%v", round, len(a.Users), a.Status, err)
		}
		parallel(func(i int) { e.do(c.GlobalForwardingRules.Delete(proj, fmt.Sprintf("fr-%d", i)).Do()) })
		if a, err := c.GlobalAddresses.Get(proj, "shared").Do(); err != nil || len(a.Users) != 0 || a.Status != "RESERVED" {
			t.Fatalf("round %d after deletes: users=%v status=%s err=%v", round, a.Users, a.Status, err)
		}
	}
	e.do(c.GlobalAddresses.Delete(proj, "shared").Do())
}
