package certs_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"testing"
	"time"

	certificatemanager "cloud.google.com/go/certificatemanager/apiv1"
	networksecurity "cloud.google.com/go/networksecurity/apiv1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

const (
	testProject = "test-proj"
	globalLoc   = "projects/" + testProject + "/locations/global"
)

// env is a started emulator with Certificate Manager and Network Security
// gRPC clients.
type env struct {
	t    *testing.T
	inst *emutest.Instance
	cm   *certificatemanager.Client
	ns   *networksecurity.Client
	ctx  context.Context
}

func start(t *testing.T, opts ...emutest.Option) *env {
	t.Helper()
	inst := emutest.Start(t, []string{"certs"}, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	copts := []option.ClientOption{
		option.WithEndpoint(inst.Endpoint("gateway")),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	}
	cm, err := certificatemanager.NewClient(ctx, copts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cm.Close() })
	ns, err := networksecurity.NewClient(ctx, copts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ns.Close() })
	return &env{t: t, inst: inst, cm: cm, ns: ns, ctx: ctx}
}

// mgr returns the CertManager the load balancer would use.
func (e *env) mgr() emu.CertManager {
	e.t.Helper()
	s, ok := e.inst.Env.Lookup("certs")
	if !ok {
		e.t.Fatal("certs service not running")
	}
	return s.(emu.CertManager)
}

// rest performs a REST call against the gateway and decodes the JSON reply.
func (e *env) rest(method, path string, body any, out any) int {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = bytes.NewReader([]byte(b))
		default:
			j, _ := json.Marshal(b)
			rd = bytes.NewReader(j)
		}
	}
	req, _ := http.NewRequestWithContext(e.ctx, method, e.inst.GatewayURL()+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, b, err)
		}
	}
	return resp.StatusCode
}

// testCA is a throwaway CA for self-managed certificates.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{cert: c, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// leaf issues a leaf for names and returns (chain PEM, key PEM).
func (c *testCA) leaf(t *testing.T, usage x509.ExtKeyUsage, names ...string) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]},
		DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(12 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}))
}
