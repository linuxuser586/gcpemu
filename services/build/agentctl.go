package build

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	"github.com/linuxuser586/gcpemu/internal/agent"
	"github.com/linuxuser586/gcpemu/internal/frontend"
	"github.com/linuxuser586/gcpemu/internal/runtime"
	"github.com/linuxuser586/gcpemu/internal/trust"
	"github.com/linuxuser586/gcpemu/internal/workload"
	"github.com/linuxuser586/gcpemu/services/iam"
)

// The emulator's half of the Build agent (agent.go): the agent's
// container, the endpoints it forwards to, and what each step is given to
// use it.
//
// Emulator endpoints under /cloudbuild/_emu/agent/{secret}/ (not part of
// the Cloud Build API):
//
//	*   md/{ip}/...  the metadata server, as the Build of the step at ip
//
// The secret is new in every emulator process, which replaces the Build
// agent it finds from an earlier one.

// agentImage runs the Build agent.
const agentImage = "busybox:1.38"

// agentReady bounds how long a started agent may take to serve.
const agentReady = 30 * time.Second

// buildNet is the Build network and the Build agent on it.
type buildNet struct {
	name string
	// agent is the agent's address on the network; "" without one.
	agent string
}

// newSecret returns the secret of the Build agent's endpoints.
func newSecret() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// buildAgent returns the Build agent's address on the Build network,
// starting the agent if it is not running. Callers hold netMu.
func (s *Service) buildAgent(ctx context.Context, rt *runtime.Manager, network string) (string, error) {
	if s.agentID != "" {
		if c, err := rt.InspectContainer(ctx, s.agentID); err == nil && c.Running && c.IPs[network] != "" {
			return c.IPs[network], nil
		}
	}
	s.agentID = ""
	np, err := s.env.Containers.Netplane(ctx)
	if err != nil {
		return "", err
	}
	svc, err := np.Services(ctx)
	if err != nil {
		return "", err
	}
	bn, err := rt.InspectNetwork(ctx, network)
	if err != nil {
		return "", err
	}
	gw, err := np.Addr(ctx, "gateway")
	if err != nil {
		return "", err
	}
	cfg := agentConfig{Emu: "http://" + gw + "/cloudbuild/_emu/agent/" + s.agentSecret, Subnet: bn.Subnet}
	if dns, err := np.Addr(ctx, "dns"); err == nil {
		cfg.DNS = dns
	}
	if reg, err := np.Addr(ctx, "ar"); err == nil {
		cfg.Registry = reg
	}
	if fe, err := np.Addr(ctx, frontend.Endpoint); err == nil && s.hosts != nil {
		cfg.Frontend, cfg.FrontendHosts = fe, s.hosts()
	}
	js, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if err := rt.EnsureImage(ctx, agentImage, rt.Offline); err != nil {
		return "", fmt.Errorf("Build agent image %s: %w", agentImage, err)
	}
	bin, err := rt.AgentBinary(ctx)
	if err != nil {
		return "", fmt.Errorf("Build agent: %w", err)
	}
	name := rt.Name("build", "agent")
	_ = rt.RemoveContainer(ctx, name, true)
	id, err := rt.CreateContainer(ctx, runtime.ContainerSpec{
		Name:       name,
		Image:      agentImage,
		Entrypoint: []string{agent.ContainerPath},
		Cmd:        []string{},
		Env:        []string{agent.EnvVar + "=" + agentName, agentConfigEnv + "=" + string(js)},
		Labels:     rt.Labels("build", "", "agent"),
		Hostname:   "build-agent",
		Networks:   []runtime.Attachment{{Network: network}, {Network: svc.Name}},
	})
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		_ = rt.RemoveContainer(context.WithoutCancel(ctx), id, true)
		return "", fmt.Errorf("Build agent: %w", err)
	}
	if err := rt.CopyTo(ctx, id, "/", []runtime.File{{Name: strings.TrimPrefix(agent.ContainerPath, "/"), Mode: 0o755, Path: bin}}); err != nil {
		return fail(err)
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		return fail(err)
	}
	c, err := rt.InspectContainer(ctx, id)
	if err != nil {
		return fail(err)
	}
	if err := waitServing(ctx, np, net.JoinHostPort(c.IPs[svc.Name], "80")); err != nil {
		return fail(err)
	}
	s.agentID = id
	return c.IPs[network], nil
}

