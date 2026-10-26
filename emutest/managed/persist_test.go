package managed_test

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/linuxuser586/gcpemu/emutest"
	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// Firestore's and Datastore's Managed emulators keep their data across a
// clean stop of a Persistent Instance, and State snapshots hold it (#649,
// ADR 0036).

const otherProject = "managed-proj-2"

// keeping delegates the two APIs whose Managed emulators keep their data.
var keeping = []emutest.Option{
	emutest.WithExternal("firestore", "managed"),
	emutest.WithExternal("datastore", "managed"),
}

// startIn starts a Persistent Instance in dir, with its clients' variables
// set, stopping it when the test ends.
func startIn(ctx context.Context, t *testing.T, dir string, opts ...emutest.Option) *emutest.Instance {
	t.Helper()
	inst, err := emutest.New(ctx, []string{"iam"}, append([]emutest.Option{emutest.WithDataDir(dir)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inst.Close(context.Background()) })
	inst.Setenv(t)
	return inst
}

// stop stops inst cleanly.
func stop(ctx context.Context, t *testing.T, inst *emutest.Instance) {
	t.Helper()
	if err := inst.Close(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// fsDoc is a Firestore document the tests write.
type fsDoc struct {
	project, database, id string
}

var fsDocs = []fsDoc{
	{project, "(default)", "a"},
	{project, "named", "b"},
	{otherProject, "(default)", "c"},
}

// dsEntity is a Datastore entity the tests write.
type dsEntity struct {
	project, database, namespace, name string
}

var dsEntities = []dsEntity{
	{project, "", "", "a"},
	{project, "named", "", "b"},
	{project, "", "ns", "c"},
	{otherProject, "", "", "d"},
}

func firestoreClient(ctx context.Context, t *testing.T, project, database string) *firestore.Client {
	t.Helper()
	c, err := firestore.NewClientWithDatabase(ctx, project, database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func datastoreClient(ctx context.Context, t *testing.T, project, database string) *datastore.Client {
	t.Helper()
	c, err := datastore.NewClientWithDatabase(ctx, project, database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func dsKey(e dsEntity) *datastore.Key {
	k := datastore.NameKey("Kind", e.name, nil)
	k.Namespace = e.namespace
	return k
}

// write writes every document and entity, with value v.
func write(ctx context.Context, t *testing.T, v string) {
	t.Helper()
	for _, d := range fsDocs {
		c := firestoreClient(ctx, t, d.project, d.database)
		if _, err := c.Collection("c").Doc(d.id).Set(ctx, map[string]any{"v": v}); err != nil {
			t.Fatalf("Firestore %+v: %v", d, err)
		}
	}
	for _, e := range dsEntities {
		c := datastoreClient(ctx, t, e.project, e.database)
		if _, err := c.Put(ctx, dsKey(e), &entity{V: v}); err != nil {
			t.Fatalf("Datastore %+v: %v", e, err)
		}
	}
}

// check fails unless every document and entity has value want, or, when
// want is "", none is there.
func check(ctx context.Context, t *testing.T, when, want string) {
	t.Helper()
	for _, d := range fsDocs {
		c := firestoreClient(ctx, t, d.project, d.database)
		snap, err := c.Collection("c").Doc(d.id).Get(ctx)
		switch {
		case want == "" && status.Code(err) != codes.NotFound:
			t.Errorf("%s: Firestore %+v: %v, want NotFound", when, d, err)
		case want != "" && err != nil:
			t.Errorf("%s: Firestore %+v: %v", when, d, err)
		case want != "" && snap.Data()["v"] != want:
			t.Errorf("%s: Firestore %+v: v = %v, want %s", when, d, snap.Data()["v"], want)
		}
	}
	for _, e := range dsEntities {
		c := datastoreClient(ctx, t, e.project, e.database)
		var got entity
		err := c.Get(ctx, dsKey(e), &got)
		switch {
		case want == "" && err != datastore.ErrNoSuchEntity:
			t.Errorf("%s: Datastore %+v: %v, want ErrNoSuchEntity", when, e, err)
		case want != "" && err != nil:
			t.Errorf("%s: Datastore %+v: %v", when, e, err)
		case want != "" && got.V != want:
			t.Errorf("%s: Datastore %+v: V = %q, want %s", when, e, got.V, want)
		}
	}
}

// checkOwnProject fails if a document or entity shows in the other project.
func checkOwnProject(ctx context.Context, t *testing.T) {
	t.Helper()
	if _, err := firestoreClient(ctx, t, otherProject, "(default)").Collection("c").Doc("a").Get(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("Firestore document of %s in %s: %v, want NotFound", project, otherProject, err)
	}
	var e entity
	if err := datastoreClient(ctx, t, otherProject, "").Get(ctx, datastore.NameKey("Kind", "a", nil), &e); err != datastore.ErrNoSuchEntity {
		t.Errorf("Datastore entity of %s in %s: %v, want ErrNoSuchEntity", project, otherProject, err)
	}
}

// TestManagedKeepsData: documents and entities in default and named
// databases, a namespace and two projects survive a stop and start, each
// in its own project only; after Reset, a stop and start brings none back
// (#649).
func TestManagedKeepsData(t *testing.T) {
	emutest.RequireRuntime(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	inst := startIn(ctx, t, dir, keeping...)
	write(ctx, t, "one")
	stop(ctx, t, inst)

	inst = startIn(ctx, t, dir, keeping...)
	check(ctx, t, "after a restart", "one")
	checkOwnProject(ctx, t)
	if err := inst.Internal().Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	check(ctx, t, "after Reset", "")
	stop(ctx, t, inst)

	startIn(ctx, t, dir, keeping...)
	check(ctx, t, "after Reset and a restart", "")
}

// TestManagedKeepsUnmanaged: saved data of an API the Instance once runs
// without managing it is left alone, and comes back when it manages the
// API again (#649).
func TestManagedKeepsUnmanaged(t *testing.T) {
	emutest.RequireRuntime(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	inst := startIn(ctx, t, dir, keeping...)
	write(ctx, t, "one")
	stop(ctx, t, inst)
	stop(ctx, t, startIn(ctx, t, dir))
	startIn(ctx, t, dir, keeping...)
	check(ctx, t, "after a start without them managed", "one")
}

// TestManagedSnapshot: restoring a State snapshot brings Firestore's and
// Datastore's data back to what it held, without what was written since
// (#649).
func TestManagedSnapshot(t *testing.T) {
	emutest.RequireRuntime(t)
	inst := emutest.Start(t, []string{"iam"}, keeping...)
	inst.Setenv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	write(ctx, t, "one")
	in := inst.Internal()
	if _, err := in.SaveSnapshot(ctx, "s"); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	write(ctx, t, "two")
	extra := firestoreClient(ctx, t, project, "(default)").Collection("c").Doc("later")
	if _, err := extra.Set(ctx, map[string]any{"v": "later"}); err != nil {
		t.Fatal(err)
	}
	if err := in.RestoreSnapshot(ctx, "s"); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	check(ctx, t, "after the restore", "one")
	if _, err := extra.Get(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("document written after the snapshot: %v, want NotFound", err)
	}
}

// TestManagedStopUnreachable: when the Managed emulator's container is
// gone at stop, the stop completes within its bound and the data saved at
// the stop before is kept (#649).
func TestManagedStopUnreachable(t *testing.T) {
	emutest.RequireRuntime(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	inst := startIn(ctx, t, dir, keeping...)
	write(ctx, t, "one")
	stop(ctx, t, inst)

	inst = startIn(ctx, t, dir, keeping...)
	write(ctx, t, "two")
	cl, err := runtime.Detect()
	if err != nil {
		t.Fatal(err)
	}
	for _, ct := range emulatorContainers(t, inst.Internal().ID) {
		if err := cl.RemoveContainer(ctx, ct.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	_ = inst.Close(ctx)
	if d := time.Since(start); d > time.Minute {
		t.Errorf("stop took %v with the emulators gone", d)
	}
	startIn(ctx, t, dir, keeping...)
	check(ctx, t, "after a stop with the emulators gone", "one")
}
