package pubsub

import (
	"context"
	"encoding/json"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// snapshotLifetime is how long a snapshot lives after its oldest message.
const snapshotLifetime = 7 * 24 * time.Hour

var snapshotUpdatable = map[string]bool{"labels": true, "expire_time": true}

func (s *Service) putSnapshot(w *writes, sn *snapshot) {
	cfg, _ := protojson.Marshal(sn.cfg)
	b, _ := json.Marshal(snapshotRecord{Config: cfg, Filter: sn.filt})
	w.put(nsSnapshots, sn.name, b)
}

// dropSnapshot deletes a snapshot and releases its messages (s.mu held).
func (s *Service) dropSnapshot(name string, w *writes) {
	sn, ok := s.snaps[name]
	if !ok {
		return
	}
	delete(s.snaps, name)
	for seq, m := range sn.msgs {
		w.del(nsBacklog, backlogKey(name, seq))
		s.unref(m, w)
	}
	w.del(nsSnapshots, name)
}

func (p *subscriberServer) CreateSnapshot(ctx context.Context, req *pubsubpb.CreateSnapshotRequest) (*pubsubpb.Snapshot, error) {
	s := p.s
	project, _, err := parseName(req.GetName(), kindSnapshots)
	if err != nil {
		return nil, err
	}
	if _, _, err := parseName(req.GetSubscription(), kindSubscriptions); err != nil {
		return nil, err
	}
	if err := validateLabels(req.GetLabels()); err != nil {
		return nil, err
	}
	if err := s.env.EnsureProject(project); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.snapshots.create", projectResource(project)); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.subscriptions.consume", fullName(req.GetSubscription())); err != nil {
		return nil, err
	}
	var w writes
	s.mu.Lock()
	sb, err := s.consumable(req.GetSubscription())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if sb.cfg.GetTopic() == deletedTopic {
		s.mu.Unlock()
		return nil, apierr.FailedPrecondition("The subscription's topic has been deleted.")
	}
	if _, ok := s.snaps[req.GetName()]; ok {
		s.mu.Unlock()
		return nil, alreadyExists(req.GetName())
	}
	now := s.env.Clock.Now()
	oldest := now
	sn := &snapshot{name: req.GetName(), filter: sb.filter, filt: sb.cfg.GetFilter(), msgs: map[uint64]*message{}}
	for seq, e := range sb.entries {
		if e.acked {
			continue
		}
		sn.msgs[seq] = e.m
		e.m.refs++
		w.put(nsBacklog, backlogKey(sn.name, seq), stateUnacked)
		if e.m.publish.Before(oldest) {
			oldest = e.m.publish
		}
	}
	sn.cfg = &pubsubpb.Snapshot{
		Name: req.GetName(), Topic: sb.cfg.GetTopic(), Labels: req.GetLabels(),
		ExpireTime: timestamppb.New(oldest.Add(snapshotLifetime)),
	}
	s.snaps[sn.name] = sn
	s.putSnapshot(&w, sn)
	out := proto.Clone(sn.cfg).(*pubsubpb.Snapshot)
	s.mu.Unlock()
	return out, s.commit(&w)
}

func (p *subscriberServer) GetSnapshot(ctx context.Context, req *pubsubpb.GetSnapshotRequest) (*pubsubpb.Snapshot, error) {
	s := p.s
	if _, _, err := parseName(req.GetSnapshot(), kindSnapshots); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.snapshots.get", fullName(req.GetSnapshot())); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sn, ok := s.snaps[req.GetSnapshot()]
	if !ok {
		return nil, notFound(req.GetSnapshot())
	}
	return proto.Clone(sn.cfg).(*pubsubpb.Snapshot), nil
}

func (p *subscriberServer) UpdateSnapshot(ctx context.Context, req *pubsubpb.UpdateSnapshotRequest) (*pubsubpb.Snapshot, error) {
	s := p.s
	name := req.GetSnapshot().GetName()
	if _, _, err := parseName(name, kindSnapshots); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.snapshots.update", fullName(name)); err != nil {
		return nil, err
	}
	var w writes
	s.mu.Lock()
	defer func() { s.mu.Unlock(); _ = s.commit(&w) }()
	sn, ok := s.snaps[name]
	if !ok {
		return nil, notFound(name)
	}
	cur := proto.Clone(sn.cfg).(*pubsubpb.Snapshot)
	if err := applyMask(cur, req.GetSnapshot(), req.GetUpdateMask().GetPaths(), snapshotUpdatable, "Snapshot"); err != nil {
		return nil, err
	}
	if err := validateLabels(cur.GetLabels()); err != nil {
		return nil, err
	}
	sn.cfg = cur
	s.putSnapshot(&w, sn)
	return proto.Clone(cur).(*pubsubpb.Snapshot), nil
}

