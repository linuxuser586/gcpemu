package ar

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Cleanup policies (FR-AR-007, ADR 0070, #1039): the first step of the
// Registry cleanup pass runs each standard repository's policies. A
// version (manifest) is deleted, as DeleteVersion with force does, when at
// least one DELETE policy matches it and no KEEP policy does, unless it is
// protected: a manifest that an index remaining in its image references, a
// referrer whose subject remains in its image, or, with immutableTags, a
// tagged version. Protections are judged on the state at the start of the
// pass, so an index and its children go on different passes.

// policyVersion is one version of a repository under evaluation.
type policyVersion struct {
	img  string
	m    *manifestRec
	tags []string
}

func (v *policyVersion) key() string { return versionKey(v.img, v.m.Digest) }

// versionKey identifies a version within its repository.
func versionKey(img, digest string) string { return img + "|" + digest }

// runCleanupPolicies deletes, or under cleanupPolicyDryRun reports, what
// each standard repository's policies select.
func (s *Service) runCleanupPolicies(context.Context) error {
	now := s.env.Clock.Now()
	var refs []repoRef
	var repos []*artifactregistrypb.Repository
	err := s.env.Store.View(func(tx store.Tx) error {
		var err error
		refs, repos, err = listRepos(tx, "")
		return err
	})
	if err != nil {
		return err
	}
	for i, repo := range repos {
		if repo.GetMode() != artifactregistrypb.Repository_STANDARD_REPOSITORY || len(repo.GetCleanupPolicies()) == 0 {
			continue
		}
		if err := s.runRepoPolicies(refs[i], now); err != nil {
			return err
		}
	}
	return nil
}

// selected is a version the policies select, with the DELETE policies
// that matched it.
type selected struct {
	v        *policyVersion
	policies []string
}

func (s *Service) runRepoPolicies(ref repoRef, now time.Time) error {
	var picked []selected
	var dryRun bool
	var freed []string
	err := s.env.Store.Update(func(tx store.Tx) error {
		picked, freed = picked[:0], freed[:0]
		repo, err := getRepo(tx, ref)
		if err != nil || repo.GetMode() != artifactregistrypb.Repository_STANDARD_REPOSITORY {
			return nil // deleted or changed since the scan
		}
		dryRun = repo.GetCleanupPolicyDryRun()
		picked = selectVersions(repo, repoVersions(tx, ref), now)
		if dryRun {
			return nil
		}
		for _, p := range picked {
			f, err := deleteManifest(tx, ref, p.v.img, p.v.m.Digest)
			if err != nil {
				return err
			}
			freed = append(freed, f...)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.collect(freed)
	// One line per version deleted, or under a dry run per version that
	// would be, reported once per process (ADR 0070).
	for _, p := range picked {
		name := versionName(ref, p.v.img, p.v.m.Digest)
		if dryRun {
			if s.dryRunReported[name] {
				continue
			}
			s.dryRunReported[name] = true
		}
		tags := p.v.tags
		if tags == nil {
			tags = []string{} // an empty list, not null, in JSON logs
		}
		s.env.Log.Info("ar: cleanup policy matched a version", "repository", ref.name(), "version", name,
			"tags", tags, "policies", p.policies, "dryRun", dryRun)
	}
	return nil
}

// repoVersions returns every version of a repository with its tags.
func repoVersions(tx store.Tx, ref repoRef) []*policyVersion {
	tags := map[string][]string{}
	tx.Scan(nsTags, ref.key()+"|", func(k string, b []byte) bool {
		var t tagRec
		if json.Unmarshal(b, &t) == nil {
			_, img, tag := splitItemKey(k)
			k := versionKey(img, t.Digest)
			tags[k] = append(tags[k], tag)
		}
		return true
	})
	imgs, ms := listManifests(tx, ref, "")
	out := make([]*policyVersion, len(ms))
	for i, m := range ms {
		v := &policyVersion{img: imgs[i], m: m}
		v.tags = tags[v.key()]
		sort.Strings(v.tags)
		out[i] = v
	}
	return out
}

// selectVersions returns the versions the repository's policies delete,
// in key order, each with the IDs of the DELETE policies that matched it.
func selectVersions(repo *artifactregistrypb.Repository, vs []*policyVersion, now time.Time) []selected {
	protected := protectedVersions(repo, vs)
	ids := make([]string, 0, len(repo.GetCleanupPolicies()))
	for id := range repo.GetCleanupPolicies() {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	kept := map[string]bool{}
	for _, id := range ids {
		if p := repo.GetCleanupPolicies()[id]; p.GetAction() == artifactregistrypb.CleanupPolicy_KEEP && p.GetMostRecentVersions() != nil {
			for k := range mostRecent(p.GetMostRecentVersions(), vs) {
				kept[k] = true
			}
		}
	}
	var out []selected
	for _, v := range vs {
		if protected[v.key()] || kept[v.key()] {
			continue
		}
		var deleteIDs []string
		keep := false
		for _, id := range ids {
			p := repo.GetCleanupPolicies()[id]
			if p.GetCondition() == nil || !conditionMatches(p.GetCondition(), v, now) {
				continue
			}
			switch p.GetAction() {
			case artifactregistrypb.CleanupPolicy_KEEP:
				keep = true
			case artifactregistrypb.CleanupPolicy_DELETE:
				deleteIDs = append(deleteIDs, id)
			}
		}
		if !keep && len(deleteIDs) > 0 {
			out = append(out, selected{v, deleteIDs})
		}
	}
	return out
}

// protectedVersions returns the keys of versions no policy deletes.
func protectedVersions(repo *artifactregistrypb.Repository, vs []*policyVersion) map[string]bool {
	exists := map[string]bool{}
	for _, v := range vs {
		exists[v.key()] = true
	}
	immutable := repo.GetDockerConfig().GetImmutableTags()
	out := map[string]bool{}
	for _, v := range vs {
		for _, c := range v.m.Children {
			out[versionKey(v.img, c.Digest)] = true
		}
		if v.m.Subject != "" && exists[versionKey(v.img, v.m.Subject)] {
			out[v.key()] = true
		}
		if immutable && len(v.tags) > 0 {
			out[v.key()] = true
		}
	}
	return out
}

// mostRecent returns the keys of the keepCount newest versions of each
// package that matches the policy's package prefixes (every package when
// there are none), newest by create time, ties broken by version name.
func mostRecent(mr *artifactregistrypb.CleanupPolicyMostRecentVersions, vs []*policyVersion) map[string]bool {
	byImg := map[string][]*policyVersion{}
	for _, v := range vs {
		if len(mr.GetPackageNamePrefixes()) == 0 || hasAnyPrefix(v.img, mr.GetPackageNamePrefixes()) {
			byImg[v.img] = append(byImg[v.img], v)
		}
	}
	out := map[string]bool{}
	for _, list := range byImg {
		sort.Slice(list, func(i, j int) bool {
			a, b := list[i].m, list[j].m
			if !a.Created.Equal(b.Created) {
				return a.Created.After(b.Created)
			}
			return a.Digest < b.Digest
		})
		for i := 0; i < len(list) && i < int(mr.GetKeepCount()); i++ {
			out[list[i].key()] = true
		}
	}
	return out
}

// conditionMatches reports whether every field set in c holds for v.
func conditionMatches(c *artifactregistrypb.CleanupPolicyCondition, v *policyVersion, now time.Time) bool {
	switch c.GetTagState() {
	case artifactregistrypb.CleanupPolicyCondition_TAGGED:
		if len(v.tags) == 0 {
			return false
		}
	case artifactregistrypb.CleanupPolicyCondition_UNTAGGED:
		if len(v.tags) > 0 {
			return false
		}
	}
	if len(c.GetTagPrefixes()) > 0 {
		found := false
		for _, t := range v.tags {
			found = found || hasAnyPrefix(t, c.GetTagPrefixes())
		}
		if !found {
			return false
		}
	}
	if len(c.GetVersionNamePrefixes()) > 0 && !hasAnyPrefix(v.m.Digest, c.GetVersionNamePrefixes()) {
		return false
	}
	if len(c.GetPackageNamePrefixes()) > 0 && !hasAnyPrefix(v.img, c.GetPackageNamePrefixes()) {
		return false
	}
	age := now.Sub(v.m.Created)
	if c.OlderThan != nil && age <= c.GetOlderThan().AsDuration() {
		return false
	}
	if c.NewerThan != nil && age >= c.GetNewerThan().AsDuration() {
		return false
	}
	return true
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// maxCleanupPolicies is how many policies a repository may hold, and
// maxPolicyIDLen how many characters a policy ID must stay under.
const (
	maxCleanupPolicies = 10
	maxPolicyIDLen     = 128
)

// checkCleanupPolicies validates a repository's cleanup policies, with
// messages of our own (#1039).
func checkCleanupPolicies(ps map[string]*artifactregistrypb.CleanupPolicy) error {
	if len(ps) > maxCleanupPolicies {
		return apierr.InvalidArgument("A repository can have at most %d cleanup policies; got %d.", maxCleanupPolicies, len(ps))
	}
	for key, p := range ps {
		if key == "" || utf8.RuneCountInString(key) >= maxPolicyIDLen {
			return apierr.InvalidArgument("Cleanup policy ID %q must be 1 to %d characters long.", key, maxPolicyIDLen-1)
		}
		if p.GetId() != "" && p.GetId() != key {
			return apierr.InvalidArgument("Cleanup policy %q has id %q; the id must match its key.", key, p.GetId())
		}
		c, mr := p.GetCondition(), p.GetMostRecentVersions()
		switch {
		case p.GetAction() != artifactregistrypb.CleanupPolicy_DELETE && p.GetAction() != artifactregistrypb.CleanupPolicy_KEEP:
			return apierr.InvalidArgument("Cleanup policy %q must set action to DELETE or KEEP.", key)
		case (c == nil) == (mr == nil):
			return apierr.InvalidArgument("Cleanup policy %q must set exactly one of condition and mostRecentVersions.", key)
		case mr != nil && p.GetAction() != artifactregistrypb.CleanupPolicy_KEEP:
			return apierr.InvalidArgument("Cleanup policy %q: mostRecentVersions is only valid with action KEEP.", key)
		case mr != nil && mr.GetKeepCount() < 0:
			return apierr.InvalidArgument("Cleanup policy %q: mostRecentVersions.keepCount must not be negative.", key)
		case c == nil:
			continue
		case len(c.GetTagPrefixes()) > 0 && c.GetTagState() != artifactregistrypb.CleanupPolicyCondition_TAGGED:
			return apierr.InvalidArgument("Cleanup policy %q: condition.tagPrefixes requires tagState TAGGED.", key)
		case c.OlderThan != nil && c.GetOlderThan().AsDuration() < 0:
			return apierr.InvalidArgument("Cleanup policy %q: condition.olderThan must not be negative.", key)
		case c.NewerThan != nil && c.GetNewerThan().AsDuration() < 0:
			return apierr.InvalidArgument("Cleanup policy %q: condition.newerThan must not be negative.", key)
		}
	}
	return nil
}
