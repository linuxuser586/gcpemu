//go:build compat

package compat

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	emuruntime "github.com/linuxuser586/gcpemu/internal/runtime"
)

// testBuilds runs gcloud builds and the Go Cloud Build client (#393)
// against an Instance without Host mode. gcloud can't tail the Build log
// there (SRS 11.3), so each submit that waits sets options.logging NONE
// and the others pass --async; --suppress-logs still reads the log.
func testBuilds(t *testing.T, e *emulator, g string) {
	src := longTempDir(t)
	_ = os.WriteFile(filepath.Join(src, "hello.txt"), []byte("from the source\n"), 0o644)
	_ = os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM busybox:1.37\nCOPY hello.txt /\n"), 0o644)
	e.run(t, g, "artifacts", "repositories", "create", "compat-builds", "--repository-format=docker", "--location=us-central1")

	t.Run("submit--config", func(t *testing.T) {
		config := filepath.Join(t.TempDir(), "cloudbuild.yaml")
		_ = os.WriteFile(config, []byte("steps:\n- name: busybox:1.37\n  id: greet\n  args: [cat, hello.txt]\noptions:\n  logging: NONE\n"), 0o644)
		out := e.run(t, g, "builds", "submit", "--config", config, "--format=value(id,logsBucket)", src)
		fields := strings.Fields(out)
		if len(fields) < 2 {
			t.Fatalf("builds submit printed %q", out)
		}
		id, bucket := fields[len(fields)-2], fields[len(fields)-1]
		contains(t, "builds describe", e.run(t, g, "builds", "describe", id, "--format=value(status)"), "SUCCESS")
		log := e.run(t, g, "storage", "cat", bucket+"/log-"+id+".txt")
		contains(t, "the Build log", log, `Step #0 - "greet": from the source`)
		contains(t, "the Build log", log, "DONE")

		_ = os.WriteFile(config, []byte("steps:\n- name: busybox:1.37\n  args: [sh, -c, 'exit 3']\noptions:\n  logging: NONE\n"), 0o644)
		if out, err := e.try(t, g, "builds", "submit", "--config", config, src); err == nil {
			t.Fatalf("builds submit of a failing Build exited 0:\n%s", out)
		}
		if id := submitAsync(t, e, g, "--config", config, src); id == "" {
			t.Fatal("builds submit --async printed no Build ID")
		}
	})

	// --tag builds the Dockerfile with gcr.io/cloud-builders/docker on the
	// Build daemon and pushes the image to Artifact Registry (FR-CB-004,
	// FR-CB-006).
	t.Run("submit--tag", func(t *testing.T) {
		img := "us-central1-docker.pkg.dev/" + project + "/compat-builds/tagged"
		id := submitAsync(t, e, g, "--tag", img+":v1", src)
		if status := waitBuild(t, e, g, id); status != "SUCCESS" {
			t.Fatalf("builds submit --tag ended %s", status)
		}
		digest := strings.TrimSpace(e.run(t, g, "builds", "describe", id, "--format=value(results.images[0].digest)"))
		contains(t, "artifacts docker images list", e.run(t, g, "artifacts", "docker", "images", "list", img, "--format=value(version)"), digest)
	})

	t.Run("cancel+list", func(t *testing.T) {
		config := filepath.Join(t.TempDir(), "cloudbuild.yaml")
		_ = os.WriteFile(config, []byte("steps:\n- name: busybox:1.37\n  args: [sleep, '600']\n"), 0o644)
		id := submitAsync(t, e, g, "--config", config, "--no-source")
		e.run(t, g, "builds", "cancel", id)
		if status := waitBuild(t, e, g, id); status != "CANCELLED" {
			t.Fatalf("a cancelled Build ended %s", status)
		}
		contains(t, "builds list", e.run(t, g, "builds", "list", "--filter=status=CANCELLED", "--format=value(id)"), id)
		if out := e.run(t, g, "builds", "list", "--filter=status=SUCCESS", "--format=value(id)"); strings.Contains(out, id) {
			t.Fatalf("builds list --filter=status=SUCCESS lists the cancelled Build %s:\n%s", id, out)
		}
	})

	// A Build trigger whose inline Build takes its source from Cloud
	// Storage, which triggers run builds (FR-CB-012).
	t.Run("triggers", func(t *testing.T) {
		e.run(t, g, "storage", "buckets", "create", "gs://compat-trigger-src", "--location=us-central1")
		tgz := filepath.Join(t.TempDir(), "src.tgz")
		writeTarball(t, tgz, map[string]string{"hello.txt": "from the trigger source\n"})
		e.run(t, g, "storage", "cp", tgz, "gs://compat-trigger-src/src.tgz")
		cfg := filepath.Join(t.TempDir(), "trigger.yaml")
		_ = os.WriteFile(cfg, []byte(`name: compat-trigger
description: Built from Cloud Storage.
substitutions:
  _GREETING: hello
build:
  source:
    storageSource:
      bucket: compat-trigger-src
      object: src.tgz
  steps:
  - name: busybox:1.37
    entrypoint: sh
    args: [-c, 'test "$(cat hello.txt)" = "from the trigger source" && echo $_GREETING $TRIGGER_NAME']
`), 0o644)
		e.run(t, g, "builds", "triggers", "create", "manual", "--trigger-config", cfg)
		contains(t, "triggers list", e.run(t, g, "builds", "triggers", "list", "--format=value(name)"), "compat-trigger")
		f := strings.Fields(e.run(t, g, "builds", "triggers", "run", "compat-trigger", "--substitutions=_GREETING=hi", "--format=value(metadata.build.id)"))
		if len(f) == 0 {
			t.Fatal("triggers run printed no Build ID")
		}
		id := f[len(f)-1]
		if status := waitBuild(t, e, g, id); status != "SUCCESS" {
			t.Fatalf("the Build of triggers run ended %s", status)
		}
		bucket := strings.TrimSpace(e.run(t, g, "builds", "describe", id, "--format=value(logsBucket)"))
		contains(t, "the triggered Build's log", e.run(t, g, "storage", "cat", bucket+"/log-"+id+".txt"), "hi compat-trigger")
		e.run(t, g, "builds", "triggers", "delete", "compat-trigger")
	})

	t.Run("go-client", func(t *testing.T) {
		e.run(t, g, "storage", "buckets", "create", "gs://compat-build-artifacts", "--location=us-central1")
		bin := filepath.Join(t.TempDir(), exe("buildgo"))
		if out, err := exec.Command("go", "build", "-tags", "compat", "-o", bin, "./sdk/buildgo").CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
		out, escaped, _ := e.exec(append(e.env, e.guard.Env()...), bin, project, "compat-build-artifacts")
		for _, h := range escaped {
			t.Errorf("the Go Cloud Build client: Escaped request to %s", h)
		}
		f := strings.Fields(out)
		if len(f) != 5 || f[0] != "build" || f[1] != "OK" || f[2] != "SUCCESS" || f[3] != "1" {
			t.Fatalf("the probe said: %s", out)
		}
		contains(t, "the artifact", e.run(t, g, "storage", "cat", "gs://compat-build-artifacts/artifacts/out.txt"), "from go")
		contains(t, "the artifact manifest", e.run(t, g, "storage", "cat", f[4]), "out.txt")
	})
}

