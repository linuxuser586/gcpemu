package instance

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/gateway"
)

// TestManagedNoRuntime: with no usable container runtime an Instance with
// a Managed emulator starts and is ready, its Delegated API answers
// UNAVAILABLE naming the missing runtime over REST and gRPC alike, and GET
// /_emu/v1/external says why (#624, ADR 0029).
func TestManagedNoRuntime(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix://"+t.TempDir()+"/no-docker.sock")
	cfg := config.Defaults()
	cfg.Ephemeral = true
	cfg.DataDir = t.TempDir()
	cfg.Services = []string{"dns"}
	cfg.Ports["gateway"] = 0
	cfg.External = map[string]string{"firestore": config.Managed}
	in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &fakeSvc{} }}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := in.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer in.Shutdown(ctx)
	if err := in.WaitReady(ctx); err != nil {
		t.Fatalf("not ready with no runtime for a Managed emulator: %v", err)
	}
	gw := in.Env.Endpoints.Get("gateway")
	if v := in.EnvVars()["FIRESTORE_EMULATOR_HOST"]; v != gw {
		t.Errorf("FIRESTORE_EMULATOR_HOST = %q, want the API gateway %s", v, gw)
	}

	resp, body := call(t, http.DefaultClient, http.MethodGet, "http://"+gw+"/v1/projects/p/databases/(default)/documents/c/d", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "container runtime") {
		t.Errorf("REST: %d %s, want 503 naming the container runtime", resp.StatusCode, body)
	}

	cc, err := grpc.NewClient(gw, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	err = cc.Invoke(ctx, "/google.firestore.v1.Firestore/GetDocument", &emptypb.Empty{}, &emptypb.Empty{})
	if s := status.Convert(err); s.Code() != codes.Unavailable || !strings.Contains(s.Message(), "container runtime") {
		t.Errorf("gRPC: %v, want UNAVAILABLE naming the container runtime", err)
	}

	var got struct {
		External []struct {
			API       string `json:"api"`
			Managed   bool   `json:"managed"`
			Reachable bool   `json:"reachable"`
			Error     string `json:"error"`
		} `json:"external"`
	}
	getJSON(t, "http://"+gw+"/_emu/v1/external", nil, &got)
	if len(got.External) != 1 {
		t.Fatalf("external = %+v, want firestore", got.External)
	}
	if fs := got.External[0]; fs.API != "firestore" || !fs.Managed || fs.Reachable || !strings.Contains(fs.Error, "container runtime") {
		t.Errorf("firestore = %+v, want it managed and unreachable for want of a container runtime", fs)
	}
}

// TestManagedSpecs: every API that can be managed has an image to run and
// a variable its clients read for the emulator's address.
func TestManagedSpecs(t *testing.T) {
	for _, api := range config.ManagedAPIs {
		if s := managedSpecs[api]; s.image == "" || s.grpcPort == 0 || s.restPort == 0 {
			t.Errorf("%s: managedSpec %+v, want an image and ports", api, s)
		}
		if gateway.EmulatorHostVar(api) == "" {
			t.Errorf("%s: the API gateway knows no *_EMULATOR_HOST for it", api)
		}
	}
}

// TestManagedNotKeptLogged: a Persistent Instance with Managed emulators
// that keep their data in memory only logs one line naming them when it
// starts; an Ephemeral one does not (#649, ADR 0036).
func TestManagedNotKeptLogged(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix://"+t.TempDir()+"/no-docker.sock")
	for _, ephemeral := range []bool{false, true} {
		cfg := config.Defaults()
		cfg.Ephemeral = ephemeral
		cfg.DataDir = t.TempDir()
		cfg.Services = []string{"dns"}
		cfg.Ports["gateway"] = 0
		cfg.External = map[string]string{"firestore": config.Managed, "spanner": config.Managed, "bigtable": config.Managed}
		var logs bytes.Buffer
		in, err := New(&cfg, map[string]Factory{"dns": func(*emu.Env) emu.Service { return &fakeSvc{} }}, &logs)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := in.Start(ctx); err != nil {
			t.Fatal(err)
		}
		in.Shutdown(ctx)
		cancel()
		var lines []string
		for l := range strings.Lines(logs.String()) {
			if strings.Contains(l, "in memory only") {
				lines = append(lines, l)
			}
		}
		switch {
		case ephemeral && len(lines) != 0:
			t.Errorf("Ephemeral Instance logged %q, want nothing", lines)
		case !ephemeral && (len(lines) != 1 || !strings.Contains(lines[0], "apis=bigtable,spanner")):
			t.Errorf("Persistent Instance logged %q, want one line naming bigtable,spanner", lines)
		}
	}
}
