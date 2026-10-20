package ca

import (
	"bytes"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCA(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := c.ServerCert("app.example.test", "127.0.0.9")
	if err != nil {
		t.Fatal(err)
	}
	opts := x509.VerifyOptions{Roots: c.Pool(), DNSName: "app.example.test"}
	if _, err := cert.Leaf.Verify(opts); err != nil {
		t.Fatalf("verify: %v", err)
	}
	again, _ := c.ServerCert("127.0.0.9", "app.example.test")
	if again != cert {
		t.Error("server cert not cached")
	}

	// Reload keeps the same root.
	c2, err := Load(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Certificate().Equal(c.Certificate()) {
		t.Error("reload produced a different root")
	}
	if _, err := cert.Leaf.Verify(x509.VerifyOptions{Roots: c2.Pool(), DNSName: "app.example.test"}); err != nil {
		t.Errorf("verify after reload: %v", err)
	}

	// Rotation invalidates old leaves.
	if err := c2.Rotate("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Leaf.Verify(x509.VerifyOptions{Roots: c2.Pool(), DNSName: "app.example.test"}); err == nil {
		t.Error("old leaf verified against rotated root")
	}

	client, err := c2.Issue(Leaf{DNSNames: []string{"lb-client"}, ClientOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Leaf.Verify(x509.VerifyOptions{Roots: c2.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client cert: %v", err)
	}
}

// TestLoad covers how Load treats each combination of ca.pem and ca-key.pem
// in dir: it creates the Instance CA only when neither file exists, and every
// error leaves dir exactly as it was.
func TestLoad(t *testing.T) {
	// A CA whose files the rows copy into dir.
	srcDir := t.TempDir()
	src, err := Load(srcDir, "src")
	if err != nil {
		t.Fatal(err)
	}
	srcCert := readFile(t, filepath.Join(srcDir, CertFile))
	srcKey := readFile(t, filepath.Join(srcDir, KeyFile))

	tests := []struct {
		name string
		// setup prepares dir, which doesn't exist yet.
		setup func(t *testing.T, dir string)
		// wantErr lists substrings of the error, with {dir} standing for dir;
		// nil means Load succeeds.
		wantErr []string
		// wantSrc means the loaded root is the src CA; otherwise a successful
		// Load creates a new one.
		wantSrc bool
	}{
		{
			name:  "neither file",
			setup: func(t *testing.T, dir string) { mkdir(t, dir) },
		},
		{
			name:  "missing dir",
			setup: func(t *testing.T, dir string) {},
		},
		{
			name: "both files matching",
			setup: func(t *testing.T, dir string) {
				mkdir(t, dir)
				writeTestFile(t, filepath.Join(dir, CertFile), srcCert)
				writeTestFile(t, filepath.Join(dir, KeyFile), srcKey)
			},
			wantSrc: true,
		},
		{
			name: "only ca.pem",
			setup: func(t *testing.T, dir string) {
				mkdir(t, dir)
				writeTestFile(t, filepath.Join(dir, CertFile), srcCert)
			},
			wantErr: []string{"no " + KeyFile, "{dir}", "gcpemu ca install"},
		},
		{
			name: "only ca-key.pem",
			setup: func(t *testing.T, dir string) {
				mkdir(t, dir)
				writeTestFile(t, filepath.Join(dir, KeyFile), srcKey)
			},
			wantErr: []string{"no " + CertFile, "{dir}", "gcpemu ca install"},
		},
		{
			name: "files from different CAs",
			setup: func(t *testing.T, dir string) {
				if _, err := Load(dir, "other"); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(dir, CertFile), srcCert)
			},
			wantErr: []string{CertFile + " in {dir} does not match " + KeyFile, "gcpemu ca install"},
		},
		{
			name: "ca-key.pem unreadable",
			setup: func(t *testing.T, dir string) {
				mkdir(t, dir)
				writeTestFile(t, filepath.Join(dir, CertFile), srcCert)
				mkdir(t, filepath.Join(dir, KeyFile))
			},
			wantErr: []string{filepath.Join("{dir}", KeyFile)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "instance")
			tt.setup(t, dir)
			before := snapshot(t, dir)

			c, err := Load(dir, "test")

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if tt.wantSrc && !c.Certificate().Equal(src.Certificate()) {
					t.Error("Load did not load the existing Instance CA")
				}
				if !tt.wantSrc && c.Certificate().Equal(src.Certificate()) {
					t.Error("Load reused the src Instance CA instead of creating one")
				}
				if got := readFile(t, c.Path()); !bytes.Equal(got, c.PEM()) {
					t.Error("ca.pem on disk is not the loaded Instance CA")
				}
				return
			}
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			for _, want := range tt.wantErr {
				want = strings.ReplaceAll(want, "{dir}", dir)
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if after := snapshot(t, dir); !reflect.DeepEqual(after, before) {
				t.Errorf("dir changed:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// snapshot returns the regular files in dir and their contents; a
// subdirectory is recorded by name only.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			m[e.Name()+"/"] = ""
			continue
		}
		m[e.Name()] = string(readFile(t, filepath.Join(dir, e.Name())))
	}
	return m
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeTestFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}
