package sql

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// candidates stands in for loopCandidates: aliases a, b, or only
// 127.0.0.1 unless another instance holds it.
func candidates(held bool, aliases ...string) func(bool) []string {
	return func(on bool) []string {
		switch {
		case on:
			return aliases
		case held:
			return nil
		}
		return []string{loopbackHost}
	}
}

func errHeld() error { return errNoLoopback("p:r:holder", true) }

func wantPorts(t *testing.T, p *loopPublish, addr string, ports ...int) {
	t.Helper()
	var want []runtime.Port
	for _, port := range ports {
		want = append(want, runtime.Port{HostIP: addr, HostPort: port, ContainerPort: port})
	}
	if p.addr() != addr || !reflect.DeepEqual(p.ports(), want) {
		t.Fatalf("plan publishes %v on %s, want %v on %s", p.ports(), p.addr(), want, addr)
	}
}

// On a VM runtime a Containerized Instance publishes 3307 and 5432 on an
// instance's 127.0.0.x (ADR 0012). A runtime that refuses the loopback alias
// moves it to 127.0.0.1 for good; a taken 3307 moves it to the next
// loopback alias; a
// taken 5432 only drops 5432.
func TestLoopPublish(t *testing.T) {
	t.Run("aliases", func(t *testing.T) {
		p, err := newLoopPublish(true, candidates(false, "127.0.0.7", "127.0.0.8"), errHeld)
		if err != nil {
			t.Fatal(err)
		}
		wantPorts(t, p, "127.0.0.7", 3307, 5432)
		retry, err := p.retry(errors.New("driver failed programming external connectivity: Error starting userland proxy: listen tcp4 127.0.0.7:3307: bind: address already in use"))
		if !retry || err != nil {
			t.Fatalf("taken 3307: retry=%v err=%v", retry, err)
		}
		wantPorts(t, p, "127.0.0.8", 3307, 5432)
		retry, err = p.retry(errors.New("Bind for 127.0.0.8:5432 failed: port is already allocated"))
		if !retry || err != nil {
			t.Fatalf("taken 5432: retry=%v err=%v", retry, err)
		}
		wantPorts(t, p, "127.0.0.8", 3307)
		if p.refused {
			t.Error("refused without a refusal")
		}
		retry, err = p.retry(errors.New("listen tcp4 127.0.0.8:3307: bind: address already in use"))
		if retry || err == nil || !strings.Contains(err.Error(), "no free loopback address") {
			t.Fatalf("no alias left: retry=%v err=%v", retry, err)
		}
	})
	for _, msg := range []string{
		"listen tcp4 127.0.0.7:3307: bind: cannot assign requested address",
		"Ports are not available: exposing port TCP 127.0.0.7:3307 -> 0.0.0.0:0: listen tcp 127.0.0.7:3307: bind: can't assign requested address",
	} {
		t.Run("refused", func(t *testing.T) {
			p, _ := newLoopPublish(true, candidates(false, "127.0.0.7", "127.0.0.8"), errHeld)
			retry, err := p.retry(errors.New(msg))
			if !retry || err != nil {
				t.Fatalf("refused alias: retry=%v err=%v", retry, err)
			}
			if !p.refused {
				t.Error("the refusal is not recorded")
			}
			wantPorts(t, p, loopbackHost, 3307, 5432)
		})
	}
	t.Run("refused with 127.0.0.1 held", func(t *testing.T) {
		p, _ := newLoopPublish(true, candidates(true, "127.0.0.7"), errHeld)
		retry, err := p.retry(errors.New("listen tcp4 127.0.0.7:3307: bind: cannot assign requested address"))
		if retry || err == nil || !strings.Contains(err.Error(), "p:r:holder") {
			t.Fatalf("retry=%v err=%v, want the holder's FAILED_PRECONDITION", retry, err)
		}
	})
	t.Run("127.0.0.1 only", func(t *testing.T) {
		p, err := newLoopPublish(false, candidates(false, "127.0.0.7"), errHeld)
		if err != nil {
			t.Fatal(err)
		}
		wantPorts(t, p, loopbackHost, 3307, 5432)
		// A local PostgreSQL holds 127.0.0.1:5432: connectors still work.
		if retry, err := p.retry(errors.New("listen tcp4 127.0.0.1:5432: bind: address already in use")); !retry || err != nil {
			t.Fatalf("taken 5432: retry=%v err=%v", retry, err)
		}
		wantPorts(t, p, loopbackHost, 3307)
		for _, msg := range []string{"some other failure", "Bind for 127.0.0.1:54321 failed: port is already allocated", "listen tcp4 127.0.0.1:33070: bind: address already in use"} {
			if retry, err := p.retry(errors.New(msg)); retry || err != nil {
				t.Fatalf("unrelated error %q: retry=%v err=%v, want it left to the caller", msg, retry, err)
			}
		}
		retry, err := p.retry(errors.New("Bind for 127.0.0.1:3307 failed: port is already allocated"))
		if retry || err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("taken 3307: retry=%v err=%v", retry, err)
		}
	})
	t.Run("127.0.0.1 held", func(t *testing.T) {
		if _, err := newLoopPublish(false, candidates(true), errHeld); err == nil || !strings.Contains(err.Error(), "p:r:holder") {
			t.Fatalf("err = %v, want the holder's FAILED_PRECONDITION", err)
		}
	})
}

