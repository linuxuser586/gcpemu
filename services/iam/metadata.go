package iam

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// GCE-compatible metadata server (FR-IAM-008).

// DefaultProjectID is the project the metadata server reports when neither
// GCPEMU_DEFAULT_PROJECT nor the seed sets one.
const DefaultProjectID = "gcpemu-project"

const defaultZone = "us-central1-a"

// metadataSettings are the host metadata server's defaults.
type metadataSettings struct {
	Project string
	Zone    string
	Email   string
	// Explicit is set when the service account was configured (config or
	// seed). Otherwise the implicit default SA acts with the default
	// principal's rights, so ADC through the host metadata server causes
	// no audit noise on a first run.
	Explicit bool
}

// MetadataIdentity selects what a metadata handler reports; zero fields
// fall back to the instance defaults. GKE uses it to serve per-workload
// identities (Workload Identity, FR-INT-007).
type MetadataIdentity struct {
	Project string
	Zone    string
	// Email is the service account behind "default".
	Email string
	// Attributes are extra instance attributes (e.g. cluster-name).
	Attributes map[string]string
}

type cachedToken struct {
	token  string
	expiry time.Time
}

func (s *Service) defaultProject() string {
	if p := s.env.Config.DefaultProject; p != "" {
		return p
	}
	return DefaultProjectID
}

// defaultComputeEmail is a project's default compute service account.
func defaultComputeEmail(projectID string) string {
	return project.NumberString(projectID) + "-compute@developer.gserviceaccount.com"
}

func (s *Service) metadataDefaults() metadataSettings {
	m := metadataSettings{Project: s.defaultProject(), Zone: defaultZone, Email: s.env.Config.MetadataServiceAccount}
	m.Explicit = m.Email != ""
	if m.Email == "" {
		m.Email = defaultComputeEmail(m.Project)
	}
	return m
}

// implicitHostAccount reports whether email is the host metadata server's
// implicit (unconfigured) default service account.
func (s *Service) implicitHostAccount(email string) bool {
	s.mu.Lock()
	m := s.meta
	s.mu.Unlock()
	if m.Project == "" {
		m = s.metadataDefaults()
	}
	return !m.Explicit && strings.EqualFold(email, m.Email)
}

// resolveIdentity fills id from the instance defaults.
func (s *Service) resolveIdentity(id MetadataIdentity) MetadataIdentity {
	s.mu.Lock()
	def := s.meta
	s.mu.Unlock()
	if def.Project == "" {
		def = s.metadataDefaults()
	}
	if id.Project == "" {
		id.Project = def.Project
	}
	if id.Zone == "" {
		id.Zone = def.Zone
	}
	if id.Email == "" {
		if id.Project == def.Project {
			id.Email = def.Email
		} else {
			id.Email = defaultComputeEmail(id.Project)
		}
	}
	return id
}

// MetadataHandler returns a metadata server handler for an identity.
func (s *Service) MetadataHandler(id MetadataIdentity) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Metadata-Flavor", "Google")
		h.Set("Server", "Metadata Server for VM")
		path := r.URL.Path
		if path == "/" || path == "" {
			h.Set("Content-Type", "application/text")
			_, _ = w.Write([]byte("computeMetadata/\n"))
			return
		}
		if r.Header.Get("X-Forwarded-For") != "" {
			http.Error(w, "Request contains X-Forwarded-For header", http.StatusForbidden)
			return
		}
		rel, ok := strings.CutPrefix(path, "/computeMetadata/v1")
		if !ok {
			if path == "/computeMetadata/" || path == "/computeMetadata" {
				_, _ = w.Write([]byte("v1/\n"))
				return
			}
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Metadata-Flavor") != "Google" && r.Header.Get("X-Google-Metadata-Request") != "True" {
			h.Set("Content-Type", "text/html; charset=UTF-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("Missing required header: Metadata-Flavor\n"))
			return
		}
		s.serveMetadata(w, r, s.resolveIdentity(id), strings.Trim(rel, "/"))
	})
}

