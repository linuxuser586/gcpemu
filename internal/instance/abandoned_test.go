package instance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// fakeRuntime serves the parts of the Docker API that FindAbandoned and
// Prune use, and records what was removed, in order.
type fakeRuntime struct {
	mu         sync.Mutex
	containers map[string]fakeObj
	networks   map[string]fakeObj
	volumes    map[string]fakeObj
	removed    []string
}

type fakeObj struct {
	labels  map[string]string
	running bool
}

func (f *fakeRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1.41")
	var want []string
	if fl := r.URL.Query().Get("filters"); fl != "" {
		var v map[string][]string
		_ = json.Unmarshal([]byte(fl), &v)
		want = v["label"]
	}
	match := func(l map[string]string) bool {
		for _, w := range want {
			k, v, eq := strings.Cut(w, "=")
			got, ok := l[k]
			if !ok || eq && got != v {
				return false
			}
		}
		return true
	}
	kind, name, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	objs := map[string]map[string]fakeObj{"containers": f.containers, "networks": f.networks, "volumes": f.volumes}[kind]
	if r.Method == http.MethodDelete {
		if _, ok := objs[name]; !ok {
			w.WriteHeader(404)
			return
		}
		delete(objs, name)
		f.removed = append(f.removed, kind+":"+name)
		return
	}
	name = strings.TrimSuffix(name, "/json")
	if name == "" || name == "json" {
		var ids []map[string]any
		for id, o := range objs {
			if match(o.labels) {
				ids = append(ids, map[string]any{"Id": id, "Name": id, "Labels": o.labels})
			}
		}
		if kind == "volumes" {
			_ = json.NewEncoder(w).Encode(map[string]any{"Volumes": ids})
			return
		}
		_ = json.NewEncoder(w).Encode(ids)
		return
	}
	o, ok := objs[name]
	if !ok {
		w.WriteHeader(404)
		return
	}
	if kind == "containers" {
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": name, "Name": "/" + name,
			"Config": map[string]any{"Labels": o.labels}, "State": map[string]any{"Running": o.running}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"Id": name, "Name": name, "Labels": o.labels})
}

func labelled(id, dir string) map[string]string {
	l := map[string]string{runtime.LabelInstance: id}
	if dir != "" {
		l[runtime.LabelDir] = dir
	}
	return l
}

// TestFindAndPruneAbandoned is #192: objects of Instance IDs that no data
// dir holds are found and removed; those of live Instances are not.
func TestFindAndPruneAbandoned(t *testing.T) {
	root := t.TempDir()
	stateHome := filepath.Join(root, "state")
	writeID := func(dir, id string) string {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, IDFile), []byte(id), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	live := writeID(filepath.Join(root, "live"), "live")
	reused := writeID(filepath.Join(root, "reused"), "newer")
	writeID(filepath.Join(stateHome, "default"), "legacyheld")
	gone := filepath.Join(root, "gone")

	f := &fakeRuntime{
		containers: map[string]fakeObj{
			"c-running":   {labels: labelled("running", gone), running: true},
			"c-mixed":     {labels: labelled("mixed", gone)},
			"c-live":      {labels: labelled("live", live)},
			"c-unrelated": {labels: map[string]string{"app": "x"}},
		},
		networks: map[string]fakeObj{
			"n-gone":       {labels: labelled("gone", gone)},
			"n-live":       {labels: labelled("live", live)},
			"n-reused":     {labels: labelled("reused", reused)},
			"n-running":    {labels: labelled("running", gone)},
			"n-legacy":     {labels: labelled("legacy", "")},
			"n-legacyheld": {labels: labelled("legacyheld", "")},
			"n-mixed":      {labels: labelled("mixed", gone)},
		},
		volumes: map[string]fakeObj{
			"v-mixed":   {labels: labelled("mixed", gone)},
			"v-running": {labels: labelled("running", gone)},
			"v-live":    {labels: labelled("live", live)},
		},
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	cl, err := runtime.New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	abandoned, err := FindAbandoned(ctx, cl, stateHome)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range abandoned {
		got = append(got, a.ID)
	}
	if want := []string{"gone", "legacy", "mixed", "reused"}; !slices.Equal(got, want) {
		t.Fatalf("abandoned IDs = %v; want %v", got, want)
	}
	byID := map[string]Abandoned{}
	for _, a := range abandoned {
		byID[a.ID] = a
	}
	if a := byID["legacy"]; !a.Unverified || len(a.Networks) != 1 {
		t.Errorf("legacy = %+v; want 1 unverified network", a)
	}
	if a := byID["mixed"]; a.Unverified || len(a.Containers) != 1 || len(a.Networks) != 1 || len(a.Volumes) != 1 {
		t.Errorf("mixed = %+v; want 1 container, network and volume", a)
	}
	if a := byID["gone"]; a.Unverified || !slices.Equal(a.Dirs, []string{gone}) {
		t.Errorf("gone = %+v; want verified, dir %s", a, gone)
	}

	for _, a := range abandoned {
		if err := a.Prune(ctx, cl); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"networks:n-gone", "networks:n-legacy", "containers:c-mixed", "networks:n-mixed", "volumes:v-mixed", "networks:n-reused"}; !slices.Equal(f.removed, want) {
		t.Errorf("removed %v; want %v", f.removed, want)
	}
	if again, err := FindAbandoned(ctx, cl, stateHome); err != nil || len(again) != 0 {
		t.Errorf("after prune: %+v, %v", again, err)
	}
}

// TestFindTagged: every Instance ID that tags an object is found, live
// ones too, with the dirs its objects record (#846).
func TestFindTagged(t *testing.T) {
	f := &fakeRuntime{
		containers: map[string]fakeObj{
			"c-running":   {labels: labelled("running", "/d/running"), running: true},
			"c-unrelated": {labels: map[string]string{"app": "x"}},
		},
		networks: map[string]fakeObj{
			"n-running": {labels: labelled("running", "/d/running")},
			"n-legacy":  {labels: labelled("legacy", "")},
		},
		volumes: map[string]fakeObj{"v-two": {labels: labelled("two", "/d/b")}, "v-two2": {labels: labelled("two", "/d/a")}},
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	cl, err := runtime.New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	tagged, err := FindTagged(context.Background(), cl)
	if err != nil {
		t.Fatal(err)
	}
	if len(tagged) != 3 {
		t.Fatalf("tagged = %+v; want legacy, running and two", tagged)
	}
	if a := tagged[0]; a.ID != "legacy" || len(a.Dirs) != 0 || len(a.Networks) != 1 {
		t.Errorf("tagged[0] = %+v; want legacy with its network and no dir", a)
	}
	if a := tagged[1]; a.ID != "running" || len(a.Containers) != 1 || len(a.Networks) != 1 || !slices.Equal(a.Dirs, []string{"/d/running"}) {
		t.Errorf("tagged[1] = %+v; want running with its container, network and dir", a)
	}
	if a := tagged[2]; a.ID != "two" || len(a.Volumes) != 2 || !slices.Equal(a.Dirs, []string{"/d/a", "/d/b"}) {
		t.Errorf("tagged[2] = %+v; want two with both volumes and sorted dirs", a)
	}
}
