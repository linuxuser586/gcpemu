// Package managed_test runs Google's emulators as Managed emulators
// (--external API=managed, ADR 0029) and drives each with its Google
// client library through the API gateway, as another Go module would.
package managed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/datastore"
	"cloud.google.com/go/firestore"
	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

const project = "managed-proj"

// managedAll delegates the four APIs to Managed emulators.
var managedAll = []emutest.Option{
	emutest.WithExternal("firestore", "managed"),
	emutest.WithExternal("datastore", "managed"),
	emutest.WithExternal("spanner", "managed"),
	emutest.WithExternal("bigtable", "managed"),
}

// TestManagedEmulators: each of the four APIs' clients, given only the
// Instance's env output, writes and reads through the API gateway; after
// Reset what they wrote is gone (#624).
func TestManagedEmulators(t *testing.T) {
	emutest.RequireRuntime(t)
	inst := emutest.Start(t, []string{"iam"}, managedAll...)
	gw := inst.Endpoint("gateway")
	for _, v := range []string{"FIRESTORE_EMULATOR_HOST", "DATASTORE_EMULATOR_HOST", "SPANNER_EMULATOR_HOST", "BIGTABLE_EMULATOR_HOST"} {
		if got := inst.EnvVars()[v]; got != gw {
			t.Errorf("%s = %q, want the API gateway %s", v, got, gw)
		}
	}
	inst.Setenv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var ext struct {
		External []struct {
			API       string `json:"api"`
			Address   string `json:"address"`
			Managed   bool   `json:"managed"`
			Reachable bool   `json:"reachable"`
		} `json:"external"`
	}
	resp, err := http.Get(inst.GatewayURL() + "/_emu/v1/external")
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewDecoder(resp.Body).Decode(&ext)
	resp.Body.Close()
	if err != nil || len(ext.External) != 4 {
		t.Fatalf("external = %+v (%v), want the four APIs", ext.External, err)
	}
	for _, e := range ext.External {
		if !e.Managed || !e.Reachable || e.Address != "" {
			t.Errorf("%s = %+v, want it managed and reachable, with no address", e.API, e)
		}
	}

	fs := firestoreRoundTrip(ctx, t)
	ds := datastoreRoundTrip(ctx, t)
	sp := spannerRoundTrip(ctx, t)
	bt := bigtableRoundTrip(ctx, t)

	if err := inst.Internal().Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, err := fs.Collection("c").Doc("d").Get(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("Firestore document after Reset: %v, want NotFound", err)
	}
	var e entity
	if err := ds.Get(ctx, datastore.NameKey("Kind", "k", nil), &e); err != datastore.ErrNoSuchEntity {
		t.Errorf("Datastore entity after Reset: %v, want ErrNoSuchEntity", err)
	}
	if _, err := sp.Single().ReadRow(ctx, "T", spanner.Key{"k"}, []string{"V"}); spanner.ErrCode(err) != codes.NotFound {
		t.Errorf("Spanner row after Reset: %v, want NotFound (the instance is gone)", err)
	}
	if _, err := bt.Open("t").ReadRow(ctx, "r"); status.Code(err) != codes.NotFound {
		t.Errorf("Bigtable row after Reset: %v, want NotFound (the table is gone)", err)
	}
}

func firestoreRoundTrip(ctx context.Context, t *testing.T) *firestore.Client {
	c, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Collection("c").Doc("d").Set(ctx, map[string]any{"v": "firestore"}); err != nil {
		t.Fatalf("Firestore Set: %v", err)
	}
	snap, err := c.Collection("c").Doc("d").Get(ctx)
	if err != nil {
		t.Fatalf("Firestore Get: %v", err)
	}
	if v := snap.Data()["v"]; v != "firestore" {
		t.Errorf("Firestore v = %v, want firestore", v)
	}
	return c
}

type entity struct{ V string }

