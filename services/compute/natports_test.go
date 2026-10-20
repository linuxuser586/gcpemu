package compute

import (
	"fmt"
	"strings"
	"testing"

	computev1 "google.golang.org/api/compute/v1"

	"github.com/linuxuser586/gcpemu/internal/store"
)

// layout renders an allocation as "ip active|drain" lines plus the extra
// NAT IPs it needs.
func layout(a natPorts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "per=%d extra=%d\n", a.perVM, a.extraIPs)
	for _, vm := range a.vms {
		fmt.Fprintf(&b, "%s %s|%s\n", vm.ip, strings.Join(vm.active(), ","), strings.Join(vm.drained(), ","))
	}
	return b.String()
}

func vms(ips ...string) []natVM {
	var out []natVM
	for _, ip := range ips {
		out = append(out, natVM{owner: "vm-" + ip, ip: ip})
	}
	return out
}

// TestAllocateNatPorts is FR-NAT-003: each workload a NAT serves gets a
// block of minPortsPerVm ports, laid out in address order over the NAT
// IPs' ports 1024-65535; workloads that do not fit get none, and the
// allocation reports how many more NAT IPs it needs.
func TestAllocateNatPorts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		nat        computev1.RouterNat
		vms        []natVM
		ips, drain []string
		want       string
	}{
		{
			name: "default 64 ports per VM",
			vms:  vms("10.0.0.2", "10.0.0.3"),
			ips:  []string{"203.0.113.5"},
			want: "per=64 extra=0\n10.0.0.2 203.0.113.5:1024-1087|\n10.0.0.3 203.0.113.5:1088-1151|\n",
		},
		{
			name: "dynamic port allocation starts at 32",
			nat:  computev1.RouterNat{EnableDynamicPortAllocation: true},
			vms:  vms("10.0.0.2"),
			ips:  []string{"203.0.113.5"},
			want: "per=32 extra=0\n10.0.0.2 203.0.113.5:1024-1055|\n",
		},
		{
			name: "blocks spill onto the next NAT IP; the rest are exhausted",
			nat:  computev1.RouterNat{MinPortsPerVm: 32768},
			vms:  vms("10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"),
			ips:  []string{"203.0.113.5", "203.0.113.6"},
			want: "per=32768 extra=2\n10.0.0.2 203.0.113.5:1024-33791|\n10.0.0.3 203.0.113.6:1024-33791|\n10.0.0.4 |\n10.0.0.5 |\n",
		},
		{
			name: "a block never exceeds one NAT IP",
			nat:  computev1.RouterNat{MinPortsPerVm: 65536},
			vms:  vms("10.0.0.2"),
			ips:  []string{"203.0.113.5"},
			want: "per=64512 extra=0\n10.0.0.2 203.0.113.5:1024-65535|\n",
		},
		{
			name: "no NAT IPs",
			vms:  vms("10.0.0.2", "10.0.0.3"),
			want: "per=64 extra=1\n10.0.0.2 |\n10.0.0.3 |\n",
		},
		{
			name:  "drain IPs keep the blocks laid out on them",
			vms:   vms("10.0.0.2", "10.0.0.3"),
			ips:   []string{"203.0.113.6"},
			drain: []string{"203.0.113.5"},
			want:  "per=64 extra=0\n10.0.0.2 203.0.113.6:1024-1087|203.0.113.5:1024-1087\n10.0.0.3 203.0.113.6:1088-1151|203.0.113.5:1088-1151\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := layout(allocateNatPorts(&tc.nat, tc.vms, tc.ips, tc.drain)); got != tc.want {
				t.Fatalf("allocation:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// setNat changes the fixture's NAT and adds workload vm-2 at 10.0.0.3.
func setNat(t *testing.T, s *Service, f func(*computev1.RouterNat)) {
	t.Helper()
	err := s.env.Store.Update(func(tx store.Tx) error {
		rt, _ := get[computev1.Router](tx, nsRouters, tRouter)
		f(rt.Nats[0])
		if err := store.PutJSON(tx, nsRouters, tRouter, rt); err != nil {
			return err
		}
		vm := "projects/p/zones/us-central1-a/instances/vm-2"
		if err := tx.Put(nsIPAlloc, "gcpemu-nodes/ip/10.0.0.3", []byte(vm)); err != nil {
			return err
		}
		return tx.Put(nsIPAlloc, "gcpemu-nodes/owner/"+vm, []byte("10.0.0.3"))
	})
	if err != nil {
		t.Fatal(err)
	}
}

// manualNatIP reserves 203.0.113.9 and makes it the NAT's only IP.
func manualNatIP(t *testing.T, s *Service) func(*computev1.RouterNat) {
	t.Helper()
	ap := "projects/p/regions/us-central1/addresses/nat-ip"
	err := s.env.Store.Update(func(tx store.Tx) error {
		return store.PutJSON(tx, nsAddresses, ap, &computev1.Address{Name: "nat-ip", Address: "203.0.113.9", AddressType: "EXTERNAL", SelfLink: link(ap)})
	})
	if err != nil {
		t.Fatal(err)
	}
	return func(nat *computev1.RouterNat) {
		nat.NatIpAllocateOption, nat.NatIps = "MANUAL_ONLY", []string{link(ap)}
	}
}

// TestNatRulesPortBlocks is FR-NAT-003 in the proxy: each workload is
// translated onto its own port block, endpoint-independent mapping caps
// its concurrent connections at the block, dynamic port allocation lets
// it grow to maxPortsPerVm, and a workload left without ports is dropped.
func TestNatRulesPortBlocks(t *testing.T) {
	snat := func(ip, ports string) string {
		return "-A GCPEMU-POST -s " + ip + "/32 -o $EXT -p tcp -j SNAT --to-source 172.22.0.2:" + ports + "\n" +
			"-A GCPEMU-POST -s " + ip + "/32 -o $EXT -p udp -j SNAT --to-source 172.22.0.2:" + ports + "\n"
	}
	limit := func(ip string, n int) string {
		return fmt.Sprintf("iptables -I GCPEMU-FWD 3 -s %s/32 -o \"$EXT\" -m conntrack --ctstate NEW -m connlimit --connlimit-above %d --connlimit-mask 32 -j GCPEMU-DROP", ip, n)
	}
	drop := func(ip string) string { return "-A GCPEMU-FWD -s " + ip + "/32 -o $EXT -j GCPEMU-DROP\n" }
	for _, tc := range []struct {
		name      string
		nat       func(*Service) func(*computev1.RouterNat)
		want, not []string
	}{
		{
			name: "endpoint-dependent blocks",
			nat:  func(*Service) func(*computev1.RouterNat) { return func(*computev1.RouterNat) {} },
			want: []string{snat("10.0.0.2", "1024-1087") + snat("10.0.0.3", "1088-1151") + "-A GCPEMU-POST -o $EXT -j MASQUERADE\n"},
			not:  []string{"connlimit", drop("10.0.0.2"), drop("10.0.0.3")},
		},
		{
			name: "endpoint-independent mapping",
			nat: func(*Service) func(*computev1.RouterNat) {
				return func(n *computev1.RouterNat) { n.EnableEndpointIndependentMapping, n.MinPortsPerVm = true, 128 }
			},
			want: []string{snat("10.0.0.2", "1024-1151"), limit("10.0.0.2", 128), limit("10.0.0.3", 128)},
		},
		{
			name: "dynamic port allocation",
			nat: func(*Service) func(*computev1.RouterNat) {
				return func(n *computev1.RouterNat) {
					n.EnableDynamicPortAllocation, n.MinPortsPerVm, n.MaxPortsPerVm = true, 64, 256
				}
			},
			want: []string{snat("10.0.0.2", "1024-1279") + snat("10.0.0.3", "1088-1343")},
			not:  []string{"connlimit"},
		},
		{
			name: "exhausted",
			nat: func(s *Service) func(*computev1.RouterNat) {
				manual := manualNatIP(t, s)
				return func(n *computev1.RouterNat) { manual(n); n.MinPortsPerVm = 64512 }
			},
			want: []string{snat("10.0.0.2", "1024-65535"), drop("10.0.0.3") + "-A GCPEMU-FWD -s 10.0.0.0/24 -o $EXT -j ACCEPT\n"},
			not:  []string{"-s 10.0.0.3/32 -o $EXT -p tcp", drop("10.0.0.2")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := natTestService(t, nil)
			setNat(t, s, tc.nat(s))
			g := &egressProxy{s: s, network: tNet, extIP: "172.22.0.2", extGW: "172.22.0.1"}
			rules, err := s.natRules(g, s.netsOf(tNet))
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(rules, w) {
					t.Errorf("missing:\n%s", w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(rules, n) {
					t.Errorf("unexpected:\n%s", n)
				}
			}
			if t.Failed() {
				t.Logf("rules:\n%s", rules)
			}
		})
	}
}
