package build

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/services/ar"
)

// dockerConfigFile is the docker CLI's config in the builder home, which
// Cloud Build fills with its service account's credentials.
var dockerConfigFile = path.Join(home, ".docker", "config.json")

// dockerConfig returns a docker CLI config holding Basic credentials of
// the Build's service account for every LOCATION-docker.pkg.dev, the
// access token as the password of oauth2accesstoken; nil without the iam
// Service to mint the token. The docker CLI sends them to the Build
// daemon with each push, pull and build, so that the daemon reaches
// Artifact Registry as the Build that asked (FR-CB-005).
func (s *Service) dockerConfig(ctx context.Context, b *cloudbuildpb.Build) ([]byte, error) {
	svc, ok := s.env.Lookup("iam")
	keys, _ := svc.(emu.ServiceAccountKeys)
	if !ok || keys == nil {
		return nil, nil
	}
	email, err := serviceAccountEmail(b.GetServiceAccount())
	if err != nil {
		return nil, err
	}
	tok, _, err := keys.AccessToken(ctx, emu.Principal("serviceAccount:"+email))
	if err != nil {
		return nil, fmt.Errorf("an access token for %s: %w", email, err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte("oauth2accesstoken:" + tok))
	auths := map[string]any{}
	for _, loc := range ar.Locations() {
		auths[loc+"-docker.pkg.dev"] = map[string]string{"auth": auth}
	}
	return json.Marshal(map[string]any{"auths": auths})
}

// writeCredentials writes the Build's docker CLI config into its builder
// home, before any step runs.
func (s *Service) writeCredentials(ctx context.Context, rt *runtime.Manager, b *cloudbuildpb.Build, vols buildVolumes) error {
	cfg, err := s.dockerConfig(ctx, b)
	if err != nil || cfg == nil {
		return err
	}
	return s.withFetcher(ctx, rt, b, vols, runtime.ContainerSpec{Image: agentImage, Entrypoint: []string{"/.gcpemu-unused"}}, func(id string) error {
		return rt.CopyTo(ctx, id, "/", []runtime.File{{Name: strings.TrimPrefix(dockerConfigFile, "/"), Mode: 0o600, Data: cfg}})
	})
}