func (s *Service) serveMetadata(w http.ResponseWriter, r *http.Request, id MetadataIdentity, rel string) {
	q := r.URL.Query()
	segs := []string{}
	if rel != "" {
		segs = strings.Split(rel, "/")
	}
	// Dynamic leaves: instance/service-accounts/{acct}/{token,identity}.
	if len(segs) == 4 && segs[0] == "instance" && segs[1] == "service-accounts" {
		email, ok := s.metadataAccount(id, segs[2])
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch segs[3] {
		case "token":
			s.metadataToken(w, email, q.Get("scopes"))
			return
		case "identity":
			aud := q.Get("audience")
			if aud == "" {
				http.Error(w, "non-empty audience parameter required", http.StatusBadRequest)
				return
			}
			tok, err := s.mintIDToken(email, aud, q.Get("format") == "full", nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			_, _ = w.Write([]byte(tok))
			return
		}
	}
	var node any = s.metadataTree(id)
	for _, seg := range segs {
		m, ok := node.(map[string]any)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if node, ok = m[seg]; !ok {
			http.NotFound(w, r)
			return
		}
	}
	if q.Get("recursive") == "true" || q.Get("alt") == "json" {
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(jsonTree(node, ""))
		_, _ = w.Write(b)
		return
	}
	w.Header().Set("Content-Type", "application/text")
	switch n := node.(type) {
	case string:
		_, _ = w.Write([]byte(n))
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k, v := range n {
			if _, sub := v.(map[string]any); sub {
				k += "/"
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		for _, k := range keys {
			sb.WriteString(k + "\n")
		}
		_, _ = w.Write([]byte(sb.String()))
	}
}

// metadataAccount resolves "default" or an email to the account email.
func (s *Service) metadataAccount(id MetadataIdentity, acct string) (string, bool) {
	if acct == "default" || strings.EqualFold(acct, id.Email) {
		return id.Email, true
	}
	return "", false
}

// metadataToken serves an access token for email, cached until five
// minutes before expiry.
func (s *Service) metadataToken(w http.ResponseWriter, email, scopes string) {
	key := email + "\x00" + scopes
	now := s.env.Clock.Now()
	s.mu.Lock()
	ct, ok := s.tokCache[key]
	s.mu.Unlock()
	if !ok || now.After(ct.expiry.Add(-5*time.Minute)) {
		sc := []string{cloudPlatformScope}
		if scopes != "" {
			sc = strings.Split(scopes, ",")
		}
		tok, exp, err := s.mintAccessToken(emu.Principal("serviceAccount:"+email), sc, "", tokenLifetime)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ct = cachedToken{token: tok, expiry: exp}
		s.mu.Lock()
		s.tokCache[key] = ct
		s.mu.Unlock()
	}
	writeJSON(w, map[string]any{"access_token": ct.token, "expires_in": s.expiresIn(ct.expiry), "token_type": "Bearer"})
}

// metadataTree builds the static part of the metadata tree.
func (s *Service) metadataTree(id MetadataIdentity) map[string]any {
	num := project.NumberString(id.Project)
	sa := map[string]any{
		"aliases": "default",
		"email":   id.Email,
		"scopes":  cloudPlatformScope + "\n",
	}
	attrs := map[string]any{}
	for k, v := range id.Attributes {
		attrs[k] = v
	}
	region := id.Zone
	if i := strings.LastIndexByte(region, '-'); i > 0 {
		region = region[:i]
	}
	return map[string]any{
		"project": map[string]any{
			"project-id":         id.Project,
			"numeric-project-id": num,
			"attributes":         map[string]any{},
		},
		"instance": map[string]any{
			"id":           strconv.FormatInt(project.Number("instance:"+id.Project+"/"+id.Email)*10_000_000, 10),
			"name":         "gcpemu",
			"hostname":     "gcpemu.c." + id.Project + ".internal",
			"zone":         "projects/" + num + "/zones/" + id.Zone,
			"region":       "projects/" + num + "/regions/" + region,
			"machine-type": "projects/" + num + "/machineTypes/e2-standard-4",
			"attributes":   attrs,
			"tags":         "[]",
			"service-accounts": map[string]any{
				"default": sa,
				id.Email:  sa,
			},
			"network-interfaces": map[string]any{
				"0": map[string]any{
					"ip":      "127.0.0.1",
					"network": "projects/" + num + "/networks/default",
				},
			},
		},
		"universe": map[string]any{
			"universe-domain": "googleapis.com",
		},
	}
}

// jsonTree converts a metadata subtree to its recursive JSON form, with
// camelCase keys as the real server renders them.
func jsonTree(node any, key string) any {
	switch n := node.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range n {
			out[camel(k)] = jsonTree(v, k)
		}
		return out
	case string:
		switch key {
		case "numeric-project-id", "id":
			if v, err := strconv.ParseInt(n, 10, 64); err == nil {
				return v
			}
		case "scopes":
			return strings.Fields(n)
		case "aliases":
			return []string{n}
		}
		return n
	}
	return node
}

func camel(k string) string {
	if strings.Contains(k, "@") {
		return k
	}
	parts := strings.Split(k, "-")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}