// dialer is how the emulator reaches container addresses.
type dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// waitServing waits until addr accepts connections.
func waitServing(ctx context.Context, d dialer, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, agentReady)
	defer cancel()
	for {
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not serving at %s: %w", addr, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// stepEnv is what a step gets to use the Build agent at addr: the agent
// as its resolver, and the variables that point client libraries at the
// metadata server and the CA bundle (stepFiles).
func (s *Service) stepEnv(addr string) (env, hosts []string) {
	env = []string{workload.MetadataHostEnv + "=" + addr, "GCE_METADATA_IP=" + addr}
	if s.env.CA != nil {
		for _, v := range workload.CAEnvVars {
			env = append(env, v+"="+workload.CABundlePath)
		}
	}
	return env, []string{"metadata.google.internal:" + addr, "metadata:" + addr}
}

// stepFiles are the files written into every step: the Instance CA
// bundle, also over the system bundle.
func (s *Service) stepFiles() []runtime.File {
	if s.env.CA == nil {
		return nil
	}
	b := trust.Bundle(s.env.CA.PEM())
	return []runtime.File{
		{Name: strings.TrimPrefix(workload.CABundlePath, "/"), Mode: 0o644, Data: b},
		{Name: strings.TrimPrefix(workload.SystemBundlePath, "/"), Mode: 0o644, Data: b},
	}
}

// serveAgent serves the Build agent's endpoints at path, below
// /_emu/agent/.
func (s *Service) serveAgent(w http.ResponseWriter, r *http.Request, path string) {
	segs := strings.Split(path, "/")
	if len(segs) < 3 || subtle.ConstantTimeCompare([]byte(segs[0]), []byte(s.agentSecret)) != 1 {
		http.NotFound(w, r)
		return
	}
	if segs[1] != "md" {
		http.NotFound(w, r)
		return
	}
	s.serveStepMetadata(w, r, segs[2], "/"+strings.Join(segs[3:], "/"))
}

// metadataHandlers is the IAM Service's metadata server.
type metadataHandlers interface {
	MetadataHandler(id iam.MetadataIdentity) http.Handler
}

// serveStepMetadata answers the metadata server at path for the step at
// ip, as its Build's service account.
func (s *Service) serveStepMetadata(w http.ResponseWriter, r *http.Request, ip, path string) {
	// Client libraries probe GET / and treat an answer without the flavor
	// header as "not on GCE".
	w.Header().Set("Metadata-Flavor", "Google")
	p, ok := s.env.Lookup("iam")
	mh, _ := p.(metadataHandlers)
	if !ok || mh == nil {
		http.Error(w, "the iam service is not running", http.StatusServiceUnavailable)
		return
	}
	b, err := s.stepBuild(r.Context(), ip)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	email, _ := serviceAccountEmail(b.GetServiceAccount())
	r2 := r.Clone(r.Context())
	r2.URL = &url.URL{Path: path, RawQuery: r.URL.RawQuery}
	mh.MetadataHandler(iam.MetadataIdentity{
		Project: b.GetProjectId(),
		Email:   email,
		// Steps reach real hostnames through the Google front end.
		Universe: iam.GoogleUniverse,
	}).ServeHTTP(w, r2)
}

// errNoStep answers a caller that is no running Build step.
var errNoStep = errors.New("no running Build step has this address")

// stepBuild returns the WORKING Build whose step has address ip on the
// Build network. A step that has just started may not be recorded yet,
// so the runtime is asked.
func (s *Service) stepBuild(ctx context.Context, ip string) (*cloudbuildpb.Build, error) {
	s.stepsMu.Lock()
	ref, ok := s.steps[ip]
	s.stepsMu.Unlock()
	if !ok {
		rt, err := s.env.Containers.Runtime(ctx)
		if err != nil {
			return nil, err
		}
		cs, err := rt.ListContainers(ctx, map[string]string{runtime.LabelInstance: rt.InstanceID, runtime.LabelService: "build", runtime.LabelRole: "step"})
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if c.Running && c.IPs[rt.Name("build")] == ip {
				if r, err := parseBuild("", "", c.Labels[runtime.LabelResource]); err == nil {
					ref, ok = r, true
				}
				break
			}
		}
		if !ok {
			return nil, errNoStep
		}
		s.recordStep(ip, ref)
	}
	b, err := s.load(ref)
	if err != nil || b.GetStatus() != cloudbuildpb.Build_WORKING {
		return nil, errNoStep
	}
	return b, nil
}

// recordStep notes that the step at ip is the Build ref's.
func (s *Service) recordStep(ip string, ref buildRef) {
	s.stepsMu.Lock()
	defer s.stepsMu.Unlock()
	s.steps[ip] = ref
}

// forgetStep drops the address of a step whose container is gone, which
// a later step may be given.
func (s *Service) forgetStep(ip string) {
	s.stepsMu.Lock()
	defer s.stepsMu.Unlock()
	delete(s.steps, ip)
}

// forgetSteps drops the addresses of the Build ref's steps.
func (s *Service) forgetSteps(ref buildRef) {
	s.stepsMu.Lock()
	defer s.stepsMu.Unlock()
	for ip, r := range s.steps {
		if r.key() == ref.key() {
			delete(s.steps, ip)
		}
	}
}
