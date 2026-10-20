package emutest_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"path"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/linuxuser586/gcpemu/emutest"
)

// Start runs an Instance for one test, and Storage connects a Cloud Storage
// client to it. In a test file, the function body is a TestXxx function's.
func ExampleStart() {
	asTest(func(t testing.TB) {
		inst := emutest.Start(t, []string{"gcs"})
		ctx := context.Background()
		b := inst.Storage(t).Bucket("example-bucket")
		if err := b.Create(ctx, "example-proj", nil); err != nil {
			t.Fatal(err)
		}
		w := b.Object("hello.txt").NewWriter(ctx)
		fmt.Fprint(w, "hello, emulator")
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		r, err := b.Object("hello.txt").NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		got, _ := io.ReadAll(r)
		fmt.Println(string(got))
	})
	// Output: hello, emulator
}

// GRPCOptions connects any gRPC client library to the Instance. An
// Instance from New lives until Close, so TestMain can share one.
func ExampleInstance_GRPCOptions() {
	ctx := context.Background()
	inst, err := emutest.New(ctx, []string{"secrets"})
	if err != nil {
		log.Fatal(err)
	}
	defer inst.Close(ctx)

	sm, err := secretmanager.NewClient(ctx, inst.GRPCOptions()...)
	if err != nil {
		log.Fatal(err)
	}
	defer sm.Close()
	s, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   "projects/example-proj",
		SecretId: "api-key",
		Secret:   &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(path.Base(s.Name))
	// Output: api-key
}

// asTest runs f as `go test` runs a test function, for examples, which get
// no testing.TB: f's cleanups run when it returns, and failing exits.
func asTest(f func(t testing.TB)) {
	t := &exampleTB{}
	defer func() {
		for i := len(t.cleanups) - 1; i >= 0; i-- {
			t.cleanups[i]()
		}
	}()
	f(t)
}

// exampleTB is the part of testing.TB that examples use.
type exampleTB struct {
	testing.TB // nil: calling any other method panics
	cleanups   []func()
}

func (t *exampleTB) Helper()                   {}
func (t *exampleTB) Cleanup(f func())          { t.cleanups = append(t.cleanups, f) }
func (t *exampleTB) Fatal(args ...any)         { log.Fatal(args...) }
func (t *exampleTB) Fatalf(f string, a ...any) { log.Fatalf(f, a...) }
