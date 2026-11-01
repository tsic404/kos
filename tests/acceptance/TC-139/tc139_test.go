// Package tc139 verifies that the one-shot records of an application that
// disappeared without an uninstall round are swept, that the abandonment
// manifest of an orphaned application stays, and that a record whose
// application is still there is left to its own round (AC-27, TC-139).
package tc139

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	// sweepInterval keeps the case short: the criterion is that the sweep
	// happens, not how often it runs.
	sweepInterval = 100 * time.Millisecond
	// wait bounds every observation of the case.
	wait = 15 * time.Second
)

// TestTC139SweepsRecordsOfGoneApplications covers C-69: the records of an
// application that is gone are deleted, a record of a live application is not
// touched by the sweep, and the abandonment manifest of an orphaned application
// survives after the application object is gone.
func TestTC139SweepsRecordsOfGoneApplications(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, harness.Namespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)

	// An application that disappears while no controller is running leaves its
	// record behind: the uninstall round never ran, so nothing consumed it.
	gone := cluster.Create(t, harness.Application("gone", nil))
	goneUID := string(gone.GetUID())
	goneRecord := app.ActionRecordName(goneUID, app.ActionRetry)
	writeRecord(t, cluster, goneRecord, goneUID, harness.Namespace+"/gone")
	deleteApplication(t, cluster, "gone")

	controller := cluster.ApplicationController(t, app.Options{})
	runtime, err := app.NewRuntime(controller, cluster.Dynamic, app.RuntimeOptions{Sweep: sweepInterval})
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

	// The first sweep runs before the first round, so a restart does not leave
	// the records of the downtime for a whole interval.
	harness.Eventually(t, wait, "the startup sweep to delete the record of the deleted application", func() bool {
		return !recordPresent(t, cluster, goneRecord)
	})

	// A record that arrives while the controller runs is swept by the periodic
	// pass: its application never existed, so no round can consume it.
	ghostRecord := app.ActionRecordName("ghost-uid", app.ActionRetry)
	writeRecord(t, cluster, ghostRecord, "ghost-uid", harness.Namespace+"/ghost")
	harness.Eventually(t, wait, "the periodic sweep to delete the record of an absent application", func() bool {
		return !recordPresent(t, cluster, ghostRecord)
	})

	// The abandonment manifest of an orphaned application is not an action and
	// stays after its application is gone (C-69). It shares neither the action
	// label nor the sweep's judgement, so the sweep leaves it alone.
	manifest := writeManifest(t, cluster, "ghost-uid")
	time.Sleep(5 * sweepInterval)
	stored := readManifest(t, cluster, manifest.GetName())
	if stored.GetLabels()[write.AbandonmentLabel] != "ghost-uid" {
		t.Fatalf("the abandonment manifest lost its label: %v", stored.GetLabels())
	}
	before, _, _ := unstructured.NestedString(manifest.Object, "data", app.AbandonmentKey)
	after, _, _ := unstructured.NestedString(stored.Object, "data", app.AbandonmentKey)
	if after != before {
		t.Fatalf("the abandonment manifest changed from %q to %q", before, after)
	}
}

// writeRecord stores one one-shot action record the way the control plane
// writes it (C-04, C-69).
func writeRecord(t *testing.T, cluster *harness.Cluster, name, owner, target string) {
	t.Helper()
	record := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": api.SystemNamespace,
			"labels": map[string]any{
				markers.ManagedBy:    write.ManagedByValue,
				app.ActionLabel:      app.ActionRetry,
				app.ActionOwnerLabel: owner,
			},
		},
		"data": map[string]any{app.ActionDataKey: target},
	}}
	if _, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).Create(
		context.Background(), record, metav1.CreateOptions{}); err != nil {
		t.Fatalf("write the action record %q: %v", name, err)
	}
}

// recordPresent reports whether one action record is still stored.
func recordPresent(t *testing.T, cluster *harness.Cluster, name string) bool {
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

// deleteApplication removes an application without the uninstall round that
// consumes its records: the object carries no uninstall gate yet, so the API
// server removes it at once (C-33, C-69).
func deleteApplication(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	if err := cluster.Direct(t, api.Group, "v1beta1", "Application", harness.Namespace).Delete(
		context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete application %q: %v", name, err)
	}
}

// writeManifest stores the immutable abandonment manifest of one orphaned
// application, as the reconciler writes it (C-69).
func writeManifest(t *testing.T, cluster *harness.Cluster, uid string) *unstructured.Unstructured {
	t.Helper()
	entries, err := app.EncodeAbandonment([]app.Abandonment{{
		Object: v1beta1.ObjectRef{
			Group: api.Group, Version: "v1beta1", Kind: "Volume",
			Namespace: harness.Namespace, Name: "data",
		},
		Disposition: app.DispositionRetained,
	}})
	if err != nil {
		t.Fatalf("encode the abandonment manifest: %v", err)
	}
	manifest := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"immutable":  true,
		"metadata": map[string]any{
			"name":      app.AbandonmentName(uid),
			"namespace": api.SystemNamespace,
			"labels": map[string]any{
				markers.ManagedBy:      write.ManagedByValue,
				write.AbandonmentLabel: uid,
			},
		},
		"data": map[string]any{app.AbandonmentKey: entries},
	}}
	created, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).Create(
		context.Background(), manifest, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("write the abandonment manifest %q: %v", manifest.GetName(), err)
	}
	return created
}

// readManifest reads one stored abandonment manifest.
func readManifest(t *testing.T, cluster *harness.Cluster, name string) *unstructured.Unstructured {
	t.Helper()
	stored, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).Get(
		context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the abandonment manifest %q: %v", name, err)
	}
	return stored
}
