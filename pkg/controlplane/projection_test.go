package controlplane

import (
	"testing"
	"time"
)

// TestNewProjectionMarksStaleBeyondThreshold pins the one freshness bound every
// source type shares: only an observation strictly older than two minutes is
// stale (C-13).
func TestNewProjectionMarksStaleBeyondThreshold(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ref := ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "web"}
	object := map[string]any{"kind": "Application"}

	cases := []struct {
		name  string
		age   time.Duration
		stale bool
	}{
		{name: "read now", age: 0, stale: false},
		{name: "one nanosecond below the threshold", age: StaleThreshold - time.Nanosecond, stale: false},
		{name: "exactly at the threshold", age: StaleThreshold, stale: false},
		{name: "one nanosecond beyond the threshold", age: StaleThreshold + time.Nanosecond, stale: true},
		{name: "three minutes old", age: 3 * time.Minute, stale: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			observedAt := now.Add(-testCase.age)
			projection := newProjection(ref, object, observedAt, now)
			if projection.Stale != testCase.stale {
				t.Fatalf("stale = %v, want %v for an age of %s", projection.Stale, testCase.stale, testCase.age)
			}
			if !projection.ObservedAt.Equal(observedAt) {
				t.Fatalf("observedAt = %s, want %s", projection.ObservedAt, observedAt)
			}
			if projection.Ref != ref {
				t.Fatalf("ref = %v, want %v", projection.Ref, ref)
			}
		})
	}
}
