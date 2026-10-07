package instance

import (
	"net/http"
	"path/filepath"

	"github.com/linuxuser586/gcpemu/internal/hostmode"
	"github.com/linuxuser586/gcpemu/internal/trust"
)

// Real hostnames: the Google frontend (FR-INT-007), host mode
// (FR-CORE-043) and CA trust hints for `gcpemu env` (FR-CORE-004).

// startHostNames writes the combined CA bundle and starts host mode when
// configured. Neither can fail instance start.
func (in *Instance) startHostNames() {
	if _, err := in.WriteCABundle(); err != nil {
		in.Env.Log.Warn("writing the CA bundle", "err", err)
	}
	if in.Config.HostMode {
		in.hm = hostmode.New(in.Env, in.gw.Hosts)
		in.hm.Start()
	}
}

// WriteCABundle (re)writes <instance-dir>/ca-bundle.pem: the host's system
// roots plus the instance CA, for SSL_CERT_FILE.
func (in *Instance) WriteCABundle() (string, error) {
	if in.Env.CA == nil {
		return "", nil
	}
	return trust.WriteBundle(filepath.Join(in.Dir, trust.BundleFile), in.Env.CA.PEM())
}

// HostMode returns host mode's status (State "disabled" when off).
func (in *Instance) HostMode() hostmode.Status { return in.hm.Status() }

// FrontendHosts returns the real hostnames the Google frontend serves.
func (in *Instance) FrontendHosts() []string { return in.fe.Hosts() }

// hostNameEnvVars adds the CA and host-mode variables to `gcpemu env`.
func (in *Instance) hostNameEnvVars(out map[string]string) {
	if in.Env.CA != nil {
		out["GCPEMU_CA_FILE"] = in.Env.CA.Path()
	}
	if st := in.HostMode(); st.State == hostmode.StateReady {
		out["GCPEMU_HOST_MODE_IP"] = st.IP
	}
}

// serveEnv is GET /_emu/v1/env[?trust=true]: with trust, SSL_CERT_FILE
// points at a freshly written combined bundle.
func (in *Instance) serveEnv(w http.ResponseWriter, r *http.Request) {
	vars := in.EnvVars()
	if t := r.URL.Query().Get("trust"); t == "1" || t == "true" {
		p, err := in.WriteCABundle()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if p != "" {
			vars["SSL_CERT_FILE"] = p
		}
	}
	writeJSON(w, http.StatusOK, vars)
}

// serveHostMode is GET /_emu/v1/hostmode.
func (in *Instance) serveHostMode(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, in.HostMode())
}
