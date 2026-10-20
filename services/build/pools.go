package build

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/listpage"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Worker pools (FR-CB-015) are Recorded: they are created, read, patched
// and deleted, each through an Operation, and change nothing about how a
// Build runs. A Build naming one in options.pool.name runs on the Build
// daemon like any other.

// nsPools holds every worker pool, keyed by locationKey and pool ID.
const nsPools = "build/workerpools"

// poolID is the form of a worker pool's ID.
var poolID = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// parsePool resolves a worker pool's resource name to its store key.
func parsePool(name string) (project, location, id string, err error) {
	segs := strings.Split(name, "/")
	if len(segs) != 6 || segs[0] != "projects" || segs[2] != "locations" || segs[4] != "workerPools" ||
		segs[1] == "" || segs[3] == "" || segs[5] == "" {
		return "", "", "", apierr.InvalidArgument("Invalid worker pool name %q: want projects/{project}/locations/{location}/workerPools/{workerPool}.", name)
	}
	return segs[1], segs[3], segs[5], nil
}

func poolKey(name string) string {
	p, l, id, _ := parsePool(name)
	return locationKey(p, l) + id
}

func putPool(tx store.Tx, wp *cloudbuildpb.WorkerPool) error {
	b, err := protojson.Marshal(wp)
	if err != nil {
		return err
	}
	return tx.Put(nsPools, poolKey(wp.GetName()), b)
}

func getPool(tx store.Tx, name string) (*cloudbuildpb.WorkerPool, error) {
	b, ok := tx.Get(nsPools, poolKey(name))
	if !ok {
		return nil, apierr.NotFound("Worker pool %q not found.", name)
	}
	wp := &cloudbuildpb.WorkerPool{}
	return wp, protojson.Unmarshal(b, wp)
}

// scanPools calls fn with each worker pool under the key prefix.
func scanPools(tx store.Tx, prefix string, fn func(*cloudbuildpb.WorkerPool)) error {
	var err error
	tx.Scan(nsPools, prefix, func(_ string, b []byte) bool {
		wp := &cloudbuildpb.WorkerPool{}
		if err = protojson.Unmarshal(b, wp); err != nil {
			return false
		}
		fn(wp)
		return true
	})
	return err
}

// poolEtag is a worker pool's etag, from its last update.
func poolEtag(wp *cloudbuildpb.WorkerPool) string {
	return `W/"` + strconv.FormatInt(wp.GetUpdateTime().AsTime().UnixNano(), 36) + `"`
}

// validatePool refuses a worker pool the real API would.
func validatePool(wp *cloudbuildpb.WorkerPool) error {
	if len(wp.GetDisplayName()) > 63 {
		return apierr.InvalidArgument("invalid worker pool: displayName must be at most 63 characters.")
	}
	if d := wp.GetPrivatePoolV1Config().GetWorkerConfig().GetDiskSizeGb(); d < 0 || d > 4000 {
		return apierr.InvalidArgument("invalid worker pool: workerConfig.diskSizeGb must be between 0 and 4000.")
	}
	return nil
}

// savePool stores a worker pool, created or updated now.
func (s *Service) savePool(wp *cloudbuildpb.WorkerPool, create bool) (*cloudbuildpb.WorkerPool, error) {
	now := timestamppb.New(s.env.Clock.Now())
	wp.UpdateTime, wp.State, wp.DeleteTime = now, cloudbuildpb.WorkerPool_RUNNING, nil
	wp.Etag = poolEtag(wp)
	err := s.env.Store.Update(func(tx store.Tx) error {
		if _, err := getPool(tx, wp.GetName()); create && err == nil {
			return apierr.AlreadyExists("Worker pool %q already exists.", wp.GetName())
		} else if !create && err != nil {
			return err
		}
		return putPool(tx, wp)
	})
	return wp, err
}

func (a *api) CreateWorkerPool(ctx context.Context, req *cloudbuildpb.CreateWorkerPoolRequest) (*longrunningpb.Operation, error) {
	project, location, err := parseParent("", req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.workerpools.create", resRoot+locationName(project, location)); err != nil {
		return nil, err
	}
	if err := a.s.env.EnsureProject(project); err != nil {
		return nil, err
	}
	if location == globalLocation {
		return nil, apierr.InvalidArgument("Worker pools are regional: the location must not be global.")
	}
	if !poolID.MatchString(req.GetWorkerPoolId()) {
		return nil, apierr.InvalidArgument("worker_pool_id %q must be 1-63 lowercase letters, digits and dashes, starting with a letter.", req.GetWorkerPoolId())
	}
	wp := proto.Clone(req.GetWorkerPool()).(*cloudbuildpb.WorkerPool)
	if wp == nil {
		wp = &cloudbuildpb.WorkerPool{}
	}
	wp.Name = locationName(project, location) + "/workerPools/" + req.GetWorkerPoolId()
	wp.Uid, wp.CreateTime = a.s.env.IDs.UUID(), timestamppb.New(a.s.env.Clock.Now())
	if err := validatePool(wp); err != nil {
		return nil, err
	}
	md := &cloudbuildpb.CreateWorkerPoolOperationMetadata{WorkerPool: wp.Name, CreateTime: wp.CreateTime}
	if req.GetValidateOnly() {
		return a.s.ops.Run(ctx, locationName(project, location), md, func(context.Context) (proto.Message, error) { return wp, nil })
	}
	if _, err := a.s.savePool(wp, true); err != nil {
		return nil, err
	}
	return a.s.ops.Run(ctx, locationName(project, location), md, func(context.Context) (proto.Message, error) {
		md.CompleteTime = timestamppb.New(a.s.env.Clock.Now())
		return a.s.loadPool(wp.Name)
	})
}

