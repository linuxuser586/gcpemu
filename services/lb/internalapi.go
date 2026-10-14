package lb

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/locations"
)

// emu.LoadBalancerResources: GKE's Ingress and Gateway controllers
// (FR-GKE-010) manage load balancers through the same validation, storage
// and reconciliation as the REST handlers, synchronously and without
// compute Operations.

// pathKind resolves "projects/P/global/C/N" or "projects/P/regions/R/C/N".
func (s *Service) pathKind(path string) (*kind, scope, string, error) {
	segs := strings.Split(path, "/")
	var sc scope
	var coll, name string
	switch {
	case len(segs) == 5 && segs[0] == "projects" && segs[2] == "global":
		sc, coll, name = scope{project: segs[1]}, segs[3], segs[4]
	case len(segs) == 6 && segs[0] == "projects" && segs[2] == "regions":
		sc, coll, name = scope{project: segs[1], region: segs[3]}, segs[4], segs[5]
	default:
		return nil, sc, "", apierr.InvalidArgument("Invalid resource path %q.", path)
	}
	for _, k := range allKinds() {
		if k.coll != coll {
			continue
		}
		if sc.region == "" && k.gperm == "" || sc.region != "" && k.rperm == "" {
			break
		}
		if err := s.env.EnsureProject(sc.project); err != nil {
			return nil, sc, "", err
		}
		if sc.region != "" && !locations.IsRegion(sc.region) {
			return nil, sc, "", errNotFound("projects/" + sc.project + "/regions/" + sc.region)
		}
		return k, sc, name, nil
	}
	return nil, sc, "", apierr.InvalidArgument("Unsupported load-balancing resource %q.", path)
}

// PutLBResource implements emu.LoadBalancerResources.
func (s *Service) PutLBResource(ctx context.Context, path string, body []byte) ([]byte, error) {
	k, sc, name, err := s.pathKind(path)
	if err != nil {
		return nil, err
	}
	obj := k.newObj()
	if err := unmarshalLenient(body, obj); err != nil {
		return nil, apierr.InvalidArgument("Invalid JSON payload received. %v", err).WithLegacy("parseError")
	}
	setString(obj, "Name", name)
	setString(obj, "CreationTimestamp", "")
	if cur, exists := s.load(k, path); exists {
		setID(obj, getID(cur))
		setString(obj, "CreationTimestamp", getString(cur, "CreationTimestamp"))
		if k.prepare != nil {
			if err := k.prepare(ctx, s, sc, path, obj, cur); err != nil {
				return nil, err
			}
		}
		err = s.save(ctx, k, sc, path, obj, cur)
	} else {
		if err := validName("resource.name", name); err != nil {
			return nil, err
		}
		if k.prepare != nil {
			if err := k.prepare(ctx, s, sc, path, obj, nil); err != nil {
				return nil, err
			}
		}
		setID(obj, s.env.IDs.Uint64())
		err = s.insert(ctx, k, sc, path, obj)
	}
	if err != nil {
		return nil, err
	}
	return s.LBResource(ctx, path)
}

// LBResource implements emu.LoadBalancerResources.
func (s *Service) LBResource(ctx context.Context, path string) ([]byte, error) {
	k, _, _, err := s.pathKind(path)
	if err != nil {
		return nil, err
	}
	obj, ok := s.load(k, path)
	if !ok {
		return nil, errNotFound(path)
	}
	return json.Marshal(obj)
}

// DeleteLBResource implements emu.LoadBalancerResources.
func (s *Service) DeleteLBResource(ctx context.Context, path string) error {
	k, _, _, err := s.pathKind(path)
	if err != nil {
		return err
	}
	obj, ok := s.load(k, path)
	if !ok {
		return nil
	}
	return s.remove(ctx, k, path, obj)
}
