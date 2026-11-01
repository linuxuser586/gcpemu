package build

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"go.yaml.in/yaml/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/store"
)

var (
	_ emu.Seeder       = (*Service)(nil)
	_ emu.SeedExporter = (*Service)(nil)
)

// seedFile is the "build" section of a seed file (FR-CORE-011). Build
// triggers and worker pools use the REST JSON field names; location
// defaults to global for a Build trigger. A Build trigger needs a name,
// which Seeding matches it by, so Seeding again updates it:
//
//	build:
//	  triggers:
//	    - project: my-project
//	      location: us-central1      # optional
//	      name: on-release
//	      pubsubConfig: {topic: projects/my-project/topics/releases}
//	      substitutions: {_TAG: $(body.message.data.tag)}
//	      build:
//	        steps:
//	          - {name: busybox, args: [echo, $_TAG]}
//	  workerPools:
//	    - project: my-project
//	      location: us-central1
//	      id: private
//	      privatePoolV1Config: {workerConfig: {machineType: e2-standard-4}}
//
// The section comes after pubsub's, secrets' and gcs' in seed order, so
// a Build trigger's topic exists when it is seeded.
type seedFile struct {
	Triggers    []seedItem `yaml:"triggers,omitempty"`
	WorkerPools []seedItem `yaml:"workerPools,omitempty"`
}

// seedItem is one Build trigger or worker pool: where it is, a pool's
// ID, and the rest of its REST JSON fields.
type seedItem struct {
	Project  string         `yaml:"project"`
	Location string         `yaml:"location,omitempty"`
	ID       string         `yaml:"id,omitempty"`
	Rest     map[string]any `yaml:",inline"`
}

// ApplySeed creates or updates the seeded Build triggers and worker
// pools. It is idempotent.
func (s *Service) ApplySeed(ctx context.Context, section *yaml.Node, baseDir string) error {
	var f seedFile
	if err := section.Decode(&f); err != nil {
		return err
	}
	for i, it := range f.Triggers {
		if err := s.seedTrigger(ctx, it); err != nil {
			return fmt.Errorf("triggers[%d] (%v): %w", i, it.Rest["name"], err)
		}
	}
	for i, it := range f.WorkerPools {
		if err := s.seedPool(it); err != nil {
			return fmt.Errorf("workerPools[%d] (%s): %w", i, it.ID, err)
		}
	}
	return nil
}

// fromSeed decodes an item's fields into m.
func fromSeed(it seedItem, m proto.Message) error {
	if it.Project == "" {
		return fmt.Errorf("project is required")
	}
	b, err := json.Marshal(it.Rest)
	if err != nil {
		return err
	}
	return protojson.Unmarshal(b, m)
}

func (s *Service) seedTrigger(ctx context.Context, it seedItem) error {
	t := &cloudbuildpb.BuildTrigger{}
	if err := fromSeed(it, t); err != nil {
		return err
	}
	if err := s.env.EnsureProject(it.Project); err != nil {
		return err
	}
	if t.GetName() == "" {
		return fmt.Errorf("name is required")
	}
	location := cmp.Or(it.Location, globalLocation)
	cur, err := s.loadTrigger(triggerRef{it.Project, location, t.GetName()})
	if err == nil {
		_, err = s.replaceTrigger(ctx, cur, t)
		return err
	}
	_, err = s.createTrigger(ctx, it.Project, location, t)
	return err
}

func (s *Service) seedPool(it seedItem) error {
	wp := &cloudbuildpb.WorkerPool{}
	if err := fromSeed(it, wp); err != nil {
		return err
	}
	if err := s.env.EnsureProject(it.Project); err != nil {
		return err
	}
	if !poolID.MatchString(it.ID) || it.Location == "" || it.Location == globalLocation {
		return fmt.Errorf("a worker pool needs an id and a regional location")
	}
	wp.Name = locationName(it.Project, it.Location) + "/workerPools/" + it.ID
	if err := validatePool(wp); err != nil {
		return err
	}
	cur, err := s.loadPool(wp.Name)
	if err == nil {
		wp.Uid, wp.CreateTime = cur.GetUid(), cur.GetCreateTime()
		_, err = s.savePool(wp, false)
		return err
	}
	if apierr.From(err).Code != codes.NotFound {
		return err
	}
	wp.Uid, wp.CreateTime = s.env.IDs.UUID(), timestamppb.New(s.env.Clock.Now())
	_, err = s.savePool(wp, true)
	return err
}

// ExportSeed implements emu.SeedExporter: every Build trigger and worker
// pool, without the fields the API sets. Builds are not exported.
func (s *Service) ExportSeed(ctx context.Context) (any, error) {
	var out seedFile
	err := s.env.Store.View(func(tx store.Tx) error {
		err := scanTriggers(tx, "", func(t *cloudbuildpb.BuildTrigger) {
			r := triggerRefOf(t)
			t = proto.Clone(t).(*cloudbuildpb.BuildTrigger)
			clearOutputs(t)
			it := seedItem{Project: r.Project}
			if r.Location != globalLocation {
				it.Location = r.Location
			}
			out.Triggers = append(out.Triggers, toSeed(it, t))
		})
		if err != nil {
			return err
		}
		return scanPools(tx, "", func(wp *cloudbuildpb.WorkerPool) {
			project, location, id, _ := parsePool(wp.GetName())
			wp = proto.Clone(wp).(*cloudbuildpb.WorkerPool)
			wp.Name, wp.Uid, wp.CreateTime, wp.UpdateTime, wp.DeleteTime, wp.Etag = "", "", nil, nil, nil, ""
			wp.State = cloudbuildpb.WorkerPool_STATE_UNSPECIFIED
			out.WorkerPools = append(out.WorkerPools, toSeed(seedItem{Project: project, Location: location, ID: id}, wp))
		})
	})
	if err != nil {
		return nil, err
	}
	if len(out.Triggers)+len(out.WorkerPools) == 0 {
		return nil, nil
	}
	byPlace := func(x, y seedItem) int {
		return cmp.Or(cmp.Compare(x.Project, y.Project), cmp.Compare(x.Location, y.Location),
			cmp.Compare(x.ID, y.ID), cmp.Compare(fmt.Sprint(x.Rest["name"]), fmt.Sprint(y.Rest["name"])))
	}
	slices.SortFunc(out.Triggers, byPlace)
	slices.SortFunc(out.WorkerPools, byPlace)
	return out, nil
}

// toSeed returns it with m's REST JSON fields.
func toSeed(it seedItem, m proto.Message) seedItem {
	b, _ := protojson.Marshal(m)
	_ = json.Unmarshal(b, &it.Rest)
	return it
}
