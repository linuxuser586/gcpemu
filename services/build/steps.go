package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/runtime"
)

// Paths every step shares, as on Cloud Build's workers.
const (
	workspace = "/workspace"
	home      = "/builder/home"
	// scriptFile is where a step's script is written, in its container.
	scriptFile = "/.gcpemu-build-script"
)

// errStepTimeout stops a step that ran past its own timeout.
var errStepTimeout = errors.New("build: step timed out")

// errStepStopped is a step stopped while it ran, with the Build or by
// another step's failure.
var errStepStopped = errors.New("build: step stopped")

// stepFailure is a step ending a Build that has not been told to stop.
type stepFailure struct{ o outcome }

func (f *stepFailure) Error() string { return f.o.detail }

// runSteps runs a Build's steps, each once the steps it waits for have
// ended: by default every earlier step, with waitFor the steps named
// ("-" for none). A step failing stops the steps still running, and no
// step starts after it.
//
// ranToEnd reports whether the Build ran to its own end: setup completed,
// and every step ended on its own or a step's failure ended the Build.
// Otherwise the Build was stopped, and its stop's cause decides its status.
// Once every step has ended, the Build's images and artifacts are
// published.
func (s *Service) runSteps(r *liveBuild, b *cloudbuildpb.Build) (o outcome, ranToEnd bool) {
	s.reach(r.ctx, b, "setup")
	rt, err := s.env.Containers.Runtime(r.ctx)
	if err != nil {
		return outcome{status: cloudbuildpb.Build_INTERNAL_ERROR, detail: "No container runtime: " + err.Error()}, false
	}
	vols, err := s.createVolumes(r.ctx, rt, b)
	if err != nil {
		return outcome{status: cloudbuildpb.Build_INTERNAL_ERROR, detail: "Creating the Build's volumes: " + err.Error()}, false
	}
	network, err := s.network(r.ctx, rt)
	if err != nil {
		return outcome{status: cloudbuildpb.Build_INTERNAL_ERROR, detail: "Creating the Build network: " + err.Error()}, false
	}
	daemon, err := s.buildDaemon(r.ctx, rt, network)
	if err != nil {
		return outcome{status: cloudbuildpb.Build_INTERNAL_ERROR, detail: "Starting the Build daemon: " + err.Error()}, false
	}
	if err := s.writeCredentials(r.ctx, rt, b, vols); err != nil {
		return outcome{status: cloudbuildpb.Build_INTERNAL_ERROR, detail: "Writing the Build's credentials: " + err.Error()}, false
	}
	if r.secrets, err = s.resolveSecrets(r.ctx, r, b); err != nil {
		var f *stepFailure
		if errors.As(err, &f) {
			return f.o, true
		}
		return outcome{status: cloudbuildpb.Build_CANCELLED, detail: "Build was cancelled."}, false
	}
	if err := s.fetchSource(r.ctx, rt, r, b, vols, network.name); err != nil {
		var f *stepFailure
		if errors.As(err, &f) {
			return f.o, true
		}
		return outcome{status: cloudbuildpb.Build_CANCELLED, detail: "Build was cancelled."}, false
	}
	s.logs.line(r.ref, "BUILD")
	ctx, fail := context.WithCancelCause(r.ctx)
	defer fail(nil)
	steps := b.GetSteps()
	done := make([]chan struct{}, len(steps))
	for i := range done {
		done[i] = make(chan struct{})
	}
	// stoppedEarly is set when a step was stopped, or never ran for the
	// stop. late is the first step failure the Build's stop got ahead of.
	var stoppedEarly atomic.Bool
	var late atomic.Pointer[stepFailure]
	var wg sync.WaitGroup
	for i := range steps {
		deps := dependencies(steps, i)
		wg.Go(func() {
			defer close(done[i])
			for _, d := range deps {
				select {
				case <-done[d]:
				case <-ctx.Done():
					stoppedEarly.Store(true)
					return
				}
			}
			s.reach(ctx, b, "step "+strconv.Itoa(i))
			if ctx.Err() != nil {
				stoppedEarly.Store(true)
				return
			}
			err := s.runStep(ctx, rt, r, b, i, vols, network)
			var f *stepFailure
			switch {
			case errors.Is(err, errStepStopped):
				stoppedEarly.Store(true)
			case errors.As(err, &f):
				fail(f)
				if context.Cause(ctx) != error(f) {
					late.CompareAndSwap(nil, f)
				}
			}
		})
	}
	wg.Wait()
	var f *stepFailure
	if errors.As(context.Cause(ctx), &f) {
		return f.o, true
	}
	if stoppedEarly.Load() {
		return outcome{status: cloudbuildpb.Build_CANCELLED, detail: "Build was cancelled."}, false
	}
	// Every step ended on its own, though the Build's stop came first.
	if f := late.Load(); f != nil {
		return f.o, true
	}
	if err := s.publish(r.ctx, rt, r, b, vols, daemon); err != nil {
		if errors.As(err, &f) {
			return f.o, true
		}
		return outcome{status: cloudbuildpb.Build_CANCELLED, detail: "Build was cancelled."}, false
	}
	return outcome{status: cloudbuildpb.Build_SUCCESS}, true
}

