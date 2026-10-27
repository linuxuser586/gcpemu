//go:build compat

package compat

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// javaProbe builds the Maven project in sdk/java, whose library versions
// libraries-bom pins, and returns java and the probe's classpath, or calls
// missing. Maven resolves into ~/.m2 outside the Escape guard; only the
// probe's own JVM runs behind it.
func javaProbe(t *testing.T) (java, classpath string) {
	t.Helper()
	java, mvn := onPath(t, "java"), onPath(t, "mvn")
	logVersion(t, java, "-version")
	logVersion(t, mvn, "--version")
	dir := t.TempDir()
	cpFile := filepath.Join(dir, "classpath.txt")
	mvnArgs := []string{"-q", "-B", "-f", filepath.Join("sdk", "java", "pom.xml"), "-DbuildDir=" + dir}
	// Resolving downloads the libraries, so a failure is a toolchain that
	// won't install; compiling offline afterwards must not fail.
	if out, err := exec.Command(mvn, append(mvnArgs, "dependency:build-classpath", "-Dmdep.outputFile="+cpFile, "-Dmdep.includeScope=runtime")...).CombinedOutput(); err != nil {
		missing(t, "mvn dependency:build-classpath: %v\n%s", err, out)
	}
	if out, err := exec.Command(mvn, append(mvnArgs, "-o", "compile")...).CombinedOutput(); err != nil {
		t.Fatalf("mvn compile: %v\n%s", err, out)
	}
	deps, err := os.ReadFile(cpFile)
	if err != nil {
		t.Fatal(err)
	}
	return java, filepath.Join(dir, "classes") + string(os.PathListSeparator) + strings.TrimSpace(string(deps))
}

// javaEnv returns env with the Escape guard's JAVA_TOOL_OPTIONS, plus a
// user.home without gcloud's application-default credentials: the JVM
// finds that file from user.home, not HOME, and `gcpemu env` ran without
// it (#247).
func javaEnv(t *testing.T, e *emulator, env []string) []string {
	t.Helper()
	out := slices.Clone(env)
	for _, kv := range e.guard.JavaEnv() {
		if v, ok := strings.CutPrefix(kv, "JAVA_TOOL_OPTIONS="); ok {
			kv = "JAVA_TOOL_OPTIONS=" + v + " -Duser.home=" + t.TempDir()
		}
		out = append(out, kv)
	}
	return withEnv(out, nil, adcEnvVar)
}

// testJava covers #602 (IF-008): the Java Storage and Pub/Sub libraries
// with only the variables `gcpemu env` prints. The Storage library gzips
// its request bodies, which the Instance decodes. Pub/Sub over the
// emulator's plaintext channel names Google's universe, which the README
// documents.
func testJava(t *testing.T, e *emulator) {
	java, cp := javaProbe(t)
	env := javaEnv(t, e, e.env)

	t.Run("storage+pubsub", func(t *testing.T) {
		got, escaped := probeResults(t, e, env, java, "-cp", cp, "Probe", project, "1")
		expectProbe(t, got, "storage", "OK java-compat-1")
		expectProbe(t, got, "pubsub", "OK")
		if len(escaped) > 0 {
			t.Errorf("the Escape guard refused %v", escaped)
		}
	})

	// The canary: without the overrides, each library's request reaches
	// the guard. Without it, a JVM that ignored JAVA_TOOL_OPTIONS would
	// pass every step. The Storage library ignores the universe and goes
	// to Google; Pub/Sub's gRPC channel goes to the Invalid universe.
	t.Run("escape-guard", func(t *testing.T) {
		got, escaped := probeResults(t, e, env, java, "-cp", cp, "Probe", project, "1", "canary")
		expectProbe(t, got, "storage-no-override", "Proxy returns \"HTTP/1.1 403 Forbidden\"")
		expectProbe(t, got, "pubsub-no-override", "HttpProxyHandler")
		if want := []string{"storage.googleapis.com", "pubsub.gcpemu.invalid"}; !slices.Equal(escaped, want) {
			t.Errorf("the Escape guard refused %v, want %v", escaped, want)
		}
	})
}
