package clock

import (
	"testing"
	"time"
)

var fakeEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestPeekDoesNotAdvanceFake is NFR-REL-002 (#436): background reads under
// --deterministic must not move the fake clock, or later request timestamps
// depend on how much wall time passed.
func TestPeekDoesNotAdvanceFake(t *testing.T) {
	f := NewFake()
	if got := Peek(f); !got.Equal(fakeEpoch) {
		t.Fatalf("Peek = %v, want %v", got, fakeEpoch)
	}
	if got := Peek(f); !got.Equal(fakeEpoch) {
		t.Fatalf("second Peek = %v, want %v", got, fakeEpoch)
	}
	if got := f.Now(); !got.Equal(fakeEpoch) {
		t.Fatalf("Now after Peek = %v, want %v", got, fakeEpoch)
	}
	if got := f.Now(); !got.Equal(fakeEpoch.Add(time.Millisecond)) {
		t.Fatalf("second Now = %v, want %v", got, fakeEpoch.Add(time.Millisecond))
	}
	if got := Peek(f); !got.Equal(fakeEpoch.Add(2 * time.Millisecond)) {
		t.Fatalf("Peek after two Nows = %v, want %v", got, fakeEpoch.Add(2*time.Millisecond))
	}
}

func TestPeekThroughOffset(t *testing.T) {
	o := NewOffset(NewFake())
	o.Advance(time.Hour)
	want := fakeEpoch.Add(time.Hour)
	if got := Peek(o); !got.Equal(want) {
		t.Fatalf("Peek = %v, want %v", got, want)
	}
	if got := o.Now(); !got.Equal(want) {
		t.Fatalf("Now after Peek = %v, want %v", got, want)
	}
	if got := Peek(o); !got.Equal(want.Add(time.Millisecond)) {
		t.Fatalf("Peek after Now = %v, want %v", got, want.Add(time.Millisecond))
	}
}

func TestPeekRealIsNow(t *testing.T) {
	before := time.Now()
	got := Peek(NewOffset(Real{}))
	if got.Before(before.Add(-time.Second)) || got.After(time.Now().Add(time.Second)) {
		t.Fatalf("Peek = %v, want about %v", got, before)
	}
}
