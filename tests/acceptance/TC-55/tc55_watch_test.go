// This file covers the production enqueue path of C-33: an application that is
// Failed schedules no further round, so the retry a user submits is reachable
// only through the action-record watch (AC-27, TC-55).
package tc55

import (
	"context"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC55RetryReachesAFailedApplicationThroughTheWatch creates a Failed
// application, lets the watch drive its first round, and then submits the retry
// the user would submit. Nothing schedules another round after the first one, so
// only the action-record watch can deliver the retry; the case requires the
// application to leave Failed and the record to be consumed.
func TestTC55RetryReachesAFailedApplicationThroughTheWatch(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, harness.Namespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	gateway := cluster.StartControlPlane(t, nil)

	created := cluster.Create(t, &unstructured.Unstructured{
		Object: harness.ApplicationManifest(applicationName, harness.Namespace, "plain", targetNamespace, nil, nil),
	})
	uid := string(created.GetUID())
	// The Failed state is a precondition, written through the same status path
	// the reconciler uses so the case does not take the field from it (C-03).
	statusDocument := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": applicationName, "namespace": harness.Namespace},
		"status":     map[string]any{"phase": app.PhaseFailed},
	}}
	if _, err := cluster.Writer().Apply(context.Background(), write.SubjectApplicationReconciler, write.Session{}, write.PartStatus, statusDocument); err != nil {
		t.Fatalf("mark the application Failed: %v", err)
	}

	controller := cluster.ApplicationController(t, app.Options{})
	runtime, err := app.NewRuntime(controller, cluster.Dynamic, app.RuntimeOptions{})
	if err != nil {
		t.Fatalf("wire the application runtime: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// The first round comes from the Application watch alone, and it ends on a
	// Failed phase without scheduling another one: the uninstall gate it wrote is
	// what the case can observe of that round (C-33).
	harness.Eventually(t, 15*time.Second, "the watch to run the first round of the application", func() bool {
		for _, finalizer := range cluster.Get(t, "v1beta1", "Application", applicationName).GetFinalizers() {
			if finalizer == app.Finalizer {
				return true
			}
		}
		return false
	})
	if phase, present := statusField(t, cluster, "phase"); !present || phase != app.PhaseFailed {
		t.Fatalf("phase = %q (present %v), want the stored %q", phase, present, app.PhaseFailed)
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

	// A Failed application has no scheduled round, so the phase can only move
	// because the record watch enqueued the application and its round consumed
	// the record (C-33, C-69).
	harness.Eventually(t, 5*time.Second, "the record watch to deliver the retry", func() bool {
		phase, present := statusField(t, cluster, "phase")
		if !present || phase != app.PhaseInstalling {
			return false
		}
		return !actionRecordPresent(t, cluster, app.ActionRecordName(uid, app.ActionRetry))
	})
	if count := retryCount(t, cluster); count != 0 {
		t.Fatalf("retryCount = %d after the user retry, want 0", count)
	}
}

// actionRecordPresent reports whether the record of one action is still stored.
func actionRecordPresent(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	_, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err == nil {
		return true
	}
	if apierrors.IsNotFound(err) {
		return false
	}
	t.Fatalf("read the action record %q: %v", name, err)
	return false
}
