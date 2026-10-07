package pubsub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// iamServer implements google.iam.v1.IAMPolicy for topics, subscriptions,
// snapshots and schemas (FR-IAM-003). Policies live in the IAM service's
// IAMPolicyStore, or in this package when IAM is not running.
type iamServer struct {
	iampb.UnimplementedIAMPolicyServer
	s *Service
}

// iamResource validates a resource name and returns its permission prefix
// (e.g. "pubsub.topics").
func (s *Service) iamResource(resource string) (string, error) {
	parts := strings.Split(resource, "/")
	if len(parts) != 4 || parts[0] != "projects" {
		return "", apierr.InvalidArgument("Invalid resource name: %s", resource)
	}
	kind := parts[2]
	if _, _, err := parseName(resource, kind); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ok bool
	switch kind {
	case kindTopics:
		_, ok = s.topics[resource]
	case kindSubscriptions:
		_, ok = s.subs[resource]
	case kindSnapshots:
		_, ok = s.snaps[resource]
	case kindSchemas:
		_, ok = s.schemas[resource]
	default:
		return "", apierr.InvalidArgument("Invalid resource name: %s", resource)
	}
	if !ok {
		return "", notFound(resource)
	}
	return "pubsub." + kind, nil
}

func (p *iamServer) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return p.s.getIamPolicy(ctx, req.GetResource())
}

func (p *iamServer) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	return p.s.setIamPolicy(ctx, req)
}

func (p *iamServer) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	return p.s.testIamPermissions(ctx, req)
}

func (s *Service) policyStore() emu.IAMPolicyStore {
	if svc, ok := s.env.Lookup("iam"); ok {
		if ps, ok := svc.(emu.IAMPolicyStore); ok {
			return ps
		}
	}
	return nil
}

func (s *Service) getIamPolicy(ctx context.Context, resource string) (*iampb.Policy, error) {
	prefix, err := s.iamResource(resource)
	if err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, prefix+".getIamPolicy", fullName(resource)); err != nil {
		return nil, err
	}
	if ps := s.policyStore(); ps != nil {
		b, err := ps.GetPolicyJSON(ctx, fullName(resource))
		if err != nil {
			if e := apierr.From(err); e.Code == codes.NotFound {
				return emptyPolicy(), nil
			}
			return nil, err
		}
		return decodePolicy(b)
	}
	var pol *iampb.Policy
	err = s.env.Store.View(func(tx store.Tx) error {
		b, ok := tx.Get(nsIAM, resource)
		if !ok {
			pol = emptyPolicy()
			return nil
		}
		var derr error
		pol, derr = decodePolicy(b)
		return derr
	})
	return pol, err
}

func (s *Service) setIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	resource := req.GetResource()
	prefix, err := s.iamResource(resource)
	if err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, prefix+".setIamPolicy", fullName(resource)); err != nil {
		return nil, err
	}
	if req.GetPolicy() == nil {
		return nil, apierr.InvalidArgument("The policy field in the SetIamPolicyRequest must be set.")
	}
	pol := proto.Clone(req.GetPolicy()).(*iampb.Policy)
	if ps := s.policyStore(); ps != nil {
		b, _ := protojson.Marshal(pol)
		out, err := ps.SetPolicyJSON(ctx, fullName(resource), b)
		if err != nil {
			return nil, err
		}
		return decodePolicy(out)
	}
	err = s.env.Store.Update(func(tx store.Tx) error {
		cur := emptyPolicy()
		if b, ok := tx.Get(nsIAM, resource); ok {
			var derr error
			if cur, derr = decodePolicy(b); derr != nil {
				return derr
			}
		}
		if len(pol.Etag) > 0 && !bytes.Equal(pol.Etag, cur.Etag) {
			return apierr.Aborted("There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.")
		}
		mask := req.GetUpdateMask().GetPaths()
		if len(mask) > 0 {
			next := proto.Clone(cur).(*iampb.Policy)
			for _, p := range mask {
				switch p {
				case "bindings":
					next.Bindings = pol.Bindings
				case "audit_configs":
					next.AuditConfigs = pol.AuditConfigs
				case "etag":
				default:
					return apierr.InvalidArgument("Invalid update_mask path: %s", p)
				}
			}
			pol = next
		}
		if pol.Version == 0 {
			pol.Version = 1
		}
		pol.Etag = nil
		b, _ := proto.Marshal(pol)
		sum := sha256.Sum256(b)
		pol.Etag = append([]byte{0x42}, sum[:7]...)
		js, _ := protojson.Marshal(pol)
		return tx.Put(nsIAM, resource, js)
	})
	if err != nil {
		return nil, err
	}
	return pol, nil
}

func (s *Service) testIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	if _, err := s.iamResource(req.GetResource()); err != nil {
		return nil, err
	}
	out := &iampb.TestIamPermissionsResponse{}
	for _, p := range req.GetPermissions() {
		if !strings.HasPrefix(p, "pubsub.") {
			return nil, apierr.InvalidArgument("Invalid permission: %s", p)
		}
		if err := s.env.Auth.Check(ctx, p, fullName(req.GetResource())); err == nil {
			out.Permissions = append(out.Permissions, p)
		}
	}
	return out, nil
}

func emptyPolicy() *iampb.Policy { return &iampb.Policy{Version: 1, Etag: []byte("\x00\x20\x01")} }

func decodePolicy(b []byte) (*iampb.Policy, error) {
	pol := &iampb.Policy{}
	if err := unmarshalOpts.Unmarshal(b, pol); err != nil {
		return nil, errors.Join(apierr.Internal("corrupt IAM policy"), err)
	}
	return pol, nil
}
