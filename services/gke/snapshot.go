package gke

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// State snapshots (FR-CORE-012) keep each RUNNING cluster's k3s server
// datastore: the server container's server/ directory (the kine sqlite
// database, the cluster CA and credentials, the join token), copied with
// the container paused, as a gzipped tar under clusters/<cluster ID>.tar.gz.
// Restoring stages each archive in the service dir; startServer writes it
// into the server's fresh volume before the container starts, so a
// snapshot does not depend on the runtime's volume names and restores into
// another Instance too. Node volumes (images, local-path PersistentVolume
// data) are not saved; recreated nodes rejoin with their recorded names
// and passwords.

var _ emu.Snapshotter = (*Service)(nil)

// k3sServerDir holds the server's Kubernetes state.
const k3sServerDir = k3sDataDir + "/server"

// snapshotOp is what a cluster's operation lock names while its datastore
// is saved.
const snapshotOp = "state-snapshot"

// archiveFile is a cluster's datastore archive in dir.
func archiveFile(dir, id string) string { return filepath.Join(dir, id+".tar.gz") }

// archivesDir is the directory of the archives in a State snapshot.
func archivesDir(dir string) string { return filepath.Join(dir, "clusters") }

// Snapshot implements emu.Snapshotter. Clusters that are not RUNNING
// (provisioning, reconciling, stopping or failed) are skipped and restore
// with a fresh data plane.
func (s *Service) Snapshot(ctx context.Context, dir string) error {
	var keys []string
	_ = s.env.Store.View(func(tx store.Tx) error {
		for _, r := range listClusters(tx, "") {
			if r.cluster().Status == containerpb.Cluster_RUNNING {
				keys = append(keys, r.key())
			}
		}
		return nil
	})
	if len(keys) == 0 {
		return nil
	}
	d, err := s.deps(ctx)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := s.hold(ctx, key, snapshotOp); err != nil {
			return fmt.Errorf("cluster %s is busy: %w", key, err)
		}
		err := s.saveCluster(ctx, d, key, dir)
		s.release(key, snapshotOp)
		if err != nil {
			return fmt.Errorf("cluster %s: %w", key, err)
		}
	}
	return nil
}

// saveCluster archives a cluster's datastore if it is still RUNNING. The
// caller holds the cluster's operation lock.
func (s *Service) saveCluster(ctx context.Context, d *deps, key, dir string) error {
	rec, err := s.loadKey(key)
	if err != nil {
		return nil // deleted meanwhile
	}
	c := rec.cluster()
	if c.Status != containerpb.Cluster_RUNNING {
		return nil
	}
	name := d.rt.Name("gke", shortID(c), "cp")
	return archiveServer(ctx, d.rt, name, archiveFile(archivesDir(dir), c.Id))
}

// serverArchiver is the part of the runtime archiveServer uses.
type serverArchiver interface {
	PauseContainer(ctx context.Context, id string) error
	UnpauseContainer(ctx context.Context, id string) error
	ArchiveFrom(ctx context.Context, id, p string, w io.Writer) error
}

// archiveServer writes the server container's server/ directory to file
// as a gzipped tar. The container is paused while it is copied, so the
// copy is crash consistent (SQLite's WAL recovers it), and unpaused even
// when the copy fails.
func archiveServer(ctx context.Context, rt serverArchiver, id, file string) (err error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp, file)
		}
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if err := rt.PauseContainer(ctx, id); err != nil {
		return fmt.Errorf("pausing the k3s server: %w", err)
	}
	zw, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	err = rt.ArchiveFrom(ctx, id, k3sServerDir, zw)
	if uerr := rt.UnpauseContainer(context.WithoutCancel(ctx), id); uerr != nil {
		err = errors.Join(err, fmt.Errorf("unpausing the k3s server: %w", uerr))
	}
	if err != nil {
		return err
	}
	return zw.Close()
}

// Restore implements emu.Snapshotter: Reset tore the data planes down and
// cleared the staged datastores; each saved datastore is staged and the
// restored clusters are brought up again in the background, reporting
// RECONCILING until they are back.
func (s *Service) Restore(ctx context.Context, dir string) error {
	sd, err := s.stagedDir()
	if err != nil {
		return err
	}
	if err := emu.LinkTree(archivesDir(dir), sd); err != nil {
		return err
	}
	s.recoverClusters()
	return nil
}

// stagedDir holds the staged datastores: datastore archives restored from a
// State snapshot and not yet written into their cluster's server volume.
func (s *Service) stagedDir() (string, error) {
	d, err := s.env.ServiceDir("gke")
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "staged"), nil
}

// stagedDatastore returns the path of a cluster's staged datastore, or ""
// if it has none.
func (s *Service) stagedDatastore(id string) (string, error) {
	sd, err := s.stagedDir()
	if err != nil {
		return "", err
	}
	p := archiveFile(sd, id)
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return p, nil
}

// clearStaged drops every staged datastore.
func (s *Service) clearStaged() error {
	sd, err := s.stagedDir()
	if err != nil {
		return err
	}
	return os.RemoveAll(sd)
}

// openArchive opens a datastore archive as its tar stream.
func openArchive(p string) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return archiveReader{zr, f}, nil
}

type archiveReader struct {
	*gzip.Reader
	f *os.File
}

func (r archiveReader) Close() error {
	return errors.Join(r.Reader.Close(), r.f.Close())
}

// writeDatastore extracts a datastore archive into the k3s data volume of the
// created, not yet started server container id.
func writeDatastore(ctx context.Context, rt *runtime.Manager, id, staged string) error {
	r, err := openArchive(staged)
	if err != nil {
		return err
	}
	defer r.Close()
	return rt.ArchiveTo(ctx, id, k3sDataDir, r)
}
