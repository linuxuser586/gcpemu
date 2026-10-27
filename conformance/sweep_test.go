//go:build conformance

package conformance

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	pubsub "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	dns "google.golang.org/api/dns/v1"
	iam "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
)

// sweepAge is how old a run's resources must be for TestSweep to delete
// them, so a run in progress keeps its own.
const sweepAge = 24 * time.Hour

// accountRE matches the account ID of a service account the suite made;
// service accounts take no labels, so the name carries the suffix.
var accountRE = regexp.MustCompile(`^gcpemu-([0-9a-z]{10})-`)

// TestSweep deletes what earlier record runs left in the project: every
// resource labelled gcpemu-conformance, and every service account named
// like the suite's, whose suffix dates it more than a day back. `task
// conformance:record` runs it first; it skips in compare mode.
func TestSweep(t *testing.T) {
	if !envMode().record {
		t.Skip("sweeps only in record mode, against GCPEMU_CONFORMANCE_PROJECT")
	}
	tg := gcpTarget(t)
	ctx := context.Background()
	p := tg.project
	opts := []option.ClientOption{option.WithScopes(cloudPlatform)}
	stale := func(suffix string) bool {
		at, ok := suffixTime(suffix)
		return ok && time.Since(at) > sweepAge
	}
	staleLabels := func(labels map[string]string) bool {
		s, ok := labels[labelKey]
		return ok && stale(s)
	}
	report := func(what string, err error) {
		if err != nil {
			t.Errorf("sweep %s: %v", what, err)
		} else {
			t.Logf("swept %s", what)
		}
	}

	if cl, err := storage.NewClient(ctx, opts...); err != nil {
		t.Error(err)
	} else {
		defer cl.Close()
		it := cl.Buckets(ctx, p)
		it.Prefix = "gcpemu-"
		for b, err := it.Next(); err == nil; b, err = it.Next() {
			if staleLabels(b.Labels) {
				deleteBucket(ctx, cl.Bucket(b.Name))
				report("bucket "+b.Name, nil)
			}
		}
	}

	if subs, err := pubsub.NewSubscriptionAdminClient(ctx, opts...); err != nil {
		t.Error(err)
	} else {
		defer subs.Close()
		it := subs.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: "projects/" + p})
		for s, err := it.Next(); err == nil; s, err = it.Next() {
			if staleLabels(s.Labels) {
				report(s.Name, subs.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: s.Name}))
			}
		}
	}
	if topics, err := pubsub.NewTopicAdminClient(ctx, opts...); err != nil {
		t.Error(err)
	} else {
		defer topics.Close()
		it := topics.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: "projects/" + p})
		for tp, err := it.Next(); err == nil; tp, err = it.Next() {
			if staleLabels(tp.Labels) {
				report(tp.Name, topics.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: tp.Name}))
			}
		}
	}

	if sm, err := secretmanager.NewClient(ctx, opts...); err != nil {
		t.Error(err)
	} else {
		defer sm.Close()
		it := sm.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: "projects/" + p, Filter: "labels." + labelKey + ":*"})
		for s, err := it.Next(); err == nil; s, err = it.Next() {
			if staleLabels(s.Labels) {
				report(s.Name, sm.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: s.Name}))
			}
		}
	}

	if d, err := dns.NewService(ctx, opts...); err != nil {
		t.Error(err)
	} else {
		_ = d.ManagedZones.List(p).Pages(ctx, func(zs *dns.ManagedZonesListResponse) error {
			for _, z := range zs.ManagedZones {
				if staleLabels(z.Labels) {
					report("zone "+z.Name, sweepZone(ctx, d, p, z.Name))
				}
			}
			return nil
		})
	}

	if ar, err := artifactregistry.NewClient(ctx, opts...); err != nil {
		t.Error(err)
	} else {
		defer ar.Close()
		it := ar.ListRepositories(ctx, &artifactregistrypb.ListRepositoriesRequest{Parent: "projects/" + p + "/locations/" + arLocation})
		for r, err := it.Next(); err == nil; r, err = it.Next() {
			if staleLabels(r.Labels) {
				op, err := ar.DeleteRepository(ctx, &artifactregistrypb.DeleteRepositoryRequest{Name: r.Name})
				if err == nil {
					err = op.Wait(ctx)
				}
				report(r.Name, err)
			}
		}
	}

	if s, err := iam.NewService(ctx, opts...); err != nil {
		t.Error(err)
	} else {
		_ = s.Projects.ServiceAccounts.List("projects/"+p).Pages(ctx, func(as *iam.ListServiceAccountsResponse) error {
			for _, a := range as.Accounts {
				id, _, _ := strings.Cut(a.Email, "@")
				if m := accountRE.FindStringSubmatch(id); m != nil && stale(m[1]) {
					_, err := s.Projects.ServiceAccounts.Delete(a.Name).Context(ctx).Do()
					report(a.Email, err)
				}
			}
			return nil
		})
	}
}

// sweepZone deletes the zone name after its record sets other than the
// apex NS and SOA, which Cloud DNS requires.
func sweepZone(ctx context.Context, d *dns.Service, p, name string) error {
	z, err := d.ManagedZones.Get(p, name).Context(ctx).Do()
	if err != nil {
		return err
	}
	var del []*dns.ResourceRecordSet
	err = d.ResourceRecordSets.List(p, name).Pages(ctx, func(rs *dns.ResourceRecordSetsListResponse) error {
		for _, r := range rs.Rrsets {
			if r.Name != z.DnsName || r.Type != "NS" && r.Type != "SOA" {
				del = append(del, r)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(del) > 0 {
		if _, err := d.Changes.Create(p, name, &dns.Change{Deletions: del}).Context(ctx).Do(); err != nil {
			return err
		}
	}
	return d.ManagedZones.Delete(p, name).Context(ctx).Do()
}
