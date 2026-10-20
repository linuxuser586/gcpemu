package build

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strings"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/listpage"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Build triggers: create, get, list, patch, delete and run, global
// (projects/*/triggers) and regional (projects/*/locations/*/triggers).
// A pubsubConfig Build trigger owns a push subscription to its topic
// (trigger_events.go), and a webhookConfig one is fired by
// triggers.webhook; the SCM events of github, triggerTemplate and
// repositoryEventConfig are Recorded and never fire. Running a Build
// trigger turns it into a Build (trigger_run.go). Like the Builds API,
// Build trigger calls check the cloudbuild.builds permissions.

// nsTriggers holds every Build trigger, keyed by triggerRef.key.
const nsTriggers = "build/triggers"

// triggerName is the form of a Build trigger's name.
var triggerName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,62}[a-zA-Z0-9])?$`)

// triggerRef names one Build trigger. ID is the trigger's ID, or, as
// parsed from a request, its ID or its name.
type triggerRef struct{ Project, Location, ID string }

func (r triggerRef) name() string {
	return locationName(r.Project, r.Location) + "/triggers/" + r.ID
}

func (r triggerRef) key() string { return locationKey(r.Project, r.Location) + r.ID }

func (r triggerRef) resource() string { return resRoot + r.name() }

// parseTrigger resolves the Build trigger a call names: a project ID and
// trigger ID or name (the global location), or a resource name.
func parseTrigger(projectID, id, name string) (triggerRef, error) {
	if name == "" {
		if projectID == "" || id == "" {
			return triggerRef{}, apierr.InvalidArgument("project_id and trigger_id are required.")
		}
		return triggerRef{projectID, globalLocation, id}, nil
	}
	segs := strings.Split(name, "/")
	if len(segs) != 6 || segs[0] != "projects" || segs[2] != "locations" || segs[4] != "triggers" ||
		segs[1] == "" || segs[3] == "" || segs[5] == "" {
		return triggerRef{}, apierr.InvalidArgument("Invalid trigger name %q: want projects/{project}/locations/{location}/triggers/{trigger}.", name)
	}
	if projectID != "" && projectID != segs[1] {
		return triggerRef{}, apierr.InvalidArgument("project_id %q does not match trigger %q.", projectID, name)
	}
	return triggerRef{segs[1], segs[3], segs[5]}, nil
}

// triggerRefOf returns the ref of a stored Build trigger.
func triggerRefOf(t *cloudbuildpb.BuildTrigger) triggerRef {
	r, _ := parseTrigger("", "", t.GetResourceName())
	r.ID = t.GetId()
	return r
}

func putTrigger(tx store.Tx, t *cloudbuildpb.BuildTrigger) error {
	b, err := protojson.Marshal(t)
	if err != nil {
		return err
	}
	return tx.Put(nsTriggers, triggerRefOf(t).key(), b)
}

func decodeTrigger(b []byte) (*cloudbuildpb.BuildTrigger, error) {
	t := &cloudbuildpb.BuildTrigger{}
	return t, protojson.Unmarshal(b, t)
}

// scanTriggers calls fn with each Build trigger under the key prefix.
func scanTriggers(tx store.Tx, prefix string, fn func(*cloudbuildpb.BuildTrigger)) error {
	var err error
	tx.Scan(nsTriggers, prefix, func(_ string, b []byte) bool {
		var t *cloudbuildpb.BuildTrigger
		if t, err = decodeTrigger(b); err != nil {
			return false
		}
		fn(t)
		return true
	})
	return err
}

// getTrigger returns the Build trigger r names by its ID or its name.
func getTrigger(tx store.Tx, r triggerRef) (*cloudbuildpb.BuildTrigger, error) {
	if b, ok := tx.Get(nsTriggers, r.key()); ok {
		return decodeTrigger(b)
	}
	var found *cloudbuildpb.BuildTrigger
	err := scanTriggers(tx, locationKey(r.Project, r.Location), func(t *cloudbuildpb.BuildTrigger) {
		if t.GetName() == r.ID {
			found = t
		}
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, apierr.NotFound("Trigger %q not found.", r.name())
	}
	return found, nil
}

// loadTrigger returns the Build trigger r names.
func (s *Service) loadTrigger(r triggerRef) (*cloudbuildpb.BuildTrigger, error) {
	var t *cloudbuildpb.BuildTrigger
	err := s.env.Store.View(func(tx store.Tx) error {
		var err error
		t, err = getTrigger(tx, r)
		return err
	})
	return t, err
}

// nameTaken reports whether another Build trigger in the location has
// the name.
func nameTaken(tx store.Tx, t *cloudbuildpb.BuildTrigger) (bool, error) {
	r := triggerRefOf(t)
	taken := false
	err := scanTriggers(tx, locationKey(r.Project, r.Location), func(o *cloudbuildpb.BuildTrigger) {
		taken = taken || (o.GetName() == t.GetName() && o.GetId() != t.GetId())
	})
	return taken, err
}

// validateTrigger refuses a Build trigger that the real API would.
func validateTrigger(t *cloudbuildpb.BuildTrigger) error {
	if !triggerName.MatchString(t.GetName()) {
		return apierr.InvalidArgument("invalid trigger: name %q must be 1-64 letters, digits and dashes, beginning and ending with a letter or digit.", t.GetName())
	}
	if t.GetBuildTemplate() == nil {
		return apierr.InvalidArgument("invalid trigger: one of build, filename, gitFileSource or autodetect is required.")
	}
	for k := range t.GetSubstitutions() {
		if !userKey.MatchString(k) {
			return apierr.InvalidArgument("invalid trigger: substitution key %q does not match ^_[A-Z0-9_]+$.", k)
		}
	}
	if len(t.GetSubstitutions()) > maxSubstitutions {
		return apierr.InvalidArgument("invalid trigger: more than %d substitutions.", maxSubstitutions)
	}
	if t.GetGithub() != nil && t.GetTriggerTemplate() != nil {
		return apierr.InvalidArgument("invalid trigger: github and triggerTemplate are mutually exclusive.")
	}
	if pc := t.GetPubsubConfig(); pc != nil {
		if segs := strings.Split(pc.GetTopic(), "/"); len(segs) != 4 || segs[0] != "projects" || segs[2] != "topics" || segs[1] == "" || segs[3] == "" {
			return apierr.InvalidArgument("invalid trigger: pubsubConfig.topic %q must be of the form projects/{project}/topics/{topic}.", pc.GetTopic())
		}
	}
	if wc := t.GetWebhookConfig(); wc != nil {
		if segs := strings.Split(wc.GetSecret(), "/"); len(segs) < 6 || segs[0] != "projects" || segs[len(segs)-2] != "versions" {
			return apierr.InvalidArgument("invalid trigger: webhookConfig.secret %q must be a Secret Manager secret version, projects/{project}/secrets/{secret}/versions/{version}.", wc.GetSecret())
		}
	}
	if sa := t.GetServiceAccount(); sa != "" {
		if _, err := serviceAccountEmail(sa); err != nil {
			return apierr.InvalidArgument("invalid trigger: serviceAccount %q must be of the form projects/{project}/serviceAccounts/{email}.", sa)
		}
	}
	return nil
}

// clearOutputs empties the fields of a Build trigger that the API sets.
func clearOutputs(t *cloudbuildpb.BuildTrigger) {
	t.Id, t.ResourceName, t.CreateTime = "", "", nil
	if pc := t.GetPubsubConfig(); pc != nil {
		pc.Subscription, pc.State = "", cloudbuildpb.PubsubConfig_STATE_UNSPECIFIED
	}
	if wc := t.GetWebhookConfig(); wc != nil {
		wc.State = cloudbuildpb.WebhookConfig_STATE_UNSPECIFIED
	}
}

// setOutputs fills the fields of a Build trigger that the API sets, but
// for its ID and creation time.
func setOutputs(t *cloudbuildpb.BuildTrigger, r triggerRef) {
	t.Id, t.ResourceName = r.ID, r.name()
	if pc := t.GetPubsubConfig(); pc != nil {
		pc.Subscription, pc.State = subscriptionName(t), cloudbuildpb.PubsubConfig_OK
	}
	if wc := t.GetWebhookConfig(); wc != nil {
		wc.State = cloudbuildpb.WebhookConfig_OK
	}
}

// createTrigger stores a new Build trigger in a location, with the
// subscription a pubsubConfig needs.
func (s *Service) createTrigger(ctx context.Context, project, location string, in *cloudbuildpb.BuildTrigger) (*cloudbuildpb.BuildTrigger, error) {
	t := proto.Clone(in).(*cloudbuildpb.BuildTrigger)
	clearOutputs(t)
	r := triggerRef{project, location, s.env.IDs.UUID()}
	if t.Name == "" {
		t.Name = "trigger-" + r.ID[:8]
	}
	setOutputs(t, r)
	t.CreateTime = timestamppb.New(s.env.Clock.Now())
	if err := validateTrigger(t); err != nil {
		return nil, err
	}
	if err := s.checkUnique(t); err != nil {
		return nil, err
	}
	if err := s.ensureSubscription(ctx, nil, t); err != nil {
		return nil, err
	}
	if err := s.env.Store.Update(func(tx store.Tx) error { return putTrigger(tx, t) }); err != nil {
		_ = s.ensureSubscription(ctx, t, nil)
		return nil, err
	}
	return t, nil
}

// checkUnique refuses a Build trigger whose name another in its location
// has.
func (s *Service) checkUnique(t *cloudbuildpb.BuildTrigger) error {
	return s.env.Store.View(func(tx store.Tx) error {
		taken, err := nameTaken(tx, t)
		if err == nil && taken {
			err = apierr.AlreadyExists("Trigger with name %q already exists.", t.GetName())
		}
		return err
	})
}

// replaceTrigger stores t, an edit of the stored Build trigger cur,
// moving the subscription a pubsubConfig needs.
func (s *Service) replaceTrigger(ctx context.Context, cur, t *cloudbuildpb.BuildTrigger) (*cloudbuildpb.BuildTrigger, error) {
	clearOutputs(t)
	r := triggerRefOf(cur)
	setOutputs(t, r)
	t.CreateTime = cur.GetCreateTime()
	if err := validateTrigger(t); err != nil {
		return nil, err
	}
	if err := s.checkUnique(t); err != nil {
		return nil, err
	}
	if err := s.ensureSubscription(ctx, cur, t); err != nil {
		return nil, err
	}
	err := s.env.Store.Update(func(tx store.Tx) error { return putTrigger(tx, t) })
	return t, err
}

func (a *api) CreateBuildTrigger(ctx context.Context, req *cloudbuildpb.CreateBuildTriggerRequest) (*cloudbuildpb.BuildTrigger, error) {
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
	if req.GetTrigger() == nil {
		return nil, apierr.InvalidArgument("trigger is required.")
	}
	return a.s.createTrigger(ctx, project, location, req.GetTrigger())
}

func (a *api) GetBuildTrigger(ctx context.Context, req *cloudbuildpb.GetBuildTriggerRequest) (*cloudbuildpb.BuildTrigger, error) {
	r, err := parseTrigger(req.GetProjectId(), req.GetTriggerId(), req.GetName())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.builds.get", r.resource()); err != nil {
		return nil, err
	}
	return a.s.loadTrigger(r)
}

// ListBuildTriggers lists a location's Build triggers by name.
func (a *api) ListBuildTriggers(ctx context.Context, req *cloudbuildpb.ListBuildTriggersRequest) (*cloudbuildpb.ListBuildTriggersResponse, error) {
	project, location, err := parseParent(req.GetProjectId(), req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.builds.list", resRoot+locationName(project, location)); err != nil {
		return nil, err
	}
	var ts []*cloudbuildpb.BuildTrigger
	err = a.s.env.Store.View(func(tx store.Tx) error {
		return scanTriggers(tx, locationKey(project, location), func(t *cloudbuildpb.BuildTrigger) { ts = append(ts, t) })
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(ts, func(x, y *cloudbuildpb.BuildTrigger) int { return cmp.Compare(x.GetName(), y.GetName()) })
	ts, next, err := listpage.Offset(ts, req.GetPageToken(), req.GetPageSize(), pageOpts)
	if err != nil {
		return nil, err
	}
	return &cloudbuildpb.ListBuildTriggersResponse{Triggers: ts, NextPageToken: next}, nil
}

// UpdateBuildTrigger replaces a Build trigger with the one given, or,
// with an update_mask, the fields it names.
func (a *api) UpdateBuildTrigger(ctx context.Context, req *cloudbuildpb.UpdateBuildTriggerRequest) (*cloudbuildpb.BuildTrigger, error) {
	if req.GetTrigger() == nil {
		return nil, apierr.InvalidArgument("trigger is required.")
	}
	r, err := parseTrigger(req.GetProjectId(), req.GetTriggerId(), req.GetTrigger().GetResourceName())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.builds.update", r.resource()); err != nil {
		return nil, err
	}
	cur, err := a.s.loadTrigger(r)
	if err != nil {
		return nil, err
	}
	t := proto.Clone(req.GetTrigger()).(*cloudbuildpb.BuildTrigger)
	if paths := req.GetUpdateMask().GetPaths(); len(paths) > 0 {
		t = proto.Clone(cur).(*cloudbuildpb.BuildTrigger)
		if err := applyMask(t, req.GetTrigger(), paths); err != nil {
			return nil, err
		}
	}
	return a.s.replaceTrigger(ctx, cur, t)
}

// DeleteBuildTrigger deletes a Build trigger and the subscription its
// pubsubConfig made.
func (a *api) DeleteBuildTrigger(ctx context.Context, req *cloudbuildpb.DeleteBuildTriggerRequest) (*emptypb.Empty, error) {
	r, err := parseTrigger(req.GetProjectId(), req.GetTriggerId(), req.GetName())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.builds.update", r.resource()); err != nil {
		return nil, err
	}
	t, err := a.s.loadTrigger(r)
	if err != nil {
		return nil, err
	}
	if err := a.s.ensureSubscription(ctx, t, nil); err != nil {
		return nil, err
	}
	err = a.s.env.Store.Update(func(tx store.Tx) error { return tx.Delete(nsTriggers, triggerRefOf(t).key()) })
	return &emptypb.Empty{}, err
}

// RunBuildTrigger starts a Build from a Build trigger, at the revision
// the request's source gives.
func (a *api) RunBuildTrigger(ctx context.Context, req *cloudbuildpb.RunBuildTriggerRequest) (*longrunningpb.Operation, error) {
	r, err := parseTrigger(req.GetProjectId(), req.GetTriggerId(), req.GetName())
	if err != nil {
		return nil, err
	}
	if err := a.s.check(ctx, "cloudbuild.builds.create", r.resource()); err != nil {
		return nil, err
	}
	t, err := a.s.loadTrigger(r)
	if err != nil {
		return nil, err
	}
	b, provided, err := a.s.triggerBuild(ctx, t, triggerEvent{repo: req.GetSource()})
	if err != nil {
		return nil, err
	}
	if err := a.s.checkActAs(ctx, r.Project, b); err != nil {
		return nil, err
	}
	return a.s.submit(r.Project, r.Location, b, provided)
}