func datastoreRoundTrip(ctx context.Context, t *testing.T) *datastore.Client {
	c, err := datastore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	k := datastore.NameKey("Kind", "k", nil)
	if _, err := c.Put(ctx, k, &entity{V: "datastore"}); err != nil {
		t.Fatalf("Datastore Put: %v", err)
	}
	var e entity
	if err := c.Get(ctx, k, &e); err != nil {
		t.Fatalf("Datastore Get: %v", err)
	}
	if e.V != "datastore" {
		t.Errorf("Datastore V = %q, want datastore", e.V)
	}
	return c
}

func spannerRoundTrip(ctx context.Context, t *testing.T) *spanner.Client {
	ia, err := instance.NewInstanceAdminClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ia.Close()
	op, err := ia.CreateInstance(ctx, &instancepb.CreateInstanceRequest{
		Parent: "projects/" + project, InstanceId: "inst",
		Instance: &instancepb.Instance{Config: "projects/" + project + "/instanceConfigs/emulator-config", DisplayName: "inst", NodeCount: 1},
	})
	if err != nil {
		t.Fatalf("Spanner CreateInstance: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("Spanner CreateInstance: %v", err)
	}
	da, err := database.NewDatabaseAdminClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer da.Close()
	dop, err := da.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{
		Parent: "projects/" + project + "/instances/inst", CreateStatement: "CREATE DATABASE db",
		ExtraStatements: []string{"CREATE TABLE T (K STRING(10), V STRING(10)) PRIMARY KEY (K)"},
	})
	if err != nil {
		t.Fatalf("Spanner CreateDatabase: %v", err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Fatalf("Spanner CreateDatabase: %v", err)
	}
	c, err := spanner.NewClient(ctx, "projects/"+project+"/instances/inst/databases/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if _, err := c.Apply(ctx, []*spanner.Mutation{spanner.Insert("T", []string{"K", "V"}, []any{"k", "spanner"})}); err != nil {
		t.Fatalf("Spanner Apply: %v", err)
	}
	row, err := c.Single().ReadRow(ctx, "T", spanner.Key{"k"}, []string{"V"})
	if err != nil {
		t.Fatalf("Spanner ReadRow: %v", err)
	}
	var v string
	if err := row.Column(0, &v); err != nil || v != "spanner" {
		t.Errorf("Spanner V = %q (%v), want spanner", v, err)
	}
	return c
}

func bigtableRoundTrip(ctx context.Context, t *testing.T) *bigtable.Client {
	ac, err := bigtable.NewAdminClient(ctx, project, "i")
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	if err := ac.CreateTable(ctx, "t"); err != nil {
		t.Fatalf("Bigtable CreateTable: %v", err)
	}
	if err := ac.CreateColumnFamily(ctx, "t", "f"); err != nil {
		t.Fatalf("Bigtable CreateColumnFamily: %v", err)
	}
	c, err := bigtable.NewClient(ctx, project, "i")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	m := bigtable.NewMutation()
	m.Set("f", "c", bigtable.Now(), []byte("bigtable"))
	if err := c.Open("t").Apply(ctx, "r", m); err != nil {
		t.Fatalf("Bigtable Apply: %v", err)
	}
	row, err := c.Open("t").ReadRow(ctx, "r")
	if err != nil {
		t.Fatalf("Bigtable ReadRow: %v", err)
	}
	if got := row["f"]; len(got) != 1 || string(got[0].Value) != "bigtable" {
		t.Errorf("Bigtable row = %v, want f:c = bigtable", row)
	}
	return c
}

// ready is GET /_emu/v1/ready.
type ready struct {
	Ready    bool `json:"ready"`
	Services map[string]struct {
		Ready  bool   `json:"ready"`
		Reason string `json:"reason"`
	} `json:"services"`
}

func getReady(t *testing.T, inst *emutest.Instance) ready {
	t.Helper()
	resp, err := http.Get(inst.GatewayURL() + "/_emu/v1/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r ready
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestManagedReady: the Instance is not ready, naming the Managed
// emulator, until it accepts connections, and is ready once it does
// (#624).
func TestManagedReady(t *testing.T) {
	emutest.RequireRuntime(t)
	inst := emutest.Start(t, []string{"iam"}, emutest.WithExternal("firestore", "managed"))
	in := inst.Internal()
	// Reset replaces the emulator; while it starts again the Instance is
	// not ready and names it.
	done := make(chan error, 1)
	go func() { done <- in.Reset(context.Background()) }()
	deadline := time.Now().Add(time.Minute)
	for {
		if r := getReady(t, inst); !r.Ready {
			if s := r.Services["managed-emulator/firestore"]; !s.Ready && strings.Contains(s.Reason, "firestore") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the Instance never reported the Managed emulator for firestore not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatalf("Reset: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := in.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if r := getReady(t, inst); !r.Ready || !r.Services["managed-emulator/firestore"].Ready {
		t.Errorf("ready = %+v, want the Managed emulator for firestore ready", r)
	}
}

// emulatorContainers lists the containers of Managed emulators the
// Instance with ID id owns.
func emulatorContainers(t *testing.T, id string) []runtime.Container {
	t.Helper()
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	cts, err := cl.ListContainers(context.Background(), map[string]string{runtime.LabelInstance: id, runtime.LabelService: "external"})
	if err != nil {
		t.Fatal(err)
	}
	return cts
}

// TestManagedStop: stopping the Instance removes its Managed emulator,
// and a Persistent Instance started again has none of the earlier data of
// one that keeps its data in memory only, such as Bigtable's (#624, #649).
func TestManagedStop(t *testing.T) {
	emutest.RequireRuntime(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	inst, err := emutest.New(ctx, []string{"iam"}, emutest.WithTestDataDir(t, dir), emutest.WithExternal("bigtable", "managed"))
	if err != nil {
		t.Fatal(err)
	}
	id := inst.Internal().ID
	if n := len(emulatorContainers(t, id)); n != 1 {
		t.Fatalf("%d Managed emulator containers, want 1", n)
	}
	inst.Setenv(t)
	bigtableRoundTrip(ctx, t)
	if err := inst.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if cts := emulatorContainers(t, id); len(cts) != 0 {
		t.Fatalf("after stop, Managed emulator containers %v remain", cts)
	}

	inst, err = emutest.New(ctx, []string{"iam"}, emutest.WithTestDataDir(t, dir), emutest.WithExternal("bigtable", "managed"))
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close(context.Background())
	inst.Setenv(t)
	c, err := bigtable.NewClient(ctx, project, "i")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Open("t").ReadRow(ctx, "r"); status.Code(err) != codes.NotFound {
		t.Errorf("Bigtable row after a restart: %v, want NotFound (the table is gone)", err)
	}
}

// TestManagedDisrupted: a Managed emulator that the runtime stops behind
// the Instance's back is a Disrupted container (#624).
func TestManagedDisrupted(t *testing.T) {
	emutest.RequireRuntime(t)
	inst := emutest.Start(t, []string{"iam"}, emutest.WithExternal("firestore", "managed"))
	cts := emulatorContainers(t, inst.Internal().ID)
	if len(cts) != 1 {
		t.Fatalf("%d Managed emulator containers, want 1", len(cts))
	}
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	if err := stopContainer(cl, cts[0].ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		r := getReady(t, inst)
		if s := r.Services["containers"]; !r.Ready && strings.Contains(s.Reason, cts[0].Name) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ready = %+v, want the containers reported Disrupted, naming %s", r, cts[0].Name)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// stopContainer stops container id as `docker stop` would, through a client other
// than the Instance's.
func stopContainer(cl *runtime.Client, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cl.StopContainer(ctx, id, time.Second); err != nil {
		return fmt.Errorf("stopping %s: %w", id, err)
	}
	return nil
}
