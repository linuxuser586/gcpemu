package gcs_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"sync"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/services/gcs"
)

const testProject = "test-project"

// start boots an instance running gcs.
func start(t *testing.T, opts ...emutest.Option) *emutest.Instance {
	t.Helper()
	return emutest.Start(t, []string{"gcs"}, opts...)
}

// gcsService returns the running gcs service.
func gcsService(t *testing.T, inst *emutest.Instance) *gcs.Service {
	t.Helper()
	svc, ok := inst.Env.Lookup("gcs")
	if !ok {
		t.Fatal("gcs not running")
	}
	return svc.(*gcs.Service)
}

// inject adds (or replaces) a peer service visible through env.Lookup.
func inject(inst *emutest.Instance, svc emu.Service) {
	m := map[string]emu.Service{}
	for _, s := range inst.Services() {
		m[s.Name()] = s
	}
	m[svc.Name()] = svc
	inst.Env.SetServices(m)
}

// emulatorClient connects through STORAGE_EMULATOR_HOST (per-service port,
// XML-path reads).
func emulatorClient(t *testing.T, inst *emutest.Instance) *storage.Client {
	t.Helper()
	t.Setenv("STORAGE_EMULATOR_HOST", inst.Endpoint("gcs"))
	c, err := storage.NewClient(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// gatewayClient connects through the gateway with option.WithEndpoint.
// Reads use the JSON API: the client's XML-path reads go to the endpoint
// host root, which the shared gateway does not route to GCS.
func gatewayClient(t *testing.T, inst *emutest.Instance) *storage.Client {
	t.Helper()
	t.Setenv("STORAGE_EMULATOR_HOST", "")
	c, err := storage.NewClient(context.Background(),
		option.WithEndpoint(inst.GatewayURL()+"/storage/v1/"),
		option.WithoutAuthentication(),
		storage.WithJSONReads())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// clientModes runs fn with both client configurations.
func clientModes(t *testing.T, fn func(t *testing.T, inst *emutest.Instance, c *storage.Client)) {
	t.Run("emulator-host", func(t *testing.T) {
		inst := start(t)
		fn(t, inst, emulatorClient(t, inst))
	})
	t.Run("gateway-endpoint", func(t *testing.T) {
		inst := start(t)
		fn(t, inst, gatewayClient(t, inst))
	})
}

// fakeIAM provides service account public keys for signed URL tests.
type fakeIAM struct {
	email string
	key   *rsa.PrivateKey
}

func newFakeIAM(t *testing.T, email string) *fakeIAM {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeIAM{email: email, key: k}
}

func (f *fakeIAM) Name() string                { return "iam" }
func (f *fakeIAM) Register(emu.Router) error   { return nil }
func (f *fakeIAM) Start(context.Context) error { return nil }
func (f *fakeIAM) Stop(context.Context) error  { return nil }
func (f *fakeIAM) Ready() error                { return nil }
func (f *fakeIAM) PublicKeys(_ context.Context, email string) ([]*rsa.PublicKey, error) {
	if email != f.email {
		return nil, errors.New("unknown service account")
	}
	return []*rsa.PublicKey{&f.key.PublicKey}, nil
}
func (f *fakeIAM) SignBlob(context.Context, string, []byte) (string, []byte, error) {
	return "", nil, errors.New("unimplemented")
}
func (f *fakeIAM) IDToken(context.Context, string, string) (string, error) {
	return "", errors.New("unimplemented")
}
func (f *fakeIAM) AccessToken(context.Context, emu.Principal) (string, int, error) {
	return "", 0, errors.New("unimplemented")
}

// published is one message captured by fakePubSub.
type published struct {
	topic string
	data  []byte
	attrs map[string]string
}

// fakePubSub records published messages.
type fakePubSub struct {
	mu     sync.Mutex
	topics map[string]bool
	msgs   []published
}

func (f *fakePubSub) Name() string                { return "pubsub" }
func (f *fakePubSub) Register(emu.Router) error   { return nil }
func (f *fakePubSub) Start(context.Context) error { return nil }
func (f *fakePubSub) Stop(context.Context) error  { return nil }
func (f *fakePubSub) Ready() error                { return nil }
func (f *fakePubSub) PublishInternal(_ context.Context, topic string, data []byte, attrs map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, published{topic, data, attrs})
	return "1", nil
}
func (f *fakePubSub) TopicExists(_ context.Context, topic string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.topics[topic]
}

func (f *fakePubSub) messages() []published {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]published(nil), f.msgs...)
}
