package ar

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/store"
)

// Store namespaces. Registry keys are "<P/L/R>|<image>|<suffix>"; '|' never
// occurs in project, location, repository or image names.
const (
	nsRepos     = "ar/repos"     // P/L/R → protojson Repository
	nsManifests = "ar/manifests" // P/L/R|image|digest → manifestRec
	nsTags      = "ar/tags"      // P/L/R|image|tag → tagRec
	nsLinks     = "ar/links"     // P/L/R|image|digest → linkRec (blob in OCI repository)
	nsRefs      = "ar/blobrefs"  // digest|<manifest or link key>|m|b → "" (reverse index for GC)
	nsIAM       = "ar/iam"       // full resource name → protojson Policy (fallback without iam)
)

// manifestRec is a manifest pushed to an OCI repository (one image path).
type manifestRec struct {
	Digest       string            `json:"digest"`
	MediaType    string            `json:"mediaType"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Subject      string            `json:"subject,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	// Blobs are the config and layer digests an image manifest references.
	Blobs []string `json:"blobs,omitempty"`
	// Children are the manifests an index references.
	Children []childRec `json:"children,omitempty"`
	// ContentSize is the manifest size plus every referenced descriptor size.
	ContentSize int64      `json:"contentSize"`
	BuildTime   *time.Time `json:"buildTime,omitempty"`
	Created     time.Time  `json:"created"`
	Updated     time.Time  `json:"updated"`
}

// childRec is one entry of an index.
type childRec struct {
	Digest       string   `json:"digest"`
	MediaType    string   `json:"mediaType"`
	Size         int64    `json:"size"`
	OS           string   `json:"os,omitempty"`
	Architecture string   `json:"architecture,omitempty"`
	Variant      string   `json:"variant,omitempty"`
	OSVersion    string   `json:"osVersion,omitempty"`
	OSFeatures   []string `json:"osFeatures,omitempty"`
}