func (p *subscriberServer) ListSnapshots(ctx context.Context, req *pubsubpb.ListSnapshotsRequest) (*pubsubpb.ListSnapshotsResponse, error) {
	s := p.s
	project, err := parseProject(req.GetProject())
	if err != nil {
		return nil, err
	}
	if err := s.env.EnsureProject(project); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.snapshots.list", projectResource(project)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var names []string
	for n := range s.snaps {
		if projectOf(n) == project {
			names = append(names, n)
		}
	}
	s.mu.Unlock()
	page, next, err := paginate(names, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	resp := &pubsubpb.ListSnapshotsResponse{NextPageToken: next}
	s.mu.Lock()
	for _, n := range page {
		if sn, ok := s.snaps[n]; ok {
			resp.Snapshots = append(resp.Snapshots, proto.Clone(sn.cfg).(*pubsubpb.Snapshot))
		}
	}
	s.mu.Unlock()
	return resp, nil
}

func (p *subscriberServer) DeleteSnapshot(ctx context.Context, req *pubsubpb.DeleteSnapshotRequest) (*emptypb.Empty, error) {
	s := p.s
	if _, _, err := parseName(req.GetSnapshot(), kindSnapshots); err != nil {
		return nil, err
	}
	if err := s.env.Auth.Check(ctx, "pubsub.snapshots.delete", fullName(req.GetSnapshot())); err != nil {
		return nil, err
	}
	var w writes
	s.mu.Lock()
	_, ok := s.snaps[req.GetSnapshot()]
	s.dropSnapshot(req.GetSnapshot(), &w)
	s.mu.Unlock()
	if !ok {
		return nil, notFound(req.GetSnapshot())
	}
	return &emptypb.Empty{}, s.commit(&w)
}

// Seek resets a subscription's ack state to a time or a snapshot (FR-PS-008).
func (p *subscriberServer) Seek(ctx context.Context, req *pubsubpb.SeekRequest) (*pubsubpb.SeekResponse, error) {
	s := p.s
	if err := s.checkConsume(ctx, req.GetSubscription()); err != nil {
		return nil, err
	}
	snapName := req.GetSnapshot()
	if snapName != "" {
		if _, _, err := parseName(snapName, kindSnapshots); err != nil {
			return nil, err
		}
		if err := s.env.Auth.Check(ctx, "pubsub.snapshots.seek", fullName(snapName)); err != nil {
			return nil, err
		}
	} else if req.GetTime() == nil {
		return nil, apierr.InvalidArgument("One of the fields time or snapshot must be set in the SeekRequest.")
	}
	var w writes
	s.mu.Lock()
	defer func() { s.mu.Unlock() }()
	sb, err := s.consumable(req.GetSubscription())
	if err != nil {
		return nil, err
	}
	target := map[uint64]*message{}
	if snapName != "" {
		sn, ok := s.snaps[snapName]
		if !ok {
			return nil, notFound(snapName)
		}
		if sn.cfg.GetTopic() != sb.cfg.GetTopic() {
			return nil, apierr.InvalidArgument("The subscription's topic is different from that of the snapshot.")
		}
		for seq, m := range sn.msgs {
			if filterMatches(sb.filter, m.pb.GetAttributes()) {
				target[seq] = m
			}
		}
	} else {
		at := req.GetTime().AsTime()
		for seq, e := range sb.entries {
			if !e.m.publish.Before(at) {
				target[seq] = e.m
			}
		}
		if t := s.topics[sb.cfg.GetTopic()]; t != nil {
			for _, m := range t.log {
				if !m.publish.Before(at) && filterMatches(sb.filter, m.pb.GetAttributes()) {
					target[m.seq] = m
				}
			}
		}
	}
	retain := sb.cfg.GetRetainAckedMessages()
	for seq, e := range sb.entries {
		if _, ok := target[seq]; ok {
			if e.acked {
				e.acked = false
				w.put(nsBacklog, backlogKey(sb.name, seq), stateUnacked)
			}
			continue
		}
		if e.acked {
			continue
		}
		if retain {
			e.acked = true
			w.put(nsBacklog, backlogKey(sb.name, seq), stateAcked)
			continue
		}
		s.endLease(sb, e)
		delete(sb.entries, seq)
		w.del(nsBacklog, backlogKey(sb.name, seq))
		s.unref(e.m, &w)
	}
	for seq, m := range target {
		if _, ok := sb.entries[seq]; ok {
			continue
		}
		sb.entries[seq] = newEntry(m)
		m.refs++
		w.put(nsBacklog, backlogKey(sb.name, seq), stateUnacked)
	}
	s.rebuildQueues(sb, time.Now())
	s.mu.Unlock()
	err = s.commit(&w)
	s.mu.Lock()
	if err != nil {
		return nil, apierr.Internal("seek: %v", err)
	}
	return &pubsubpb.SeekResponse{}, nil
}