// The message for a Containerized Instance says how to give it aliases.
func TestErrNoLoopback(t *testing.T) {
	host := errNoLoopback("p:r:a", false).Error()
	img := errNoLoopback("p:r:a", true).Error()
	for _, msg := range []string{host, img} {
		if !strings.Contains(msg, "p:r:a") || !strings.Contains(msg, "sudo gcpemu loopback install") {
			t.Errorf("message %q does not name the holder and the fix", msg)
		}
	}
	if !strings.Contains(img, "GCPEMU_LOOPBACK_ALIASES=true") || strings.Contains(host, "GCPEMU_LOOPBACK_ALIASES") {
		t.Errorf("only the Containerized Instance's message names GCPEMU_LOOPBACK_ALIASES:\n%s\n%s", host, img)
	}
}

// `gcpemu doctor` reads each public instance's host address: its loopback
// address where host clients need one, and otherwise its public IP. A
// running instance that needs one and has none is reported missing.
func TestHostsReport(t *testing.T) {
	s := testService(t, config.IAMOff)
	inst := func(name, state string, public bool) *sqladmin.DatabaseInstance {
		return &sqladmin.DatabaseInstance{Project: "p", Name: name, ConnectionName: "p:r:" + name, State: state,
			Settings: &sqladmin.Settings{IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: public}}}
	}
	_ = s.env.Store.Update(func(tx store.Tx) error {
		_ = store.PutJSON(tx, nsInstances, "p/a", &instanceRecord{PublicIP: "172.18.0.4", LoopIP: "127.0.0.7", Instance: inst("a", "RUNNABLE", true)})
		_ = store.PutJSON(tx, nsInstances, "p/b", &instanceRecord{PublicIP: "172.18.0.5", Instance: inst("b", "RUNNABLE", true)})
		_ = store.PutJSON(tx, nsInstances, "p/c", &instanceRecord{Instance: inst("c", "FAILED", true)})
		return store.PutJSON(tx, nsInstances, "p/d", &instanceRecord{PrivateIP: "10.0.0.3", Instance: inst("d", "RUNNABLE", false)})
	})
	byName := func(hs []hostInfo) map[string]hostInfo {
		m := map[string]hostInfo{}
		for _, h := range hs {
			m[h.Instance] = h
		}
		return m
	}
	if s.serveHostsBody(true)["aliasesRefused"] != false {
		t.Error("aliasesRefused before any refusal")
	}
	s.aliasRefused.Store(true)
	if s.serveHostsBody(true)["aliasesRefused"] != true {
		t.Error("the refusal is not reported")
	}
	got := byName(s.hostsReport(true))
	if len(got) != 3 || got["p:r:a"].Address != "127.0.0.7" || !got["p:r:a"].Loopback ||
		got["p:r:b"].Address != "" || got["p:r:b"].Missing == "" ||
		got["p:r:c"].Address != "" || !strings.Contains(got["p:r:c"].Missing, "FAILED") {
		t.Errorf("with loopback addresses: %+v", got)
	}
	got = byName(s.hostsReport(false))
	if got["p:r:b"].Address != "172.18.0.5" || got["p:r:b"].Loopback || got["p:r:b"].Missing != "" {
		t.Errorf("without loopback addresses: %+v", got)
	}
}
