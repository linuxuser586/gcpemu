package instance

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Saved data of Managed emulators (ADR 0036): Firestore's and Datastore's
// keep their data, Spanner's and Bigtable's do not. The API gateway
// records, in the Store, each project and database it forwards to one that
// keeps data, as the emulator cannot list them. When a Persistent Instance
// stops cleanly, it exports each recorded database through the emulator's
// HTTP API while the container still runs, and archives the exports as
// that API's saved data in the data directory; when it starts, it imports
// them before the emulator is ready. State snapshots hold the same
// archives. Reset deletes the saved data of every API, managed or not.

const (
	// managedDatabasesNS holds the record of databases, keyed
	// API/project/database; Datastore's default database is "".
	managedDatabasesNS = "managed-emulator-databases"
	// managedDataDir, under the data directory, holds each API's saved
	// data, API.tar.gz.
	managedDataDir = "managed-emulators"
	// managedExportDir is where exports are written inside the container;
	// its archive's entries are named under its base name.
	managedExportDir = "/tmp/gcpemu-export"
	// managedManifest, in managedExportDir, lists the exports.
	managedManifest = "databases.json"
	// managedSaveTimeout bounds the whole export step of a stop.
	managedSaveTimeout = 20 * time.Second
)

// managedExport is one database's export in saved data.
type managedExport struct {
	Project  string `json:"project"`
	Database string `json:"database"`
	// Name is its directory in managedExportDir.
	Name string `json:"name"`
}

// resourceName is the database's name in the emulator's export and import
// requests; Datastore mode wants an empty id for its default database.
func (e managedExport) resourceName() string {
	return "projects/" + e.Project + "/databases/" + e.Database
}

// record keeps, in the Store, a project and database the API gateway
// forwarded to it.
func (m *managedEmulator) record(project, database string) {
	key := m.api + "/" + project + "/" + database
	m.mu.Lock()
	seen := m.recorded[key]
	m.recorded[key] = true
	m.mu.Unlock()
	if seen {
		return
	}
	err := m.st.Update(func(tx store.Tx) error { return tx.Put(managedDatabasesNS, key, []byte("{}")) })
	if err != nil {
		m.mu.Lock()
		delete(m.recorded, key)
		m.mu.Unlock()
		m.log.Error("could not record a database of a Managed emulator; its data may not be kept", "api", m.api, "project", project, "database", database, "err", err)
	}
}

// forget drops what record remembers, as Reset clears the Store.
func (m *managedEmulator) forget() {
	m.mu.Lock()
	clear(m.recorded)
	m.mu.Unlock()
}

// databases returns the recorded databases, sorted.
func (m *managedEmulator) databases() ([]managedExport, error) {
	var out []managedExport
	err := m.st.View(func(tx store.Tx) error {
		tx.Scan(managedDatabasesNS, m.api+"/", func(key string, _ []byte) bool {
			project, database, ok := strings.Cut(strings.TrimPrefix(key, m.api+"/"), "/")
			if ok {
				out = append(out, managedExport{Project: project, Database: database})
			}
			return true
		})
		return nil
	})
	return out, err
}

// savedFile is the path of its saved data.
func (m *managedEmulator) savedFile() string {
	return filepath.Join(m.dataDir, managedDataDir, m.api+".tar.gz")
}

// running returns its runtime, container and address, or false when it is
// not running.
func (m *managedEmulator) running() (*runtime.Manager, string, string, bool) {
	m.mu.Lock()
	ip := m.ip
	m.mu.Unlock()
	if ip == "" {
		return nil, "", "", false
	}
	rt := m.c.Connected()
	if rt == nil {
		return nil, "", "", false
	}
	return rt, rt.Name("external", m.api), ip, true
}

// export exports each recorded database and archives the exports to
// file, replacing it only once the archive is whole.
func (m *managedEmulator) export(ctx context.Context, file string) error {
	rt, id, ip, ok := m.running()
	if !ok {
		return errors.New("it is not running")
	}
	dbs, err := m.databases()
	if err != nil {
		return err
	}
	res, err := rt.Exec(ctx, id, []string{"sh", "-c", "rm -rf " + managedExportDir + " && mkdir -p " + managedExportDir}, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("preparing %s: %s", managedExportDir, bytes.TrimSpace(res.Stderr))
	}
	for i := range dbs {
		dbs[i].Name = strconv.Itoa(i)
		err := m.post(ctx, ip, "/emulator/v1/projects/"+url.PathEscape(dbs[i].Project)+":export", map[string]string{
			"database": dbs[i].resourceName(), "export_directory": managedExportDir, "export_name": dbs[i].Name,
		})
		if err != nil {
			return fmt.Errorf("exporting %s: %w", dbs[i].resourceName(), err)
		}
	}
	b, err := json.Marshal(dbs)
	if err != nil {
		return err
	}
	if err := rt.CopyTo(ctx, id, managedExportDir, []runtime.File{{Name: managedManifest, Data: b}}); err != nil {
		return err
	}
	return archiveExports(ctx, rt, id, file)
}