// dependencies returns the indexes of the steps step i waits for.
func dependencies(steps []*cloudbuildpb.BuildStep, i int) []int {
	wait := steps[i].GetWaitFor()
	if len(wait) == 0 {
		deps := make([]int, i)
		for j := range deps {
			deps[j] = j
		}
		return deps
	}
	var deps []int
	for j, st := range steps[:i] {
		if st.GetId() != "" && slices.Contains(wait, st.GetId()) {
			deps = append(deps, j)
		}
	}
	return deps
}

// network returns the Build network, which the Service owns and every
// Build's steps join, creating it on first use, and the Build agent on it,
// starting it if needed. Steps reach the internet through the network,
// and the Instance through the agent.
func (s *Service) network(ctx context.Context, rt *runtime.Manager) (buildNet, error) {
	s.netMu.Lock()
	defer s.netMu.Unlock()
	name := rt.Name("build")
	if _, err := rt.InspectNetwork(ctx, name); errors.Is(err, runtime.ErrNotFound) {
		if _, err := rt.CreateNetwork(ctx, runtime.NetworkSpec{Name: name, Labels: rt.Labels("build", "", "network")}); err != nil {
			return buildNet{}, err
		}
	} else if err != nil {
		return buildNet{}, err
	}
	addr, err := s.buildAgent(ctx, rt, name)
	if err != nil {
		return buildNet{}, fmt.Errorf("starting the Build agent: %w", err)
	}
	return buildNet{name: name, agent: addr}, nil
}

// buildVolumes names the runtime volumes of a Build by mount path.
type buildVolumes map[string]string

// createVolumes creates the Build's /workspace, its builder home and the
// volumes its steps and options name, each labelled as the Build's.
func (s *Service) createVolumes(ctx context.Context, rt *runtime.Manager, b *cloudbuildpb.Build) (buildVolumes, error) {
	vols := buildVolumes{}
	add := func(kind, name, at string) error {
		v := rt.Name("build", b.GetId(), kind)
		if name != "" {
			v = rt.Name("build", b.GetId(), kind, name)
		}
		if err := rt.CreateVolume(ctx, v, rt.Labels("build", b.GetName(), kind)); err != nil {
			return err
		}
		vols[at] = v
		return nil
	}
	if err := add("workspace", "", workspace); err != nil {
		return nil, err
	}
	if err := add("home", "", home); err != nil {
		return nil, err
	}
	user := slices.Clone(b.GetOptions().GetVolumes())
	for _, st := range b.GetSteps() {
		user = append(user, st.GetVolumes()...)
	}
	named := map[string]bool{}
	for _, v := range user {
		if named[v.GetName()] {
			continue
		}
		named[v.GetName()] = true
		if err := add("volume", v.GetName(), "volume:"+v.GetName()); err != nil {
			return nil, err
		}
	}
	return vols, nil
}

// mounts returns a step's mounts: the shared ones and its volumes.
func (v buildVolumes) mounts(b *cloudbuildpb.Build, st *cloudbuildpb.BuildStep) []runtime.Mount {
	ms := []runtime.Mount{
		{Type: "volume", Source: v[workspace], Target: workspace},
		{Type: "volume", Source: v[home], Target: home},
	}
	for _, uv := range append(slices.Clone(b.GetOptions().GetVolumes()), st.GetVolumes()...) {
		ms = append(ms, runtime.Mount{Type: "volume", Source: v["volume:"+uv.GetName()], Target: path.Clean(uv.GetPath())})
	}
	return ms
}

