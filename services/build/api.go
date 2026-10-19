package build

import (
	"context"
	"encoding/base64"
	"path"
	"slices"
	"strconv"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/listfilter"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Defaults of the real API.
const (
	defaultTimeout  = 10 * time.Minute
	defaultQueueTTL = time.Hour
)

// api implements cloudbuildpb.CloudBuildServer: Builds only; Build
// triggers and worker pools answer UNIMPLEMENTED.
type api struct {
	cloudbuildpb.UnimplementedCloudBuildServer
	s *Service
}

func (a *api) CreateBuild(ctx context.Context, req *cloudbuildpb.CreateBuildRequest) (*longrunningpb.Operation, error) {
	project, location, err := parseParent(req.GetProjectId(), req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.builds.create", resRoot+locationName(project, location)); err != nil {
		return nil, err
	}
	if err := a.s.env.EnsureProject(project); err != nil {
		return nil, err
	}
	if req.GetBuild() == nil {
		return nil, apierr.InvalidArgument("build is required.")
	}
	return a.s.submit(project, location, req.GetBuild())
}

func (a *api) GetBuild(ctx context.Context, req *cloudbuildpb.GetBuildRequest) (*cloudbuildpb.Build, error) {
	r, err := parseBuild(req.GetProjectId(), req.GetId(), req.GetName())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.builds.get", r.resource()); err != nil {
		return nil, err
	}
	return a.s.load(r)
}

// buildFilterFields are the builds.list filter fields.
var buildFilterFields = map[string]listfilter.Field[*cloudbuildpb.Build]{
	"build_id":    {Get: (*cloudbuildpb.Build).GetId},
	"id":          {Get: (*cloudbuildpb.Build).GetId},
	"status":      {Get: func(b *cloudbuildpb.Build) string { return b.GetStatus().String() }},
	"trigger_id":  {Get: (*cloudbuildpb.Build).GetBuildTriggerId},
	"tags":        {Values: (*cloudbuildpb.Build).GetTags},
	"images":      {Values: (*cloudbuildpb.Build).GetImages},
	"create_time": {Time: true, Get: func(b *cloudbuildpb.Build) string { return stamp(b.GetCreateTime()) }},
	"start_time":  {Time: true, Get: func(b *cloudbuildpb.Build) string { return stamp(b.GetStartTime()) }},
	"finish_time": {Time: true, Get: func(b *cloudbuildpb.Build) string { return stamp(b.GetFinishTime()) }},
}

func invalid(format string, args ...any) error { return apierr.InvalidArgument(format, args...) }

func stamp(t *timestamppb.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.AsTime().Format(time.RFC3339Nano)
}

// ListBuilds lists a location's Builds, newest first.
func (a *api) ListBuilds(ctx context.Context, req *cloudbuildpb.ListBuildsRequest) (*cloudbuildpb.ListBuildsResponse, error) {
	project, location, err := parseParent(req.GetProjectId(), req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.builds.list", resRoot+locationName(project, location)); err != nil {
		return nil, err
	}
	match, err := listfilter.Compile(req.GetFilter(), buildFilterFields, invalid)
	if err != nil {
		return nil, err
	}
	var builds []*cloudbuildpb.Build
	err = a.s.env.Store.View(func(tx store.Tx) error {
		return scan(tx, locationKey(project, location), func(st *stored) {
			if match(st.build) {
				builds = append(builds, st.build)
			}
		})
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(builds, func(x, y *cloudbuildpb.Build) int {
		return y.GetCreateTime().AsTime().Compare(x.GetCreateTime().AsTime())
	})
	builds, next, err := page(builds, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &cloudbuildpb.ListBuildsResponse{Builds: builds, NextPageToken: next}, nil
}

// page slices items by an opaque offset token.
func page[T any](items []T, token string, size int32) ([]T, string, error) {
	start := 0
	if token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		n, err2 := strconv.Atoi(string(b))
		if err != nil || err2 != nil || n < 0 || n > len(items) {
			return nil, "", apierr.InvalidArgument("Invalid page token.")
		}
		start = n
	}
	if size < 0 {
		return nil, "", apierr.InvalidArgument("page_size must not be negative.")
	}
	if size == 0 || size > 1000 {
		size = 1000
	}
	end := min(start+int(size), len(items))
	next := ""
	if end < len(items) {
		next = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
	}
	return items[start:end], next, nil
}

// CancelBuild cancels a QUEUED or WORKING Build; a finished one is
// returned as it is.
func (a *api) CancelBuild(ctx context.Context, req *cloudbuildpb.CancelBuildRequest) (*cloudbuildpb.Build, error) {
	r, err := parseBuild(req.GetProjectId(), req.GetId(), req.GetName())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.builds.update", r.resource()); err != nil {
		return nil, err
	}
	return a.s.cancel(ctx, r)
}

// RetryBuild submits the Build's original config again as a new Build.
func (a *api) RetryBuild(ctx context.Context, req *cloudbuildpb.RetryBuildRequest) (*longrunningpb.Operation, error) {
	r, err := parseBuild(req.GetProjectId(), req.GetId(), req.GetName())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.builds.create", r.resource()); err != nil {
		return nil, err
	}
	st, err := a.s.loadStored(r)
	if err != nil {
		return nil, err
	}
	return a.s.submit(r.Project, r.Location, st.request)
}