// archiveExports writes managedExportDir in container id to file as a
// gzipped tar, replacing file only once it is whole.
func archiveExports(ctx context.Context, rt *runtime.Manager, id, file string) (err error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".tmp-")
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(f.Name(), file)
		}
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	zw, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err := rt.ArchiveFrom(ctx, id, managedExportDir, zw); err != nil {
		return err
	}
	return zw.Close()
}

// importSaved imports its saved data, if it has any, into its container
// at ip, and records each database it imports.
func (m *managedEmulator) importSaved(ctx context.Context, rt *runtime.Manager, id, ip string) error {
	f, err := os.Open(m.savedFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", f.Name(), err)
	}
	if err := rt.ArchiveTo(ctx, id, path.Dir(managedExportDir), zr); err != nil {
		return err
	}
	b, err := rt.CopyFrom(ctx, id, path.Join(managedExportDir, managedManifest))
	if err != nil {
		return err
	}
	var dbs []managedExport
	if err := json.Unmarshal(b, &dbs); err != nil {
		return fmt.Errorf("%s: %w", f.Name(), err)
	}
	for _, db := range dbs {
		err := m.post(ctx, ip, "/emulator/v1/projects/"+url.PathEscape(db.Project)+":import", map[string]string{
			"database": db.resourceName(), "export_directory": path.Join(managedExportDir, db.Name, db.Name+".overall_export_metadata"),
		})
		if err != nil {
			return fmt.Errorf("importing %s from %s: %w", db.resourceName(), f.Name(), err)
		}
		m.record(db.Project, db.Database)
	}
	return nil
}

// post sends body as JSON to its REST server at ip, failing unless it
// answers 2xx.
func (m *managedEmulator) post(ctx context.Context, ip, p string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+net.JoinHostPort(ip, strconv.Itoa(m.spec.restPort))+p, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{Transport: &http.Transport{DialContext: m.c.DialContext, DisableKeepAlives: true}}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// warnNotKept logs, on a Persistent Instance, the managed APIs whose data
// is not kept when it stops.
func (in *Instance) warnNotKept() {
	if in.Config.Ephemeral {
		return
	}
	var apis []string
	for _, api := range slices.Sorted(maps.Keys(in.managed)) {
		if !in.managed[api].spec.keepsData {
			apis = append(apis, api)
		}
	}
	if len(apis) > 0 {
		in.Env.Log.Warn("these Managed emulators keep their data in memory only; it is lost when the Instance stops", "apis", strings.Join(apis, ","))
	}
}

// saveManaged exports, on a Persistent Instance, each running Managed
// emulator that keeps data as its saved data, all within
// managedSaveTimeout. One that fails keeps the saved data it had.
func (in *Instance) saveManaged(ctx context.Context) {
	if in.Config.Ephemeral {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, managedSaveTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, m := range in.managed {
		if !m.spec.keepsData {
			continue
		}
		wg.Go(func() {
			if _, _, _, ok := m.running(); !ok {
				return // it never ran, so holds no data
			}
			if err := m.export(ctx, m.savedFile()); err != nil {
				in.Env.Log.Error("could not save the Managed emulator's data; the data saved before is kept, and data written since it started is lost",
					"api", m.api, "err", err)
			}
		})
	}
	wg.Wait()
}

// clearManagedData deletes the saved data of every API (Reset).
func (in *Instance) clearManagedData() error {
	for _, m := range in.managed {
		m.forget()
	}
	return os.RemoveAll(filepath.Join(in.Env.DataDir, managedDataDir))
}

// snapshotManaged exports each running Managed emulator that keeps data
// into dir, as its saved data would be.
func (in *Instance) snapshotManaged(ctx context.Context, dir string) error {
	for _, api := range slices.Sorted(maps.Keys(in.managed)) {
		m := in.managed[api]
		if _, _, _, ok := m.running(); !ok || !m.spec.keepsData {
			continue
		}
		if err := m.export(ctx, filepath.Join(dir, api+".tar.gz")); err != nil {
			return fmt.Errorf("the Managed emulator for %s: %w", api, err)
		}
	}
	return nil
}

// restoreManaged makes what a State snapshot saved in dir the saved data,
// and imports it into each running Managed emulator, whose container
// Reset replaced, as import merges into existing data.
func (in *Instance) restoreManaged(ctx context.Context, dir string) error {
	if err := emu.LinkTree(dir, filepath.Join(in.Env.DataDir, managedDataDir)); err != nil {
		return err
	}
	var errs []error
	for _, api := range slices.Sorted(maps.Keys(in.managed)) {
		m := in.managed[api]
		if !m.spec.keepsData {
			continue
		}
		m.runMu.Lock()
		if rt, id, ip, ok := m.running(); ok {
			if err := m.importSaved(ctx, rt, id, ip); err != nil {
				errs = append(errs, fmt.Errorf("the Managed emulator for %s: %w", api, err))
			}
		}
		m.runMu.Unlock()
	}
	return errors.Join(errs...)
}
