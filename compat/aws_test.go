//go:build compat

package compat

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// awsCanaryHost is where the aws CLI sends S3 requests without
// AWS_ENDPOINT_URL, in the region awsConfig sets.
const awsCanaryHost = "s3.us-central1.amazonaws.com"

// awsConfig is the aws CLI's config file: path style (out of scope:
// virtual-hosted style), one attempt so the canary fails fast, and a
// multipart threshold low enough that a 6 MiB copy goes multipart. 5 MB is
// the smallest part S3 clients allow.
const awsConfig = `[default]
region = us-central1
max_attempts = 1
s3 =
  addressing_style = path
  multipart_threshold = 5MB
  multipart_chunksize = 5MB
`

// ownerCall sends a JSON API request to the gateway as the emulator's
// default principal and returns the response body.
func ownerCall(t *testing.T, e *emulator, method, path string, body any) []byte {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+e.vars["GCPEMU_GATEWAY"]+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer owner")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, out)
	}
	return out
}

// testAWS runs the aws CLI v2 against the XML API with an HMAC key the
// emulator issued (IF-007, #600).
func testAWS(t *testing.T, e *emulator) {
	aws := onPath(t, "aws")
	logVersion(t, aws, "--version")

	// A service account that may write the bucket, and its HMAC key.
	const bucket = "compat-s3"
	sa := "compat-s3@" + project + ".iam.gserviceaccount.com"
	ownerCall(t, e, "POST", "/iam/v1/projects/"+project+"/serviceAccounts", map[string]string{"accountId": "compat-s3"})
	ownerCall(t, e, "POST", "/storage/v1/b?project="+project, map[string]string{"name": bucket, "location": "us-central1"})
	ownerCall(t, e, "PUT", "/storage/v1/b/"+bucket+"/iam", map[string]any{
		"bindings": []map[string]any{{"role": "roles/storage.objectAdmin", "members": []string{"serviceAccount:" + sa}}},
	})
	var key struct {
		Metadata struct{ AccessID string }
		Secret   string
	}
	if err := json.Unmarshal(ownerCall(t, e, "POST", "/storage/v1/projects/"+project+"/hmacKeys?serviceAccountEmail="+sa, nil), &key); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key.Metadata.AccessID, "GOOG1E") || key.Secret == "" {
		t.Fatalf("HMAC key %+v", key)
	}

	// The developer's AWS profile is never read: every AWS_* variable from
	// the environment is dropped and the config and credentials files are
	// temp files. Requests that would leave the machine reach the guard.
	cfg := t.TempDir()
	writeTree(t, cfg, map[string][]byte{"config": []byte(awsConfig), "credentials": nil})
	endpoint := e.vars["CLOUDSDK_STORAGE_GS_XML_ENDPOINT_URL"]
	env := slices.DeleteFunc(slices.Clone(e.env), func(kv string) bool { return strings.HasPrefix(kv, "AWS_") })
	env = append(env, e.guard.Env()...)
	env = withEnv(env, map[string]string{
		"AWS_CONFIG_FILE":             filepath.Join(cfg, "config"),
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(cfg, "credentials"),
		"AWS_ACCESS_KEY_ID":           key.Metadata.AccessID,
		"AWS_SECRET_ACCESS_KEY":       key.Secret,
		"AWS_ENDPOINT_URL":            endpoint,
		"AWS_EC2_METADATA_DISABLED":   "true",
		"AWS_PAGER":                   "",
	})
	try := func(t *testing.T, env []string, args ...string) (string, error) {
		t.Helper()
		out, escaped, err := e.exec(env, aws, args...)
		for _, h := range escaped {
			t.Errorf("aws %s: Escaped request to %s", strings.Join(args, " "), h)
		}
		return out, err
	}
	run := func(t *testing.T, args ...string) string {
		t.Helper()
		out, err := try(t, env, args...)
		if err != nil {
			t.Fatalf("aws %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	s3 := "s3://" + bucket

	t.Run("escape-guard", func(t *testing.T) {
		out, escaped, err := e.exec(withEnv(env, nil, "AWS_ENDPOINT_URL"), aws, "s3", "ls", s3)
		if err == nil {
			t.Errorf("aws s3 ls without AWS_ENDPOINT_URL succeeded:\n%s", out)
		}
		if !slices.Equal(escaped, []string{awsCanaryHost}) {
			t.Errorf("the Escape guard refused %v, want [%s]; aws said:\n%s", escaped, awsCanaryHost, out)
		}
	})

	t.Run("cp", func(t *testing.T) {
		dir := longTempDir(t)
		small := []byte("small from aws\x00\xff\n")
		big := bytes.Repeat([]byte("0123456789abcdef"), 6<<20/16) // 6 MiB: two parts
		writeTree(t, dir, map[string][]byte{"small.bin": small, "big.bin": big})
		for _, c := range []struct {
			name      string
			content   []byte
			multipart bool
		}{{"small.bin", small, false}, {"big.bin", big, true}} {
			// --debug logs each S3 operation the copy calls.
			out := run(t, "s3", "cp", "--no-progress", "--debug", filepath.Join(dir, c.name), s3+"/cp/"+c.name, "--metadata", "origin=aws,case="+c.name)
			if got := strings.Contains(out, "CompleteMultipartUpload"); got != c.multipart {
				t.Errorf("aws s3 cp %s: multipart = %v, want %v", c.name, got, c.multipart)
			}
			var meta map[string]string
			if err := json.Unmarshal([]byte(run(t, "s3api", "head-object", "--bucket", bucket, "--key", "cp/"+c.name, "--query", "Metadata", "--output", "json")), &meta); err != nil {
				t.Fatal(err)
			}
			if want := map[string]string{"origin": "aws", "case": c.name}; !maps.Equal(meta, want) {
				t.Errorf("%s metadata = %v, want %v", c.name, meta, want)
			}
			back := filepath.Join(dir, "back-"+c.name)
			run(t, "s3", "cp", "--no-progress", s3+"/cp/"+c.name, back)
			if b, _ := os.ReadFile(back); !bytes.Equal(b, c.content) {
				t.Errorf("%s downloaded %d bytes, want the %d uploaded", c.name, len(b), len(c.content))
			}
		}
		contains(t, "aws s3 ls", run(t, "s3", "ls", s3+"/cp/"), "big.bin")
		run(t, "s3", "rm", s3+"/cp/small.bin")
		if out, _ := try(t, env, "s3", "ls", s3+"/cp/"); strings.Contains(out, "small.bin") || !strings.Contains(out, "big.bin") {
			t.Errorf("after aws s3 rm, aws s3 ls says:\n%s", out)
		}
	})

	t.Run("sync", func(t *testing.T) {
		src := longTempDir(t)
		tree := map[string][]byte{
			"top.txt":                      []byte("top\n"),
			"sub/gone.txt":                 []byte("deleted on the second sync\n"),
			"sub/deep/bytes.bin":           {0, 1, 2, 0xfe, 0xff, '\n', '\r', 0},
			"sub/deep/name with space.txt": []byte("spaced\n"),
		}
		writeTree(t, src, tree)
		dst := s3 + "/tree"
		listed := func() {
			t.Helper()
			var keys []string
			out := run(t, "s3api", "list-objects-v2", "--bucket", bucket, "--prefix", "tree/", "--query", "Contents[].Key", "--output", "json")
			if err := json.Unmarshal([]byte(out), &keys); err != nil {
				t.Fatalf("list-objects-v2: %v\n%s", err, out)
			}
			var want []string
			for name := range tree {
				want = append(want, "tree/"+name)
			}
			slices.Sort(keys)
			if slices.Sort(want); !slices.Equal(keys, want) {
				t.Fatalf("objects under %s: %q, want %q", dst, keys, want)
			}
		}
		run(t, "s3", "sync", "--no-progress", src, dst)
		listed()

		// Another size, so sync sees the change whatever the mtime
		// granularity.
		tree["top.txt"] = []byte("top, changed\n")
		delete(tree, "sub/gone.txt")
		writeTree(t, src, map[string][]byte{"top.txt": tree["top.txt"]})
		if err := os.Remove(filepath.Join(src, "sub", "gone.txt")); err != nil {
			t.Fatal(err)
		}
		run(t, "s3", "sync", "--no-progress", "--delete", src, dst)
		listed()

		down := longTempDir(t)
		run(t, "s3", "sync", "--no-progress", dst, down)
		if got := readTree(t, down); !treesEqual(got, tree) {
			t.Fatalf("synced back down: %q, want %q", got, tree)
		}
	})

	content := []byte("presigned by aws\x00\xff\n")
	f := filepath.Join(longTempDir(t), "signed.bin")
	if err := os.WriteFile(f, content, 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "s3", "cp", "--no-progress", f, s3+"/signed.bin")
	// aws s3 presign signs locally, so a URL signed with a bad key is how
	// to see the XML API's own status code.
	presign := func(t *testing.T, env []string) string {
		t.Helper()
		out, err := try(t, env, "s3", "presign", s3+"/signed.bin", "--expires-in", "600")
		if err != nil {
			t.Fatalf("aws s3 presign: %v\n%s", err, out)
		}
		u := strings.TrimSpace(out)
		if !strings.HasPrefix(u, endpoint+"/"+bucket+"/signed.bin?") {
			t.Fatalf("presigned URL %q is not on %s", u, endpoint)
		}
		return u
	}
	// refused fails unless the CLI fails with code and a URL presigned in
	// env gets a 403.
	refused := func(t *testing.T, env []string, code string) {
		t.Helper()
		out, err := try(t, env, "s3", "ls", s3)
		if err == nil || !strings.Contains(out, "("+code+")") {
			t.Errorf("aws s3 ls = %v, want a %s error:\n%s", err, code, out)
		}
		if status, body := plainGet(t, presign(t, env)); status != http.StatusForbidden || !strings.Contains(string(body), "<Code>"+code+"</Code>") {
			t.Errorf("GET of the presigned URL = %d %s, want 403 %s", status, body, code)
		}
	}

	t.Run("presign", func(t *testing.T) {
		u := presign(t, env)
		if status, body := plainGet(t, u); status != http.StatusOK || !bytes.Equal(body, content) {
			t.Fatalf("GET of the presigned URL = %d %q, want 200 %q", status, body, content)
		}
		before, sig, ok := strings.Cut(u, "X-Amz-Signature=")
		if !ok || sig == "" {
			t.Fatalf("no X-Amz-Signature in %s", u)
		}
		digit := "0"
		if sig[0] == '0' {
			digit = "1"
		}
		if status, body := plainGet(t, before+"X-Amz-Signature="+digit+sig[1:]); status != http.StatusForbidden {
			t.Errorf("GET with a modified signature = %d %s, want 403", status, body)
		}
	})

	t.Run("bad-key", func(t *testing.T) {
		t.Run("wrong-secret", func(t *testing.T) {
			refused(t, withEnv(env, map[string]string{"AWS_SECRET_ACCESS_KEY": "x" + key.Secret}), "SignatureDoesNotMatch")
		})
		hmacKey := "/storage/v1/projects/" + project + "/hmacKeys/" + key.Metadata.AccessID
		t.Run("inactive", func(t *testing.T) {
			ownerCall(t, e, "PUT", hmacKey, map[string]string{"state": "INACTIVE"})
			refused(t, env, "InvalidAccessKeyId")
		})
		t.Run("deleted", func(t *testing.T) {
			// Only an inactive key can be deleted.
			ownerCall(t, e, "PUT", hmacKey, map[string]string{"state": "INACTIVE"})
			ownerCall(t, e, "DELETE", hmacKey, nil)
			refused(t, env, "InvalidAccessKeyId")
		})
	})
}