// tagRec points a tag at a manifest digest.
type tagRec struct {
	Digest  string    `json:"digest"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// linkRec records that a blob is part of an OCI repository.
type linkRec struct {
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

func imgPrefix(r repoRef, img string) string { return r.key() + "|" + img + "|" }

func itemKey(r repoRef, img, suffix string) string { return imgPrefix(r, img) + suffix }

// splitItemKey splits "P/L/R|image|suffix".
func splitItemKey(k string) (repoRef, string, string) {
	p := strings.SplitN(k, "|", 3)
	if len(p) != 3 {
		return repoRef{}, "", ""
	}
	return refFromKey(p[0]), p[1], p[2]
}

func refKey(digest, key, kind string) string { return digest + "|" + key + "|" + kind }

// getRepo loads a repository.
func getRepo(tx store.Tx, r repoRef) (*artifactregistrypb.Repository, error) {
	b, ok := tx.Get(nsRepos, r.key())
	if !ok {
		return nil, apierr.NotFound(notFound).WithReason(errDom, "RESOURCE_NOT_FOUND")
	}
	repo := &artifactregistrypb.Repository{}
	return repo, protojson.Unmarshal(b, repo)
}

func putRepo(tx store.Tx, r repoRef, repo *artifactregistrypb.Repository) error {
	b, err := protojson.Marshal(repo)
	if err != nil {
		return err
	}
	return tx.Put(nsRepos, r.key(), b)
}

// listRepos returns every repository under prefix ("P/L/" or "P/").
func listRepos(tx store.Tx, prefix string) ([]repoRef, []*artifactregistrypb.Repository, error) {
	var refs []repoRef
	var repos []*artifactregistrypb.Repository
	var err error
	tx.Scan(nsRepos, prefix, func(k string, b []byte) bool {
		repo := &artifactregistrypb.Repository{}
		if err = protojson.Unmarshal(b, repo); err != nil {
			return false
		}
		refs = append(refs, refFromKey(k))
		repos = append(repos, repo)
		return true
	})
	return refs, repos, err
}

// putManifest stores a manifest record and its blob reference.
func putManifest(tx store.Tx, r repoRef, img string, m *manifestRec) error {
	k := itemKey(r, img, m.Digest)
	if err := store.PutJSON(tx, nsManifests, k, m); err != nil {
		return err
	}
	return tx.Put(nsRefs, refKey(m.Digest, k, "m"), nil)
}

func getManifest(tx store.Tx, r repoRef, img, digest string) (*manifestRec, bool) {
	var m manifestRec
	if store.GetJSON(tx, nsManifests, itemKey(r, img, digest), &m) != nil {
		return nil, false
	}
	return &m, true
}

// deleteManifest removes a manifest and every tag pointing at it, returning
// the digest for garbage collection.
func deleteManifest(tx store.Tx, r repoRef, img, digest string) error {
	k := itemKey(r, img, digest)
	if err := tx.Delete(nsManifests, k); err != nil {
		return err
	}
	if err := tx.Delete(nsRefs, refKey(digest, k, "m")); err != nil {
		return err
	}
	for _, t := range tagsFor(tx, r, img, digest) {
		if err := tx.Delete(nsTags, itemKey(r, img, t)); err != nil {
			return err
		}
	}
	return nil
}

// tagsFor returns the tags of an image pointing at digest, sorted.
func tagsFor(tx store.Tx, r repoRef, img, digest string) []string {
	var out []string
	pre := imgPrefix(r, img)
	tx.Scan(nsTags, pre, func(k string, b []byte) bool {
		var t tagRec
		if json.Unmarshal(b, &t) == nil && t.Digest == digest {
			out = append(out, strings.TrimPrefix(k, pre))
		}
		return true
	})
	sort.Strings(out)
	return out
}

func getTag(tx store.Tx, r repoRef, img, tag string) (*tagRec, bool) {
	var t tagRec
	if store.GetJSON(tx, nsTags, itemKey(r, img, tag), &t) != nil {
		return nil, false
	}
	return &t, true
}

// putLink records a blob in an OCI repository.
func putLink(tx store.Tx, r repoRef, img, digest string, size int64, now time.Time) error {
	k := itemKey(r, img, digest)
	if store.Exists(tx, nsLinks, k) {
		return nil
	}
	if err := store.PutJSON(tx, nsLinks, k, linkRec{Size: size, Created: now}); err != nil {
		return err
	}
	return tx.Put(nsRefs, refKey(digest, k, "b"), nil)
}

func getLink(tx store.Tx, r repoRef, img, digest string) (*linkRec, bool) {
	var l linkRec
	if store.GetJSON(tx, nsLinks, itemKey(r, img, digest), &l) != nil {
		return nil, false
	}
	return &l, true
}

func deleteLink(tx store.Tx, r repoRef, img, digest string) error {
	k := itemKey(r, img, digest)
	if err := tx.Delete(nsLinks, k); err != nil {
		return err
	}
	return tx.Delete(nsRefs, refKey(digest, k, "b"))
}

// deleteImageData removes every manifest, tag and link under prefix (a repo
// key + "|" or an image prefix) and returns the digests that lost a
// reference.
func deleteImageData(tx store.Tx, prefix string) ([]string, error) {
	digests := map[string]bool{}
	for _, ns := range []string{nsManifests, nsTags, nsLinks} {
		var keys []string
		tx.Scan(ns, prefix, func(k string, _ []byte) bool { keys = append(keys, k); return true })
		for _, k := range keys {
			_, _, suffix := splitItemKey(k)
			switch ns {
			case nsManifests:
				digests[suffix] = true
				if err := tx.Delete(nsRefs, refKey(suffix, k, "m")); err != nil {
					return nil, err
				}
			case nsLinks:
				digests[suffix] = true
				if err := tx.Delete(nsRefs, refKey(suffix, k, "b")); err != nil {
					return nil, err
				}
			}
			if err := tx.Delete(ns, k); err != nil {
				return nil, err
			}
		}
	}
	out := make([]string, 0, len(digests))
	for d := range digests {
		out = append(out, d)
	}
	return out, nil
}

// imageEntry is one image (package) of a repository.
type imageEntry struct {
	Image            string
	Created, Updated time.Time
}

// listImages returns the image paths of a repository, sorted.
func listImages(tx store.Tx, r repoRef) []imageEntry {
	byImg := map[string]*imageEntry{}
	tx.Scan(nsManifests, r.key()+"|", func(k string, b []byte) bool {
		_, img, _ := splitItemKey(k)
		var m manifestRec
		if json.Unmarshal(b, &m) != nil {
			return true
		}
		e, ok := byImg[img]
		if !ok {
			e = &imageEntry{Image: img, Created: m.Created, Updated: m.Updated}
			byImg[img] = e
		}
		if m.Created.Before(e.Created) {
			e.Created = m.Created
		}
		if m.Updated.After(e.Updated) {
			e.Updated = m.Updated
		}
		return true
	})
	out := make([]imageEntry, 0, len(byImg))
	for _, e := range byImg {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Image < out[j].Image })
	return out
}

// listManifests returns the manifests of an image (all images when img is
// ""), in key order.
func listManifests(tx store.Tx, r repoRef, img string) ([]string, []*manifestRec) {
	pre := r.key() + "|"
	if img != "" {
		pre = imgPrefix(r, img)
	}
	var imgs []string
	var out []*manifestRec
	tx.Scan(nsManifests, pre, func(k string, b []byte) bool {
		var m manifestRec
		if json.Unmarshal(b, &m) == nil {
			_, i, _ := splitItemKey(k)
			imgs = append(imgs, i)
			out = append(out, &m)
		}
		return true
	})
	return imgs, out
}

// repoSize sums the bytes of every blob and manifest in a repository.
func repoSize(tx store.Tx, r repoRef) int64 {
	var n int64
	seen := map[string]bool{}
	tx.Scan(nsLinks, r.key()+"|", func(k string, b []byte) bool {
		_, _, d := splitItemKey(k)
		var l linkRec
		if !seen[d] && json.Unmarshal(b, &l) == nil {
			seen[d] = true
			n += l.Size
		}
		return true
	})
	_, ms := listManifests(tx, r, "")
	for _, m := range ms {
		if !seen[m.Digest] {
			seen[m.Digest] = true
			n += m.Size
		}
	}
	return n
}