func (s *Service) loadPool(name string) (*cloudbuildpb.WorkerPool, error) {
	var wp *cloudbuildpb.WorkerPool
	err := s.env.Store.View(func(tx store.Tx) error {
		var err error
		wp, err = getPool(tx, name)
		return err
	})
	return wp, err
}

func (a *api) GetWorkerPool(ctx context.Context, req *cloudbuildpb.GetWorkerPoolRequest) (*cloudbuildpb.WorkerPool, error) {
	if _, _, _, err := parsePool(req.GetName()); err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.workerpools.get", resRoot+req.GetName()); err != nil {
		return nil, err
	}
	return a.s.loadPool(req.GetName())
}

func (a *api) ListWorkerPools(ctx context.Context, req *cloudbuildpb.ListWorkerPoolsRequest) (*cloudbuildpb.ListWorkerPoolsResponse, error) {
	project, location, err := parseParent("", req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.workerpools.list", resRoot+locationName(project, location)); err != nil {
		return nil, err
	}
	var wps []*cloudbuildpb.WorkerPool
	err = a.s.env.Store.View(func(tx store.Tx) error {
		return scanPools(tx, locationKey(project, location), func(wp *cloudbuildpb.WorkerPool) { wps = append(wps, wp) })
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(wps, func(x, y *cloudbuildpb.WorkerPool) int { return cmp.Compare(x.GetName(), y.GetName()) })
	wps, next, err := listpage.Offset(wps, req.GetPageToken(), req.GetPageSize(), pageOpts)
	if err != nil {
		return nil, err
	}
	return &cloudbuildpb.ListWorkerPoolsResponse{WorkerPools: wps, NextPageToken: next}, nil
}

// UpdateWorkerPool changes the fields the update_mask names, or, without
// one, displayName, annotations and the pool's config.
func (a *api) UpdateWorkerPool(ctx context.Context, req *cloudbuildpb.UpdateWorkerPoolRequest) (*longrunningpb.Operation, error) {
	in := req.GetWorkerPool()
	project, location, _, err := parsePool(in.GetName())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.workerpools.update", resRoot+in.GetName()); err != nil {
		return nil, err
	}
	cur, err := a.s.loadPool(in.GetName())
	if err != nil {
		return nil, err
	}
	if in.GetEtag() != "" && in.GetEtag() != cur.GetEtag() {
		return nil, apierr.Aborted("The worker pool's etag %q does not match %q.", in.GetEtag(), cur.GetEtag())
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"display_name", "annotations", "private_pool_v1_config"}
	}
	wp := proto.Clone(cur).(*cloudbuildpb.WorkerPool)
	if err := applyMask(wp, in, paths); err != nil {
		return nil, err
	}
	wp.Name, wp.Uid, wp.CreateTime = cur.Name, cur.Uid, cur.CreateTime
	if err := validatePool(wp); err != nil {
		return nil, err
	}
	md := &cloudbuildpb.UpdateWorkerPoolOperationMetadata{WorkerPool: wp.Name, CreateTime: timestamppb.New(a.s.env.Clock.Now())}
	if !req.GetValidateOnly() {
		if wp, err = a.s.savePool(wp, false); err != nil {
			return nil, err
		}
	}
	return a.s.ops.Run(ctx, locationName(project, location), md, func(context.Context) (proto.Message, error) {
		md.CompleteTime = timestamppb.New(a.s.env.Clock.Now())
		return wp, nil
	})
}

func (a *api) DeleteWorkerPool(ctx context.Context, req *cloudbuildpb.DeleteWorkerPoolRequest) (*longrunningpb.Operation, error) {
	project, location, _, err := parsePool(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.workerpools.delete", resRoot+req.GetName()); err != nil {
		return nil, err
	}
	md := &cloudbuildpb.DeleteWorkerPoolOperationMetadata{WorkerPool: req.GetName(), CreateTime: timestamppb.New(a.s.env.Clock.Now())}
	err = a.s.env.Store.Update(func(tx store.Tx) error {
		cur, err := getPool(tx, req.GetName())
		if err != nil {
			if req.GetAllowMissing() {
				return nil
			}
			return err
		}
		if req.GetEtag() != "" && req.GetEtag() != cur.GetEtag() {
			return apierr.Aborted("The worker pool's etag %q does not match %q.", req.GetEtag(), cur.GetEtag())
		}
		if req.GetValidateOnly() {
			return nil
		}
		return tx.Delete(nsPools, poolKey(req.GetName()))
	})
	if err != nil {
		return nil, err
	}
	return a.s.ops.Run(ctx, locationName(project, location), md, func(context.Context) (proto.Message, error) {
		md.CompleteTime = timestamppb.New(a.s.env.Clock.Now())
		return nil, nil
	})
}
