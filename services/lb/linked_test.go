package lb_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	computev1 "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/services/lb"
)

// startLinkable starts an instance serving the link protocol on loopback,
// and its rules on the --lb-bind addresses lbBind.
func startLinkable(t *testing.T, lbBind ...string) *env {
	t.Helper()
	inst := emutest.Start(t, []string{"lb", "cdn"}, emutest.WithConfig(func(c *config.Config) {
		c.LBBind, c.LinkBind = lbBind, []string{"127.0.0.1"}
	}))
	svc, _ := inst.Internal().Env.Lookup("lb")
	svc.(*lb.Service).SetOptions(lb.Options{DirectDial: true})
	c, err := computev1.NewService(context.Background(), option.WithEndpoint(inst.GatewayURL()+"/compute/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, inst: inst, c: c}
}

// admin calls e's Admin API, failing the test unless it answers 200.
func (e *env) admin(method, path string, body, out any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.inst.GatewayURL()+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatal(err)
		}
	}
}

// delegateTo pairs front with linked over the Admin API, approved on
// both sides, and delegates proj, which linked shares, to it.
func delegateTo(t *testing.T, front, linked *env) {
	t.Helper()
	addr := linked.inst.Endpoint("link")
	var id struct{ Fingerprint string }
	front.admin(http.MethodPost, "/_emu/v1/links/identify", map[string]string{"address": addr}, &id)
	done := make(chan struct{})
	go func() {
		defer close(done)
		front.admin(http.MethodPost, "/_emu/v1/links", map[string]string{"address": addr, "fingerprint": id.Fingerprint, "name": "office"}, nil)
	}()
	for i := 0; ; i++ {
		var reqs struct{ Requests []struct{ ID string } }
		linked.admin(http.MethodGet, "/_emu/v1/links/requests", nil, &reqs)
		if len(reqs.Requests) > 0 {
			linked.admin(http.MethodPost, "/_emu/v1/links/requests/"+reqs.Requests[0].ID+"/approve", map[string]string{"name": "laptop"}, nil)
			break
		}
		if i == 500 {
			t.Fatal("no Pairing request arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	<-done
	linked.admin(http.MethodPost, "/_emu/v1/links/shares", map[string]string{"project": proj}, nil)
	front.admin(http.MethodPost, "/_emu/v1/links/delegations", map[string]string{"project": proj, "link": "office"}, nil)
}

// TestFrontEnvReachesLinkedRule: a forwarding rule of a Project delegated
// to an Instance with --lb-bind gets its variables in the front Instance's
// `gcpemu env`, at the --lb-bind address, where a client on the front
// Instance's machine reaches it (#715).
func TestFrontEnvReachesLinkedRule(t *testing.T) {
	if c, err := net.DialTimeout("tcp", "127.0.0.1:8080", time.Second); err == nil {
		c.Close()
		t.Skip("127.0.0.1:8080 is in use")
	}
	office, laptop := startLinkable(t, "127.0.0.1"), startLinkable(t)
	office.lanLB(newBackend(t))
	office.forwardingRule("web", 8080)
	delegateTo(t, laptop, office)

	vars := laptop.inst.EnvVars()
	addr := vars["GCPEMU_LB_"+strings.ToUpper(strings.ReplaceAll(proj, "-", "_"))+"_GLOBAL_WEB"]
	if addr != "127.0.0.1:8080" || vars["GCPEMU_LB_WEB"] != addr {
		t.Fatalf("the laptop's variables name web at %q, GCPEMU_LB_WEB=%q; want the office's --lb-bind 127.0.0.1:8080", addr, vars["GCPEMU_LB_WEB"])
	}
	cl := &http.Client{Timeout: 10 * time.Second}
	if resp, body := get(t, cl, "http://"+addr+"/hello"); resp.StatusCode != 200 || !strings.HasSuffix(body, "/hello") {
		t.Fatalf("via the laptop's variable: %d %q, want the office's backend", resp.StatusCode, body)
	}
}