// submitAsync runs gcloud builds submit --async and returns the Build ID.
func submitAsync(t *testing.T, e *emulator, g string, args ...string) string {
	t.Helper()
	out := e.run(t, g, append([]string{"builds", "submit", "--async", "--format=value(id)"}, args...)...)
	// gcloud's progress precedes the ID in the output.
	f := strings.Fields(out)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// waitBuild returns the status a Build ends in.
func waitBuild(t *testing.T, e *emulator, g, id string) string {
	t.Helper()
	status := ""
	for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		if status = strings.TrimSpace(e.run(t, g, "builds", "describe", id, "--format=value(status)")); status != "QUEUED" && status != "WORKING" {
			break
		}
	}
	return status
}

// writeTarball writes files as a gzipped tarball, the form of a Build's
// storageSource.
func writeTarball(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))})
		_, _ = io.WriteString(tw, body)
	}
	_ = tw.Close()
	_ = zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHostMode runs gcloud builds submit in Host mode (#393), where gcloud
// streams the Build log from storage.googleapis.com (SRS 11.3). The
// Escape guard tunnels the hostnames Host mode serves to the Google front
// end,
// so the test changes neither /etc/hosts nor the system resolver, and
// gcloud trusts the emulator CA through core/custom_ca_certs_file.
func TestHostMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Host mode is Linux-only")
	}
	if _, err := emuruntime.Detect(); err != nil {
		t.Skip("Host mode needs a container runtime")
	}
	g := onPath(t, "gcloud")
	e := startWith(t, options{services: "iam,gcs,pubsub,secrets,ar,build", hostMode: true})

	// Outside the Invalid universe the canary is an API the Instance
	// doesn't serve, so Host mode doesn't either and the guard refuses it.
	t.Run("gcloud/escape-guard", func(t *testing.T) {
		out, escaped, err := e.exec(e.env, g, "dns", "managed-zones", "list")
		if err == nil {
			t.Errorf("gcloud dns managed-zones list succeeded:\n%s", out)
		}
		if want := "dns.googleapis.com"; !slices.Equal(escaped, []string{want}) {
			t.Errorf("the Escape guard refused %v, want [%s]; gcloud said:\n%s", escaped, want, out)
		}
	})

	t.Run("gcloud/builds", func(t *testing.T) {
		src := longTempDir(t)
		_ = os.WriteFile(filepath.Join(src, "hello.txt"), []byte("from the source\n"), 0o644)
		_ = os.WriteFile(filepath.Join(src, "Dockerfile"), []byte("FROM busybox:1.37\nCOPY hello.txt /\n"), 0o644)
		config := filepath.Join(t.TempDir(), "cloudbuild.yaml")
		_ = os.WriteFile(config, []byte("steps:\n- name: busybox:1.37\n  id: greet\n  args: [cat, hello.txt]\n"), 0o644)
		out := e.run(t, g, "builds", "submit", "--config", config, src)
		contains(t, "the streamed log", out, `Step #0 - "greet": from the source`)
		contains(t, "the streamed log", out, "SUCCESS")

		e.run(t, g, "artifacts", "repositories", "create", "compat-builds", "--repository-format=docker", "--location=us-central1")
		img := "us-central1-docker.pkg.dev/" + project + "/compat-builds/tagged:v1"
		out = e.run(t, g, "builds", "submit", "--tag", img, src)
		contains(t, "the streamed log", out, "COPY hello.txt /")
		contains(t, "the streamed log", out, img)
		contains(t, "the streamed log", out, "SUCCESS")
	})
}
