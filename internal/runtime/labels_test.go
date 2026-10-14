package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every object records its Instance's data dir, so that `gcpemu prune`
// can tell an Abandoned Instance ID from a live one (#192).
func TestLabelsRecordDir(t *testing.T) {
	m := &Manager{InstanceID: "abc", Dir: "/state/gcpemu/default"}
	l := m.Labels("sql", "p/db", "postgres")
	if l[LabelInstance] != "abc" || l[LabelDir] != "/state/gcpemu/default" || l[LabelService] != "sql" {
		t.Errorf("Labels = %v", l)
	}
	if l := (&Manager{InstanceID: "abc"}).Labels("", "", ""); len(l) != 1 {
		t.Errorf("without a dir: %v", l)
	}
}

// Volumes returns each volume with its labels; ListVolumes only the names.
func TestVolumes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/volumes") || !strings.Contains(r.URL.Query().Get("filters"), LabelInstance) {
			w.WriteHeader(404)
			return
		}
		_, _ = io.WriteString(w, `{"Volumes":[{"Name":"v1","Labels":{"gcpemu.instance":"abc","gcpemu.dir":"/d"}}]}`)
	}))
	defer srv.Close()
	cl, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	sel := map[string]string{LabelInstance: ""}
	vs, err := cl.Volumes(context.Background(), sel)
	if err != nil || len(vs) != 1 || vs[0].Name != "v1" || vs[0].Labels[LabelDir] != "/d" {
		t.Fatalf("Volumes = %v, %v", vs, err)
	}
	names, err := cl.ListVolumes(context.Background(), sel)
	if err != nil || len(names) != 1 || names[0] != "v1" {
		t.Fatalf("ListVolumes = %v, %v", names, err)
	}
}
