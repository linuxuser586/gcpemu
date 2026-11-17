package clock

import (
	"testing"
	"time"
)

var fakeEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestFakeMovesOnlyOnRequestSteps is ADR 0020 (#441): reads of the fake
// clock never move it; only a request step does, by exactly 1 ms, so
// background readers cannot make request timestamps depend on wall time.
func TestFakeMovesOnlyOnRequestSteps(t *testing.T) {
	f := NewFake()
	for range 3 {
		if got := f.Now(); !got.Equal(fakeEpoch) {
			t.Fatalf("Now = %v, want %v", got, fakeEpoch)
		}
	}
	RequestStep(f)
	want := fakeEpoch.Add(time.Millisecond)
	for range 3 {
		if got := f.Now(); !got.Equal(want) {
			t.Fatalf("Now after one step = %v, want %v", got, want)
		}
	}
	RequestStep(f)
	if got := f.Now(); !got.Equal(fakeEpoch.Add(2 * time.Millisecond)) {
		t.Fatalf("Now after two steps = %v, want %v", got, fakeEpoch.Add(2*time.Millisecond))
	}
}

// A request step reaches the fake clock through an Offset, and the offset
// that `gcpemu time advance` adds is kept.
func TestRequestStepThroughOffset(t *testing.T) {
	o := NewOffset(NewFake())
	o.Advance(time.Hour)
	want := fakeEpoch.Add(time.Hour)
	if got := o.Now(); !got.Equal(want) {
		t.Fatalf("Now = %v, want %v", got, want)
	}
	if got := o.Now(); !got.Equal(want) {
		t.Fatalf("second Now = %v, want %v", got, want)
	}
	RequestStep(o)
	if got := o.Now(); !got.Equal(want.Add(time.Millisecond)) {
		t.Fatalf("Now after step = %v, want %v", got, want.Add(time.Millisecond))
	}
}

// On the real clock a request step is a no-op.
func TestRequestStepRealIsNoop(t *testing.T) {
	o := NewOffset(Real{})
	before := time.Now()
	RequestStep(o)
	got := o.Now()
	if got.Before(before.Add(-time.Second)) || got.After(time.Now().Add(time.Second)) {
		t.Fatalf("Now = %v, want about %v", got, before)
	}
}

// TestOffsetReadsAndResets is #1172: Read reports the time and what has
// been added, and Reset sets the offset back to zero, so the clock reads
// its base again.
func TestOffsetReadsAndResets(t *testing.T) {
	o := NewOffset(NewFake())
	if now, off := o.Read(); off != 0 || !now.Equal(fakeEpoch) {
		t.Fatalf("Read = %v, %v, want %v, 0", now, off, fakeEpoch)
	}
	o.Advance(2 * time.Hour)
	if now, off := o.Read(); off != 2*time.Hour || !now.Equal(fakeEpoch.Add(2*time.Hour)) {
		t.Fatalf("Read = %v, %v, want %v, 2h", now, off, fakeEpoch.Add(2*time.Hour))
	}
	if got := o.Reset(); !got.Equal(fakeEpoch) {
		t.Fatalf("Reset = %v, want the base %v", got, fakeEpoch)
	}
	if now, off := o.Read(); off != 0 || !now.Equal(fakeEpoch) {
		t.Fatalf("Read after Reset = %v, %v, want %v, 0", now, off, fakeEpoch)
	}
}
