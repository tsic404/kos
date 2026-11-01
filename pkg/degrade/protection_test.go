package degrade

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/budget"
)

// recorder is a Listener that counts its switches.
type recorder struct {
	suspends []string
	resumes  int
}

func (r *recorder) Suspend(reason string) { r.suspends = append(r.suspends, reason) }
func (r *recorder) Resume()               { r.resumes++ }

const mib = int64(1 << 20)

func TestProtectionTriggersOnceAndRecoversOnce(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	listener := &recorder{}
	protection := NewProtection(ProtectionConfig{
		ThresholdBytes: 100 * mib,
		Now:            func() time.Time { return now },
		Listener:       listener,
	})

	// Six minutes above the threshold: the 5-minute sustain ends the trigger
	// once, and the following samples do not add a second one.
	for minute := range 7 {
		now = now.Add(time.Minute)
		protection.Observe(200 * mib)
		if minute >= 5 && len(listener.suspends) != 1 {
			t.Fatalf("after %d minutes suspends = %d, want one", minute+1, len(listener.suspends))
		}
	}
	if len(listener.suspends) != 1 {
		t.Fatalf("suspends = %d, want one", len(listener.suspends))
	}
	status := protection.Status()
	if !status.IsDegraded || status.Reason != ReasonMemoryExceeded || status.Since.IsZero() {
		t.Fatalf("status = %+v, want a degraded state with a reason", status)
	}

	// The percentile window must clear before the ten-minute sustained period
	// starts: sixteen minutes below two thirds of the threshold recover once.
	for range 16 {
		now = now.Add(time.Minute)
		protection.Observe(50 * mib)
	}
	if listener.resumes != 1 {
		t.Fatalf("resumes = %d, want one", listener.resumes)
	}
	if status := protection.Status(); status.IsDegraded || status.Reason != "" {
		t.Fatalf("status = %+v after recovery, want a clean state", status)
	}

	// Staying below the recovery fraction never triggers again.
	for range 30 {
		now = now.Add(time.Minute)
		protection.Observe(50 * mib)
	}
	if listener.resumes != 1 || len(listener.suspends) != 1 {
		t.Fatalf("suspends=%d resumes=%d after a quiet half hour, want one each", len(listener.suspends), listener.resumes)
	}
}

func TestProtectionDoesNotRecoverBetweenTheFractions(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	listener := &recorder{}
	protection := NewProtection(ProtectionConfig{
		ThresholdBytes: 300 * mib,
		Now:            func() time.Time { return now },
		Listener:       listener,
	})
	for range 6 {
		now = now.Add(time.Minute)
		protection.Observe(400 * mib)
	}
	if len(listener.suspends) != 1 {
		t.Fatalf("suspends = %d, want one", len(listener.suspends))
	}
	// 250 MiB is below the threshold but above its two thirds (200 MiB): the
	// hysteresis keeps the degradation, so a measurement hovering there cannot
	// flap.
	for range 30 {
		now = now.Add(time.Minute)
		protection.Observe(250 * mib)
	}
	if listener.resumes != 0 {
		t.Fatalf("resumes = %d between the fractions, want none", listener.resumes)
	}
	if !protection.Status().IsDegraded {
		t.Fatal("the protection recovered between the fractions")
	}
}

func TestProtectionUnmeasurableDoesNotJudge(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	listener := &recorder{}
	protection := NewProtection(ProtectionConfig{
		ThresholdBytes: 100 * mib,
		Now:            func() time.Time { return now },
		Listener:       listener,
	})
	protection.MarkUnmeasurable("reader failed")
	for range 20 {
		now = now.Add(time.Minute)
	}
	if status := protection.Status(); status.IsMeasurable || status.IsDegraded || status.UnmeasurableReason != "reader failed" {
		t.Fatalf("status = %+v, want unmeasurable and not degraded", status)
	}
	if len(listener.suspends) != 0 {
		t.Fatalf("a missing measurement triggered %d suspends", len(listener.suspends))
	}

	// A failed reader through Sample keeps the same verdict.
	protection.Sample(context.Background(), failingReader{})
	if status := protection.Status(); status.IsMeasurable {
		t.Fatalf("status = %+v after a failed read, want unmeasurable", status)
	}
}

func TestProtectionStaleMeasurementIsUnmeasurable(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	protection := NewProtection(ProtectionConfig{
		ThresholdBytes: 100 * mib,
		Now:            func() time.Time { return now },
	})
	protection.Observe(10 * mib)
	if !protection.Status().IsMeasurable {
		t.Fatal("a fresh measurement is measurable")
	}
	now = now.Add(6 * time.Minute)
	status := protection.Status()
	if status.IsMeasurable || status.UnmeasurableReason != ReasonStaleReading {
		t.Fatalf("status = %+v, want the stale reading reported", status)
	}
}

func TestProtectionIgnoresTheInstallWindow(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	listener := &recorder{}
	installFrom := now.Add(time.Minute)
	installTo := installFrom.Add(20 * time.Minute)
	protection := NewProtection(ProtectionConfig{
		ThresholdBytes: 100 * mib,
		Now:            func() time.Time { return now },
		Listener:       listener,
		IsExcluded: func(at time.Time) bool {
			return !at.Before(installFrom) && at.Before(installTo)
		},
	})
	for range 20 {
		now = now.Add(time.Minute)
		protection.Observe(500 * mib)
	}
	if len(listener.suspends) != 0 {
		t.Fatalf("a spike confined to the install window triggered %d suspends", len(listener.suspends))
	}
	// The window only excludes the judgement; the process is still measured,
	// so the health endpoint must not report it unmeasurable (C-57).
	if status := protection.Status(); !status.IsMeasurable {
		t.Fatalf("status = %+v after an install window, want a measurable process", status)
	}
}

func TestProtectionDefaultThresholdFollowsTheSteadyBudget(t *testing.T) {
	protection := NewProtection(ProtectionConfig{})
	want := int64(float64(budget.SteadyMemoryBytes) * ThresholdFactor)
	if got := protection.Status().ThresholdBytes; got != want {
		t.Fatalf("default threshold = %d, want %d (1.5 times the steady budget)", got, want)
	}
}

func TestProtectionRecoversAfterThresholdRaised(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	listener := &recorder{}
	protection := NewProtection(ProtectionConfig{
		ThresholdBytes: 64 * mib,
		Now:            func() time.Time { return now },
		Listener:       listener,
	})
	for range 6 {
		now = now.Add(time.Minute)
		protection.Observe(100 * mib)
	}
	if len(listener.suspends) != 1 {
		t.Fatalf("suspends = %d with a threshold below usage, want one", len(listener.suspends))
	}
	// Raising the threshold above twice the usage brings the measurement below
	// its two thirds; the recovery sustain is still ten minutes.
	protection.SetThreshold(300 * mib)
	for minute := range 11 {
		now = now.Add(time.Minute)
		protection.Observe(100 * mib)
		if listener.resumes != 0 && minute < 10 {
			t.Fatalf("recovered after %d minutes, want ten", minute+1)
		}
	}
	if listener.resumes != 1 {
		t.Fatalf("resumes = %d after the sustain, want one", listener.resumes)
	}
}

// failingReader always fails.
type failingReader struct{}

func (failingReader) ReadMemory(context.Context) (int64, error) {
	return 0, errors.New("no memory counter")
}