// runStep runs step i in a container and records its status. It returns
// a *stepFailure when the step ends the Build, errStepStopped when the
// step was stopped, and nil when the step succeeded or failed but was
// allowed to.
func (s *Service) runStep(ctx context.Context, rt *runtime.Manager, r *liveBuild, b *cloudbuildpb.Build, i int, vols buildVolumes, network buildNet) error {
	st := b.GetSteps()[i]
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	r.setStepStop(i, stop)
	defer r.setStepStop(i, nil)
	label := stepLabel(i, st)
	s.logs.line(r.ref, "Starting "+label)
	defer s.logs.line(r.ref, "Finished "+label)
	start := timestamppb.New(s.now())
	s.setStep(r, i, func(step *cloudbuildpb.BuildStep) {
		step.Status = cloudbuildpb.Build_WORKING
		step.Timing = &cloudbuildpb.TimeSpan{StartTime: start}
	})
	ended := func(status cloudbuildpb.Build_Status, code int32) {
		end := timestamppb.New(s.now())
		s.setStep(r, i, func(step *cloudbuildpb.BuildStep) {
			step.Status, step.ExitCode = status, code
			step.Timing = &cloudbuildpb.TimeSpan{StartTime: start, EndTime: end}
		})
	}
	failure := func(code int32, detail string) error {
		ended(cloudbuildpb.Build_FAILURE, code)
		detail = fmt.Sprintf("build step %d %q failed: %s", i, st.GetName(), detail)
		return &stepFailure{outcome{status: cloudbuildpb.Build_FAILURE, detail: "Build step failure: " + detail,
			failure: &cloudbuildpb.Build_FailureInfo{Type: cloudbuildpb.Build_FailureInfo_USER_BUILD_STEP, Detail: detail}}}
	}
	// failed is a step failing with an error, which may come of its stop.
	failed := func(detail string) error {
		if ctx.Err() != nil {
			return s.stopped(ctx, ended)
		}
		return failure(0, detail)
	}

	pullStart := timestamppb.New(s.now())
	if err := rt.EnsureImage(ctx, st.GetName(), rt.Offline); err != nil {
		return failed("pulling the image: " + err.Error())
	}
	pullEnd := timestamppb.New(s.now())
	s.setStep(r, i, func(step *cloudbuildpb.BuildStep) {
		step.PullTiming = &cloudbuildpb.TimeSpan{StartTime: pullStart, EndTime: pullEnd}
	})

	// The container is created even when the step is stopped meanwhile:
	// a create cut off mid-request may still be carried out by the
	// runtime, after the Build's cleanup looked for its containers.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	id, err := rt.CreateContainer(cctx, s.stepSpec(rt, b, i, vols, network, r.secrets, r.substitutions))
	cancel()
	if err != nil {
		return failed(err.Error())
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = rt.RemoveContainer(rctx, id, true)
	}()
	if ctx.Err() != nil {
		return s.stopped(ctx, ended)
	}
	files := s.stepFiles()
	if sc := st.GetScript(); sc != "" {
		files = append(files, runtime.File{Name: strings.TrimPrefix(scriptFile, "/"), Mode: 0o755, Data: []byte(sc)})
	}
	if len(files) > 0 {
		if err := rt.CopyTo(ctx, id, "/", files); err != nil {
			return failed("writing the step's files: " + err.Error())
		}
	}
	if err := rt.StartContainer(ctx, id); err != nil {
		return failed(err.Error())
	}
	if c, err := rt.InspectContainer(ctx, id); err == nil && c.IPs[network.name] != "" {
		ip := c.IPs[network.name]
		s.recordStep(ip, r.ref)
		defer s.forgetStep(ip) // runs before the container is removed
	}
	copied := s.follow(ctx, rt, r.ref, id, label+": ")
	code, err := rt.WaitContainer(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			return s.stopped(ctx, ended)
		}
		return failed(err.Error())
	}
	s.reach(ctx, b, "step "+strconv.Itoa(i)+" exited")
	waitCopied(copied)
	if code == 0 {
		ended(cloudbuildpb.Build_SUCCESS, 0)
		return nil
	}
	if st.GetAllowFailure() || slices.Contains(st.GetAllowExitCodes(), int32(code)) {
		ended(cloudbuildpb.Build_FAILURE, int32(code))
		return nil
	}
	// The step exited on its own: its failure stands even if the Build
	// was stopped meanwhile (#455).
	return failure(int32(code), "step exited with non-zero status: "+strconv.Itoa(code))
}

// stopped records a step that was stopped while it ran: by its own
// timeout, which ends the Build TIMEOUT, or with the Build, returning
// errStepStopped.
func (s *Service) stopped(ctx context.Context, ended func(cloudbuildpb.Build_Status, int32)) error {
	if errors.Is(context.Cause(ctx), errStepTimeout) {
		ended(cloudbuildpb.Build_TIMEOUT, 0)
		return &stepFailure{outcome{status: cloudbuildpb.Build_TIMEOUT, detail: "Build step timed out."}}
	}
	status := cloudbuildpb.Build_CANCELLED
	if errors.Is(context.Cause(ctx), errTimeout) {
		status = cloudbuildpb.Build_TIMEOUT
	}
	ended(status, 0)
	return errStepStopped
}

