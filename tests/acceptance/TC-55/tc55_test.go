// Package tc55 verifies the retry budget of C-33: a retryable failure keeps the
// application Installing with an observable count and a bounded backoff, the
// fifth failure ends automatic retries, and a user retry resets the budget
// (AC-27, TC-55).
package tc55

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	targetNamespace = "apps"
	applicationName = "flaky"
)

// failingStore injects one retryable downstream failure. The lifecycle rules of
// C-33 are what the case measures; the failure itself is the fault.
type failingStore struct {
	app.Store
	failing bool
}

// Apply fails while the fault is armed.
func (s *failingStore) Apply(ctx context.Context, namespace string, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if s.failing {
		return nil, errors.New("the volume claim references a storage class the cluster does not have")
	}
	return s.Store.Apply(ctx, namespace, document)
}

func TestTC55RetryBudgetAndTermination(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	cluster.InstallDefinition(t, harness.Definition(t, "plain", nil))
	gateway := cluster.StartControlPlane(t, nil)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.ApplicationManifest(applicationName, harness.Namespace, "plain", targetNamespace, nil, nil), nil); status != http.StatusAccepted {
		t.Fatalf("create application: status %d, body %v", status, body)
	}

	clock := harness.NewClock(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	fault := &failingStore{Store: app.NewWriterStore(cluster.Writer()), failing: true}
	controller := app.NewController(fault, app.Options{Clock: clock.Now})

	reconcileOnce(t, controller)
	if phase := phaseOf(t, cluster); phase != app.PhaseInstalling {
		t.Fatalf("phase = %q, want %q before the first attempt", phase, app.PhaseInstalling)
	}
	if count := retryCount(t, cluster); count != 0 {
		t.Fatalf("retryCount = %d, want 0 before the first attempt", count)
	}

	for attempt := 1; attempt <= app.MaxAttempts; attempt++ {
		if attempt > 1 {
			advanceToNextRetry(t, cluster, clock)
		}
		reconcileOnce(t, controller)
		phase, count := phaseOf(t, cluster), retryCount(t, cluster)
		if count != int64(attempt) {
			t.Fatalf("after attempt %d retryCount = %d", attempt, count)
		}
		if attempt < app.MaxAttempts {
			if phase != app.PhaseInstalling {
				t.Fatalf("attempt %d: phase = %q, want %q", attempt, phase, app.PhaseInstalling)
			}
			next := nextRetryAt(t, cluster)
			interval := next.Sub(clock.Now())
			if interval <= 0 || interval > app.MaxBackoff {
				t.Fatalf("attempt %d: the next attempt is %v away, outside (0, %v]", attempt, interval, app.MaxBackoff)
			}
			if want := app.Backoff(attempt); interval != want {
				t.Fatalf("attempt %d: backoff = %v, want %v", attempt, interval, want)
			}
			if attempt == 1 {
				// A round inside the backoff window must not spend an attempt:
				// the schedule is what paces retries (C-33).
				scheduled := nextRetryAt(t, cluster)
				reconcileOnce(t, controller)
				if phase, count := phaseOf(t, cluster), retryCount(t, cluster); phase != app.PhaseInstalling || count != 1 {
					t.Fatalf("a round inside the window changed phase/count to %q/%d", phase, count)
				}
				if again := nextRetryAt(t, cluster); !again.Equal(scheduled) {
					t.Fatalf("a round inside the window moved nextRetryAt from %v to %v", scheduled, again)
				}
			}
			continue
		}
		if phase != app.PhaseFailed {
			t.Fatalf("attempt %d: phase = %q, want %q", attempt, phase, app.PhaseFailed)
		}
		if value, present := statusField(t, cluster, "nextRetryAt"); present {
			t.Fatalf("a failed application still schedules a retry at %v", value)
		}
	}

	// Waiting past the backoff cap must not restart anything.
	clock.Advance(app.MaxBackoff + time.Minute)
	reconcileOnce(t, controller)
	if phase, count := phaseOf(t, cluster), retryCount(t, cluster); phase != app.PhaseFailed || count != app.MaxAttempts {
		t.Fatalf("after the budget, phase = %q count = %d, want Failed/%d", phase, count, app.MaxAttempts)
	}

	status, response := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", map[string]any{
		"kind":      "Application",
		"namespace": harness.Namespace,
		"name":      applicationName,
		"action":    app.ActionRetry,
	}, nil)
	if status != http.StatusAccepted {
		t.Fatalf("retry action: status %d, body %v", status, response)
	}

	reconcileOnce(t, controller)
	if phase := phaseOf(t, cluster); phase != app.PhaseInstalling {
		t.Fatalf("after the user retry phase = %q, want %q", phase, app.PhaseInstalling)
	}
	if count := retryCount(t, cluster); count != 0 {
		t.Fatalf("after the user retry retryCount = %d, want 0", count)
	}
	next := nextRetryAt(t, cluster)
	if interval := next.Sub(clock.Now()); interval != app.InitialBackoff {
		t.Fatalf("the reset budget waits %v, want %v", interval, app.InitialBackoff)
	}

	records, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: app.ActionOwnerLabel,
	})
	if err != nil {
		t.Fatalf("list the action records: %v", err)
	}
	for _, record := range records.Items {
		if record.GetLabels()[app.ActionLabel] == app.ActionRetry {
			t.Fatalf("the consumed retry record %q is still there", record.GetName())
		}
	}
}

// reconcileOnce drives one round of the case's application.
func reconcileOnce(t *testing.T, controller *app.Controller) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: harness.Namespace, Name: applicationName,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// advanceToNextRetry moves the clock to the recorded retry instant.
func advanceToNextRetry(t *testing.T, cluster *harness.Cluster, clock *harness.Clock) {
	t.Helper()
	next := nextRetryAt(t, cluster)
	clock.Advance(next.Sub(clock.Now()))
}

// phaseOf reads the stored phase.
func phaseOf(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	phase, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "phase")
	if err != nil {
		t.Fatalf("read the phase: %v", err)
	}
	return phase
}

// retryCount reads the observed attempt count.
func retryCount(t *testing.T, cluster *harness.Cluster) int64 {
	t.Helper()
	count, _, err := unstructured.NestedInt64(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "retryCount")
	if err != nil {
		t.Fatalf("read retryCount: %v", err)
	}
	return count
}

// nextRetryAt reads the scheduled retry instant.
func nextRetryAt(t *testing.T, cluster *harness.Cluster) time.Time {
	t.Helper()
	text, present := statusField(t, cluster, "nextRetryAt")
	if !present {
		t.Fatal("nextRetryAt is not observable")
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("parse nextRetryAt %q: %v", text, err)
	}
	return parsed
}

// statusField reads one string field of the stored status.
func statusField(t *testing.T, cluster *harness.Cluster, field string) (string, bool) {
	t.Helper()
	status, found, err := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status")
	if err != nil {
		t.Fatalf("read the status: %v", err)
	}
	if !found {
		return "", false
	}
	text, isText := status[field].(string)
	return text, isText
}
