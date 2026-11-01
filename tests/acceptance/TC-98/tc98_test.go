// Package tc98 verifies the degradation protection of AC-50 and C-57: a
// memory percentile above the threshold for five minutes turns native-view
// real-time listening off once, the reason stays readable, and dropping below
// two thirds of the threshold for ten minutes recovers once, without flapping.
package tc98

import (
	"net/http"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/degrade"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const mib = int64(1 << 20)

func TestTC98DegradationTriggerAndRecovery(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	clock := harness.NewClock(now)
	listener := &listener{}
	protection := degrade.NewProtection(degrade.ProtectionConfig{
		ThresholdBytes: 64 * mib,
		Now:            clock.Now,
		Listener:       listener,
	})
	cluster := harness.Start(t)
	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Protection = protection
	})

	// Five minutes above the threshold degrade once: the native view's
	// real-time listening is suspended with a readable reason.
	for range 6 {
		clock.Advance(time.Minute)
		protection.Observe(100 * mib)
	}
	if listener.suspends != 1 {
		t.Fatalf("suspends = %d after the sustain, want one", listener.suspends)
	}
	if reason := listener.reason; reason == "" {
		t.Fatal("the suspension carries no reason")
	}
	status, body := gateway.Call(t, http.MethodGet, "/api/v1/system/health", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("health status = %d while degraded: %v", status, body)
	}
	if got := harness.String(body, "status"); got != "degraded" {
		t.Fatalf("health status = %q, want degraded", got)
	}
	if degraded, _ := body["protection"].(map[string]any)["degraded"].(bool); !degraded {
		t.Fatalf("health protection = %v, want degraded", body["protection"])
	}
	if reason := harness.String(body, "protection", "reason"); reason == "" {
		t.Fatal("the health report carries no degradation reason")
	}

	// Raising the threshold above twice the usage lets the measurement fall
	// below its two thirds; ten sustained minutes recover once.
	protection.SetThreshold(300 * mib)
	for range 16 {
		clock.Advance(time.Minute)
		protection.Observe(100 * mib)
	}
	if listener.resumes != 1 {
		t.Fatalf("resumes = %d after the sustain, want one", listener.resumes)
	}
	status, body = gateway.Call(t, http.MethodGet, "/api/v1/system/health", nil, nil)
	if status != http.StatusOK || harness.String(body, "status") != "ok" {
		t.Fatalf("health after recovery = %d %v, want ok", status, body)
	}

	// No flapping: a further quiet stretch changes nothing.
	for range 30 {
		clock.Advance(time.Minute)
		protection.Observe(100 * mib)
	}
	if listener.suspends != 1 || listener.resumes != 1 {
		t.Fatalf("suspends=%d resumes=%d after a quiet half hour, want one each", listener.suspends, listener.resumes)
	}
}

// listener records what the protection switched (C-57).
type listener struct {
	suspends int
	resumes  int
	reason   string
}

func (l *listener) Suspend(reason string) {
	l.suspends++
	l.reason = reason
}

func (l *listener) Resume() {
	l.resumes++
}
