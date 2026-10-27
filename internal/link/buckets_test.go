package link_test

import (
	"testing"

	"github.com/linuxuser586/gcpemu/internal/link"
)

// A reply the watch stream carries may arrive after a later refresh's: the
// front Instance keeps the later one, so a bucket the stream had not yet
// heard of stays routed. A Linked Instance that numbers no reply is
// always heard.
func TestStaleBucketsIgnored(t *testing.T) {
	m := link.New(link.Config{})
	m.HearBuckets("office", map[string]string{"office-made": "shared"}, 2)
	m.HearBuckets("office", map[string]string{}, 1)
	if _, _, ok := m.BucketOwner("office-made"); !ok {
		t.Error("an earlier reply without office-made, heard last, forgot it")
	}
	m.HearBuckets("office", map[string]string{}, 0)
	if _, _, ok := m.BucketOwner("office-made"); ok {
		t.Error("an unnumbered reply without office-made did not forget it")
	}
}
