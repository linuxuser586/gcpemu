package certs

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/config"
	"github.com/linuxuser586/gcpemu/internal/emu"
)

// IAM policies on server and client TLS policies (FR-IAM-003), the Network
// Security resources with IAM methods that the emulator models;
// Certificate Manager has none. The iam service, which certs depends on,
// keeps them under the resource's full name.

// iamKinds are the kinds that serve getIamPolicy, setIamPolicy and
// testIamPermissions.
var iamKinds = map[*kind]bool{kServerTLS: true, kClientTLS: true}

func (s *Service) policyStore() (emu.IAMPolicyStore, error) {
	if svc, ok := s.env.Lookup("iam"); ok {
		if ps, ok := svc.(emu.IAMPolicyStore); ok {
			return ps, nil
		}
	}
	return nil, apierr.FailedPrecondition("IAM policies need the iam service.")
}

// serveIAM serves {name}:getIamPolicy, :setIamPolicy and
// :testIamPermissions of an existing resource.
func (s *Service) serveIAM(w http.ResponseWriter, r *http.Request, k *kind, name, verb string) {
	if err := s.routeIAM(w, r, k, name, verb); err != nil {
		apierr.Write(w, err)
	}
}

func (s *Service) routeIAM(w http.ResponseWriter, r *http.Request, k *kind, name, verb string) error {
	ctx := r.Context()
	perm := k.perm + "." + verb
	switch {
	case verb == "getIamPolicy" && r.Method == http.MethodGet:
	case verb == "setIamPolicy" && r.Method == http.MethodPost:
	case verb == "testIamPermissions" && r.Method == http.MethodPost:
		perm = k.perm + ".get"
	default:
		methodNotAllowed(w, r)
		return nil
	}
	n, err := parseName(k, name)
	if err != nil {
		return err
	}
	res := n.fullName(k)
	if err := s.access(ctx, n, k, perm, res, false); err != nil {
		return err
	}
	if _, _, err := s.loadKind(k, n.name(k)); err != nil {
		return err
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	var req struct {
		Policy      json.RawMessage `json:"policy"`
		UpdateMask  string          `json:"updateMask"`
		Permissions []string        `json:"permissions"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return apierr.InvalidArgument("Invalid JSON payload received. %v", err)
		}
	}
	if verb == "testIamPermissions" {
		for _, p := range req.Permissions {
			if !strings.HasPrefix(p, "networksecurity.") {
				return apierr.InvalidArgument("Permission %s is not valid for this resource.", p)
			}
		}
		out := req.Permissions
		if s.env.Auth.Mode() != config.IAMOff {
			out = []string{}
			if svc, ok := s.env.Lookup("iam"); ok {
				if pt, ok := svc.(emu.IAMPermissionTester); ok {
					out = pt.TestPermissions(ctx, res, req.Permissions)
				}
			}
		}
		writeJSON(w, map[string]any{"permissions": out})
		return nil
	}
	ps, err := s.policyStore()
	if err != nil {
		return err
	}
	var out []byte
	if verb == "getIamPolicy" {
		out, err = ps.GetPolicyJSON(ctx, res)
	} else {
		if len(req.Policy) == 0 {
			req.Policy = json.RawMessage(`{}`)
		}
		out, err = ps.SetPolicyJSON(ctx, res, req.Policy)
	}
	if err != nil {
		return err
	}
	writeJSON(w, json.RawMessage(out))
	return nil
}

// deletePolicy drops a deleted resource's policy.
func (s *Service) deletePolicy(ctx context.Context, k *kind, n resName) {
	if !iamKinds[k] {
		return
	}
	if ps, err := s.policyStore(); err == nil {
		_ = ps.DeletePolicy(ctx, n.fullName(k))
	}
}
