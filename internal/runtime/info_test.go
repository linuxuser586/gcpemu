package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestInfo: Info names the runtime by its operating system and kernel, so
// errors about the kernel (#173) say which VM runtime is meant.
func TestInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/version"):
			_, _ = io.WriteString(w, `{"Version":"27.3.1","Arch":"arm64"}`)
		case strings.HasSuffix(r.URL.Path, "/info"):
			_, _ = io.WriteString(w, `{"OperatingSystem":"Docker Desktop","KernelVersion":"6.10.14-linuxkit","OSType":"linux"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cl, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	in, err := cl.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := in.String(), "docker 27.3.1 (Docker Desktop, kernel 6.10.14-linuxkit)"; got != want {
		t.Errorf("String() = %q; want %q", got, want)
	}
	if in.OSType != "linux" {
		t.Errorf("OSType = %q; want linux", in.OSType)
	}
	if got := (Info{Name: "podman", Version: "5.2.0"}).String(); got != "podman 5.2.0" {
		t.Errorf("without /info: %q", got)
	}
}

// TestInfoCPUSet: Info reports a runtime whose containers lack the cpuset
// cgroup controller, as a rootless Podman machine's do (#632), and only
// then. A Podman's /libpod/info decides over its compat /info, whose
// "CPUSet" is false on a rootful Podman 6 machine whose containers have
// cpuset; without /libpod/info, /info does.
func TestInfoCPUSet(t *testing.T) {
	for _, tc := range []struct {
		info, libpod string
		want         bool
	}{
		{`{"CPUSet":false,"CgroupVersion":"2"}`, ``, true},
		{`{"CPUSet":true,"CgroupVersion":"2"}`, ``, false},
		{`{}`, ``, false},
		{`{"CPUSet":false}`, `{"host":{"cgroupControllers":["cpuset","cpu","io","memory","pids"]}}`, false},
		{`{"CPUSet":false}`, `{"host":{"cgroupControllers":["cpu","io","memory","pids"]}}`, true},
		{`{"CPUSet":false}`, `{"host":{}}`, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/version"):
				_, _ = io.WriteString(w, `{"Version":"6.1.4","Arch":"arm64","Components":[{"Name":"Podman Engine"}]}`)
			case strings.HasSuffix(r.URL.Path, "/libpod/info"):
				if tc.libpod == "" {
					w.WriteHeader(404)
					return
				}
				_, _ = io.WriteString(w, tc.libpod)
			case strings.HasSuffix(r.URL.Path, "/info"):
				_, _ = io.WriteString(w, tc.info)
			default:
				w.WriteHeader(404)
			}
		}))
		cl, err := New("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		in, err := cl.Info(context.Background())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if in.NoCPUSet != tc.want {
			t.Errorf("/info %s, /libpod/info %s: NoCPUSet = %v; want %v", tc.info, tc.libpod, in.NoCPUSet, tc.want)
		}
	}
}