// load returns a stored Build.
func (s *Service) load(r buildRef) (*cloudbuildpb.Build, error) {
	st, err := s.loadStored(r)
	if err != nil {
		return nil, err
	}
	return st.build, nil
}

// loadStored returns a stored Build with its request and Operation.
func (s *Service) loadStored(r buildRef) (*stored, error) {
	var st *stored
	err := s.env.Store.View(func(tx store.Tx) error {
		var err error
		st, err = get(tx, r)
		return err
	})
	return st, err
}

// submit validates a Build, stores it QUEUED with the Operation that ends
// with it, and has the scheduler start it.
func (s *Service) submit(project, location string, in *cloudbuildpb.Build) (*longrunningpb.Operation, error) {
	if err := validate(in); err != nil {
		return nil, err
	}
	b := proto.Clone(in).(*cloudbuildpb.Build)
	h := s.env.IDs.Hex(16)
	r := buildRef{project, location, h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]}
	b.Id, b.Name, b.ProjectId = r.ID, r.name(), project
	b.Status, b.StatusDetail = cloudbuildpb.Build_QUEUED, ""
	b.CreateTime, b.StartTime, b.FinishTime = timestamppb.New(s.env.Clock.Now()), nil, nil
	b.Results, b.Timing, b.FailureInfo = nil, nil, nil
	if b.Timeout == nil {
		b.Timeout = durationpb.New(defaultTimeout)
	}
	if b.QueueTtl == nil {
		b.QueueTtl = durationpb.New(defaultQueueTTL)
	}
	for _, st := range b.Steps {
		st.Status, st.ExitCode, st.Timing, st.PullTiming = cloudbuildpb.Build_QUEUED, 0, nil, nil
	}
	if gw := s.env.Endpoints.Get("gateway"); gw != "" {
		b.LogUrl = "http://" + gw + "/_emu/v1/build/" + r.name() + "/log"
	}
	op, err := s.ops.Begin(r.parent(), &cloudbuildpb.BuildOperationMetadata{Build: b})
	if err != nil {
		return nil, err
	}
	err = s.env.Store.Update(func(tx store.Tx) error {
		return put(tx, &stored{build: b, request: in, op: op.Name})
	})
	if err != nil {
		s.ops.Finish(op.Name, nil, nil, err)
		return nil, err
	}
	s.mu.Lock()
	s.queue = append(s.queue, r)
	s.mu.Unlock()
	s.kick()
	return op, nil
}

// validate refuses a Build that the real API would, and the features this
// emulator does not have yet rather than run without them.
func validate(b *cloudbuildpb.Build) error {
	if len(b.GetSteps()) == 0 {
		return apierr.InvalidArgument("invalid build: no build steps.")
	}
	if len(b.GetSecrets()) > 0 || b.GetAvailableSecrets() != nil {
		return apierr.New(codes.Unimplemented, "Build secrets are not emulated yet.")
	}
	for _, d := range []*durationpb.Duration{b.GetTimeout(), b.GetQueueTtl()} {
		if d != nil && d.AsDuration() < 0 {
			return apierr.InvalidArgument("invalid build: a duration must not be negative.")
		}
	}
	ids := map[string]int{}
	for i, st := range b.GetSteps() {
		if st.GetName() == "" {
			return apierr.InvalidArgument("invalid build: build step %d must specify name.", i)
		}
		if st.GetScript() != "" && len(st.GetArgs()) > 0 {
			return apierr.InvalidArgument("invalid build: build step %d: script and args are mutually exclusive.", i)
		}
		if len(st.GetSecretEnv()) > 0 {
			return apierr.New(codes.Unimplemented, "Build secrets are not emulated yet.")
		}
		if st.GetTimeout() != nil && st.GetTimeout().AsDuration() < 0 {
			return apierr.InvalidArgument("invalid build: build step %d: timeout must not be negative.", i)
		}
		for _, w := range st.GetWaitFor() {
			if w == "-" {
				continue
			}
			if _, ok := ids[w]; !ok {
				return apierr.InvalidArgument("invalid build: build step %d depends on %q, which is not the id of an earlier build step.", i, w)
			}
		}
		if id := st.GetId(); id != "" {
			if _, dup := ids[id]; dup {
				return apierr.InvalidArgument("invalid build: build step id %q is used more than once.", id)
			}
			ids[id] = i
		}
		if err := validateVolumes(st.GetVolumes()); err != nil {
			return err
		}
	}
	return validateVolumes(b.GetOptions().GetVolumes())
}

func validateVolumes(vs []*cloudbuildpb.Volume) error {
	for _, v := range vs {
		p := v.GetPath()
		if v.GetName() == "" || !path.IsAbs(p) {
			return apierr.InvalidArgument("invalid build: a volume needs a name and an absolute path.")
		}
		if p = path.Clean(p); p == workspace || p == home {
			return apierr.InvalidArgument("invalid build: volume path %q is reserved.", p)
		}
	}
	return nil
}
