package proxy

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Point-in-time recovery (FR-SQL-008). The emulator gives a clone's
// container, before it starts, a restore bundle in Dir: a gzipped tar of
// one base backup (base/<id>/base.tar.gz, from pg_basebackup -Ft -z) and
// the source's archived WAL (wal/), plus the recovery target time. On a
// fresh data directory the agent lays the base backup out as PGDATA and
// sets PostgreSQL up to replay the WAL up to the target and promote; the
// emulator then applies the clone's own settings.
const (
	RestoreBundle = "restore.tar.gz"
	RestoreTarget = "restore-target"
)

// prepareRestore unpacks the restore bundle in dir into pgdata unless
// pgdata already holds a cluster. It reports whether it did.
func prepareRestore(pgdata, dir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(dir, RestoreBundle)); err != nil {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(pgdata, "PG_VERSION")); err == nil {
		return false, nil
	}
	restore := filepath.Join(dir, "restore")
	if err := untgz(filepath.Join(dir, RestoreBundle), restore); err != nil {
		return false, fmt.Errorf("unpack restore bundle: %w", err)
	}
	bases, _ := filepath.Glob(filepath.Join(restore, "base", "*", "base.tar.gz"))
	if len(bases) != 1 {
		return false, fmt.Errorf("restore bundle holds %d base backups, want 1", len(bases))
	}
	// Parents stay traversable, as the image's entrypoint leaves them
	// (from PostgreSQL 18 PGDATA is /var/lib/postgresql/18/docker).
	if err := os.MkdirAll(filepath.Dir(pgdata), 0o755); err != nil {
		return false, err
	}
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		return false, err
	}
	if err := os.Chmod(pgdata, 0o700); err != nil {
		return false, err
	}
	if err := untgz(bases[0], pgdata); err != nil {
		return false, fmt.Errorf("unpack base backup: %w", err)
	}
	target, err := os.ReadFile(filepath.Join(dir, RestoreTarget))
	if err != nil {
		return false, err
	}
	conf := "# Point-in-time recovery by gcpemu; reset once recovery is done.\n" +
		"restore_command = 'cp " + filepath.Join(restore, "wal") + "/%f \"%p\"'\n" +
		"recovery_target_time = '" + strings.TrimSpace(string(target)) + "'\n" +
		"recovery_target_action = 'promote'\n"
	if err := os.WriteFile(filepath.Join(pgdata, "postgresql.auto.conf"), []byte(conf), 0o600); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(pgdata, "recovery.signal"), nil, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// untgz extracts a gzipped tar into dir, refusing entries outside it.
func untgz(file, dir string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("entry %q is outside the target directory", h.Name)
		}
		p := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(h.Mode).Perm()|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			// pg_basebackup -Ft stores pg_wal and tablespaces as directories
			// or separate archives; links are not expected.
			return fmt.Errorf("entry %q is a symbolic link", h.Name)
		}
	}
}

// prepareDataDirs creates the archive directories and, for a clone,
// restores its base backup; everything is handed to the postgres user,
// which runs the server and the archive command.
func prepareDataDirs() error {
	for _, d := range []string{filepath.Join(ArchiveDir, "wal"), filepath.Join(ArchiveDir, "base")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	paths := []string{ArchiveDir}
	if pgdata := os.Getenv("PGDATA"); pgdata != "" {
		ok, err := prepareRestore(pgdata, Dir)
		if err != nil {
			return err
		}
		if ok {
			paths = append(paths, pgdata, filepath.Join(Dir, "restore"))
		}
	}
	u, err := user.Lookup("postgres")
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	for _, p := range paths {
		if err := filepath.WalkDir(p, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Lchown(path, uid, gid)
		}); err != nil {
			return err
		}
	}
	return nil
}