// stepSpec is the container of step i: its image, entrypoint and args or
// script, env (substs when the step is automapped, then the Build's
// options, the step's own and its secretEnv payloads from secrets), dir
// under the source's, the Build's volumes, the Build daemon's socket and
// the Build network, labelled as the Build's so cleanup finds it. The Build agent is its resolver, and the
// variables that point clients at the agent come before the Build's own.
func (s *Service) stepSpec(rt *runtime.Manager, b *cloudbuildpb.Build, i int, vols buildVolumes, network buildNet, secrets, substs map[string]string) runtime.ContainerSpec {
	st := b.GetSteps()[i]
	agentEnv, hosts := s.stepEnv(network.agent)
	env := append([]string{"HOME=" + home}, agentEnv...)
	if automapped(b, st) {
		env = append(env, automapEnv(substs)...)
	}
	env = append(env, b.GetOptions().GetEnv()...)
	spec := runtime.ContainerSpec{
		Name:       rt.Name("build", b.GetId(), "step", strconv.Itoa(i)),
		Image:      st.GetName(),
		Env:        append(append(env, st.GetEnv()...), secretEnv(b, i, secrets)...),
		Labels:     rt.Labels("build", b.GetName(), "step"),
		WorkingDir: path.Join(sourceDir(b), st.GetDir()),
		Mounts:     append(vols.mounts(b, st), runtime.Mount{Type: "volume", Source: daemonVolume(rt, "run"), Target: stepRun}),
		Networks:   []runtime.Attachment{{Network: network.name}},
		DNS:        []string{network.agent},
		ExtraHosts: hosts,
	}
	if path.IsAbs(st.GetDir()) {
		spec.WorkingDir = path.Clean(st.GetDir())
	}
	if ep := st.GetEntrypoint(); ep != "" {
		spec.Entrypoint = []string{ep}
	}
	if len(st.GetArgs()) > 0 {
		spec.Cmd = st.GetArgs()
	}
	if sc := st.GetScript(); sc != "" {
		switch {
		case spec.Entrypoint != nil:
			spec.Entrypoint = append(spec.Entrypoint, scriptFile)
		case strings.HasPrefix(sc, "#!"):
			spec.Entrypoint = []string{scriptFile}
		default:
			spec.Entrypoint = []string{"/bin/sh", scriptFile}
		}
	}
	return spec
}

// follow copies a container's output into the Build's log, each line
// after prefix; the returned channel is closed once the output has ended.
// The copy counts as the Service's work, so that Reset waits for it before
// clearing the logs.
func (s *Service) follow(ctx context.Context, rt *runtime.Manager, ref buildRef, id, prefix string) <-chan struct{} {
	copied := make(chan struct{})
	rc, err := rt.FollowLogs(ctx, id)
	if err != nil {
		s.logs.line(ref, prefix+"(no output: "+err.Error()+")")
		close(copied)
		return copied
	}
	s.work.Enter()
	go func() {
		defer s.work.Exit()
		defer close(copied)
		defer rc.Close()
		w := s.logs.writer(ref, prefix)
		_, _ = io.Copy(w, rc)
		w.flush()
	}()
	return copied
}

// waitCopied waits briefly for a step's output to be copied after the
// step exited.
func waitCopied(copied <-chan struct{}) {
	select {
	case <-copied:
	case <-time.After(5 * time.Second):
	}
}

// setStep changes step i of a WORKING Build, unless it was halted.
func (s *Service) setStep(r *liveBuild, i int, fn func(*cloudbuildpb.BuildStep)) {
	if r.halted() {
		return
	}
	if err := s.update(r.ref, func(st *stored) { fn(st.build.Steps[i]) }); err != nil {
		s.env.Log.Error("build: step status", "build", r.ref.name(), "step", i, "err", err)
	}
}

// stepLabel names step i in the log as Cloud Build does.
func stepLabel(i int, st *cloudbuildpb.BuildStep) string {
	if st.GetId() != "" {
		return fmt.Sprintf("Step #%d - %q", i, st.GetId())
	}
	return fmt.Sprintf("Step #%d", i)
}

// removeObjects removes a Build's containers and then its volumes,
// trying again while a volume is still held by a container the runtime
// was finishing.
func (s *Service) removeObjects(ref buildRef) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rt, err := s.env.Containers.Runtime(ctx)
	if err != nil {
		return
	}
	s.forgetSteps(ref)
	sel := map[string]string{runtime.LabelInstance: rt.InstanceID, runtime.LabelService: "build", runtime.LabelResource: ref.name()}
	for attempt := 0; ; attempt++ {
		var errs []error
		cs, err := rt.ListContainers(ctx, sel)
		errs = append(errs, err)
		for _, c := range cs {
			errs = append(errs, rt.RemoveContainer(ctx, c.ID, true))
		}
		vs, err := rt.ListVolumes(ctx, sel)
		errs = append(errs, err)
		for _, v := range vs {
			errs = append(errs, rt.RemoveVolume(ctx, v))
		}
		err = errors.Join(errs...)
		if err == nil {
			return
		}
		if attempt == 4 {
			s.env.Log.Warn("build: remove the Build's containers and volumes", "build", ref.name(), "err", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}
