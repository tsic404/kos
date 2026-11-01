// Package tc99 verifies the unmeasurable arm of AC-50 and C-57: when the
// control plane cannot read its own memory the health endpoint reports
// unavailable with a reason, no degradation is triggered, and the diagnostic
// bundle carries the same record.
package tc99

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/degrade"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC99DegradationProtectionUnmeasurable(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	clock := harness.NewClock(now)
	listener := &listener{}
	protection := degrade.NewProtection(degrade.ProtectionConfig{
		ThresholdBytes: 64 << 20,
		Now:            clock.Now,
		Listener:       listener,
	})
	if status := protection.Sample(context.Background(), failingReader{}); status.IsMeasurable {
		t.Fatalf("status = %+v after a failed read, want unmeasurable", status)
	}

	cluster := harness.Start(t)
	world := harness.StartDiagnosticWorld(t, cluster)
	service := cluster.DiagnosticService(world.Installation)
	service.Protection = protection
	api := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Diagnostics = service
		config.Protection = protection
	})

	// The health endpoint reports unavailable and the reason, and no
	// degradation was triggered (C-57).
	status, body := api.Call(t, http.MethodGet, "/api/v1/system/health", nil, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("health status = %d, want 503: %v", status, body)
	}
	if got := harness.String(body, "status"); got != "unavailable" {
		t.Fatalf("health status = %q, want unavailable", got)
	}
	if reason := harness.String(body, "protection", "unmeasurableReason"); !strings.Contains(reason, "no memory counter") {
		t.Fatalf("unmeasurable reason = %q", reason)
	}
	if degraded, _ := body["protection"].(map[string]any)["degraded"].(bool); degraded {
		t.Fatal("an unmeasurable protection reported a degradation")
	}
	if listener.suspends != 0 {
		t.Fatalf("an unmeasurable protection suspended real-time listening %d times", listener.suspends)
	}

	// The diagnostic bundle carries the same record (C-56, C-57).
	status, bundle, err := api.Raw(http.MethodGet, "/api/v1/diagnostics/"+world.Namespace+"/"+world.Application+"/bundle", nil, nil)
	if err != nil {
		t.Fatalf("export the bundle: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("export the bundle: %d %s", status, bundle)
	}
	document := map[string]any{}
	if err = json.Unmarshal(bundle, &document); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	record, found := document["degradation"].(map[string]any)
	if !found {
		t.Fatalf("the bundle carries no degradation record: %v", document)
	}
	if measurable, _ := record["measurable"].(bool); measurable {
		t.Fatalf("bundle degradation = %v, want unmeasurable", record)
	}
	if degraded, _ := record["degraded"].(bool); degraded {
		t.Fatalf("bundle degradation = %v, want no degradation", record)
	}
	if reason, _ := record["unmeasurableReason"].(string); !strings.Contains(reason, "no memory counter") {
		t.Fatalf("bundle degradation reason = %q", reason)
	}
}

// failingReader reports no memory counter.
type failingReader struct{}

func (failingReader) ReadMemory(context.Context) (int64, error) {
	return 0, errors.New("no memory counter")
}

// listener records what the protection switched.
type listener struct {
	suspends int
}

func (l *listener) Suspend(string) { l.suspends++ }
func (l *listener) Resume()        {}
