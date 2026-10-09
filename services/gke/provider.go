package gke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/secrets-store-csi-driver/provider/v1alpha1"
)

// The `gke` Secrets Store CSI provider (secretManagerConfig). The node
// agent serves it on the provider socket of every node, whether or not the
// add-on is enabled; each mount is answered by the emulator
// (serveProviderMount), so the provider holds no credentials.

const providerSocket = "/var/run/secrets-store-csi-providers/gke.sock"

type csiProvider struct {
	v1alpha1.UnimplementedCSIDriverProviderServer
	emu string
}

// serveProvider listens on the provider socket until ctx is done.
func serveProvider(ctx context.Context, emu string) error {
	if err := os.MkdirAll(filepath.Dir(providerSocket), 0o755); err != nil {
		return err
	}
	_ = os.Remove(providerSocket)
	l, err := net.Listen("unix", providerSocket)
	if err != nil {
		return err
	}
	srv := grpc.NewServer()
	v1alpha1.RegisterCSIDriverProviderServer(srv, &csiProvider{emu: emu})
	go func() {
		<-ctx.Done()
		srv.Stop()
	}()
	go func() { _ = srv.Serve(l) }()
	return nil
}

func (p *csiProvider) Version(ctx context.Context, req *v1alpha1.VersionRequest) (*v1alpha1.VersionResponse, error) {
	return &v1alpha1.VersionResponse{Version: "v1alpha1", RuntimeName: "gcpemu-gke", RuntimeVersion: "v1"}, nil
}

func (p *csiProvider) Mount(ctx context.Context, req *v1alpha1.MountRequest) (*v1alpha1.MountResponse, error) {
	mode := int32(0o644)
	if req.GetPermission() != "" {
		m, err := strconv.ParseInt(req.GetPermission(), 10, 32)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "bad permission %q", req.GetPermission())
		}
		mode = int32(m)
	}
	body, _ := json.Marshal(providerMount{Attributes: req.GetAttributes()})
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.emu+"/mount", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "emulator: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, status.Errorf(codes.Internal, "failed to fetch secrets: %s", bytes.TrimSpace(b))
	}
	var out struct {
		Files []secretFile `json:"files"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("emulator response: %w", err)
	}
	r := &v1alpha1.MountResponse{}
	for _, f := range out.Files {
		r.Files = append(r.Files, &v1alpha1.File{Path: f.Path, Mode: mode, Contents: f.Contents})
		r.ObjectVersion = append(r.ObjectVersion, &v1alpha1.ObjectVersion{Id: f.Resource, Version: f.Version})
	}
	return r, nil
}
