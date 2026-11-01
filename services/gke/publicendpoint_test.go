package gke_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/netplane"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// TestPublicEndpointMoves (#644): the k3s server's certificate covers its
// address on the external network on every start. The cluster keeps the
// address while it is free on that network, and gets a new one when the
// network moved to another range or something else holds the address.
func TestPublicEndpointMoves(t *testing.T) {
	emutest.RequireRuntime(t)
	t.Parallel()
	emutest.HoldClusterSlot(t)
	ctx := context.Background()
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.EnsureImage(ctx, "busybox:1.38", false); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	inst := startPersistent(t, dir)
	m := &runtime.Manager{Client: cl, InstanceID: inst.Internal().ID}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = m.Cleanup(ctx, true)
	})
	running := true
	defer func() {
		if running {
			_ = inst.Internal().Shutdown(context.Background())
		}
	}()
	restart := func(between func()) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := inst.Internal().Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		running = false
		between()
		inst = startPersistent(t, dir)
		running = true
	}
	extName := m.Name("ext")

	cm := newClient(t, inst)
	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: "pub", NodePools: []*containerpb.NodePool{{Name: "np", InitialNodeCount: 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, cm, zone, op.Name)
	name := parent + "/clusters/pub"
	c := waitRunning(t, inst, name)
	first := serverExternalIP(t, cl, inst.Internal().ID, extName)
	checkServes(t, c, first)
	putConfigMap(t, inst, c, "kept")

	// A plain restart keeps the address and the Public endpoint.
	restart(func() {})
	c2 := waitRunning(t, inst, name)
	if got := serverExternalIP(t, cl, inst.Internal().ID, extName); got != first {
		t.Errorf("after a restart the external address is %s; want %s kept", got, first)
	}
	if c2.Endpoint != c.Endpoint {
		t.Errorf("after a restart the Public endpoint is %s; want %s kept", c2.Endpoint, c.Endpoint)
	}
	checkServes(t, c2, first)

	// The external network moves to another range: its old subnet is held
	// by another network when the Instance creates it again.
	restart(func() {
		n, err := cl.InspectNetwork(ctx, extName)
		if err != nil {
			t.Fatal(err)
		}
		if err := cl.RemoveNetwork(ctx, extName); err != nil {
			t.Fatal(err)
		}
		blocker := fmt.Sprintf("gcpemu-test-blocker-%d", rand.Uint32())
		if _, err := cl.CreateNetwork(ctx, runtime.NetworkSpec{Name: blocker, Subnet: n.Subnet}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cl.RemoveNetwork(context.Background(), blocker) })
	})
	c3 := waitRunning(t, inst, name)
	moved := serverExternalIP(t, cl, inst.Internal().ID, extName)
	n, err := cl.InspectNetwork(ctx, extName)
	if err != nil {
		t.Fatal(err)
	}
	if !netplane.Contains(n.Subnet, moved) || netplane.Contains(n.Subnet, first) {
		t.Fatalf("external network on %s, server at %s (was %s); want it moved off the old range", n.Subnet, moved, first)
	}
	checkServes(t, c3, moved)
	checkConfigMap(t, masterAuthEnv(t, inst, c3), "kept")

	// Something else holds the recorded address when the server starts.
	var holder string
	restart(func() {
		id, err := cl.CreateContainer(ctx, runtime.ContainerSpec{
			Name: fmt.Sprintf("gcpemu-test-holder-%d", rand.Uint32()), Image: "busybox:1.38", Entrypoint: []string{"sleep", "600"},
			Networks: []runtime.Attachment{{Network: extName, IP: moved}},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cl.RemoveContainer(context.Background(), id, true) })
		if err := cl.StartContainer(ctx, id); err != nil {
			t.Fatal(err)
		}
		holder = id
	})
	c4 := waitRunning(t, inst, name)
	taken := serverExternalIP(t, cl, inst.Internal().ID, extName)
	if taken == moved || !netplane.Contains(n.Subnet, taken) {
		t.Errorf("with %s held by %s the server is at %s; want another address in %s", moved, holder, taken, n.Subnet)
	}
	checkServes(t, c4, taken)
	checkConfigMap(t, masterAuthEnv(t, inst, c4), "kept")
}

// serverExternalIP is the address of the Instance's one k3s server on the
// external network, which its arguments must name as a certificate SAN.
// k3s also adds the address it is reached on to the certificate by itself,
// which hides a missing SAN most of the time but not always (#644).
func serverExternalIP(t *testing.T, cl *runtime.Client, instanceID, extName string) string {
	t.Helper()
	cs, err := cl.ListContainers(context.Background(), map[string]string{runtime.LabelInstance: instanceID})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if !strings.HasSuffix(c.Name, "-cp") || c.IPs[extName] == "" {
			continue
		}
		ip := c.IPs[extName]
		ct, err := cl.InspectContainer(context.Background(), c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(" "+strings.Join(ct.Cmd, " ")+" ", " --tls-san "+ip+" ") {
			t.Errorf("k3s server at %s on %s has no --tls-san for it: %v", ip, extName, ct.Cmd)
		}
		return ip
	}
	t.Fatalf("no k3s server on %s\n%s", extName, containerDiagnostics(instanceID))
	return ""
}

// checkServes checks that the cluster's endpoint presents a certificate,
// issued by its masterAuth CA, that is valid for ip. Through the VM relay
// the endpoint is a host address, so the name checked is set explicitly.
func checkServes(t *testing.T, c *containerpb.Cluster, ip string) {
	t.Helper()
	pem, _ := base64.StdEncoding.DecodeString(c.MasterAuth.GetClusterCaCertificate())
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("no clusterCaCertificate")
	}
	addr := c.Endpoint
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "443")
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, &tls.Config{RootCAs: pool, ServerName: ip})
	if err != nil {
		t.Fatalf("TLS to %s as %s: %v", addr, ip, err)
	}
	conn.Close()
}
