// Package tc19 verifies that a marker key takes effect only on its own carrier:
// the label form decides the attribution, the annotation form never does, and
// the projection agrees with what a label selector returns (AC-8, TC-19).
package tc19

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// requestTimeout bounds every API call so a wedged server fails the case.
const requestTimeout = 30 * time.Second

// TestTC19MarkerCarrier writes the same attribution key as an annotation and as
// a label and compares the derived attribution with label selection.
func TestTC19MarkerCarrier(t *testing.T) {
	cluster := harness.Start(t)
	writer := cluster.Writer()
	session := write.Session{Username: "alice", Role: write.RoleOwner}
	ctx := context.Background()

	// The same key, written as an annotation, changes nothing.
	annotated := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":        "annotated",
			"namespace":   harness.Namespace,
			"annotations": map[string]any{markers.ManagedBy: "kos-shell"},
		},
		"spec": map[string]any{},
	}}
	cluster.Create(t, annotated)

	stored := cluster.Get(t, "v1beta1", "Application", "annotated")
	if got := attribution(t, stored); got != markers.AttributionUnmanaged {
		t.Fatalf("annotation-carried marker changed the attribution to %s", got)
	}
	if selected := cluster.ListByLabels(t, "v1beta1", "Application", markers.ManagedBy+"=kos-shell"); len(selected) != 0 {
		t.Fatalf("label selection returned %d objects for an annotation-only marker", len(selected))
	}

	// The write path refuses to put a marker on the wrong carrier.
	_, err := writer.Apply(ctx, write.SubjectUser, session, write.PartSpec, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":        "annotated",
			"namespace":   harness.Namespace,
			"annotations": map[string]any{markers.ManagedBy: "kos-shell"},
		},
	}})
	var denied *write.Denied
	if !errors.As(err, &denied) {
		t.Fatalf("a marker on the wrong carrier was accepted: %v", err)
	}
	if !strings.Contains(denied.Error(), markers.ManagedBy) {
		t.Fatalf("refusal does not name the marker key: %v", denied)
	}

	// Adding the key as a label makes the same object managed.
	labels := stored.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[markers.ManagedBy] = "kos-shell"
	stored.SetLabels(labels)
	updated, err := cluster.Direct(t, api.Group, "v1beta1", "Application", harness.Namespace).Update(ctx, stored, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("write the label form: %v", err)
	}
	if got := attribution(t, updated); got != markers.AttributionManaged {
		t.Fatalf("label-carried marker gave attribution %s, want managed", got)
	}
	if selected := cluster.ListByLabels(t, "v1beta1", "Application", markers.ManagedBy+"=kos-shell"); len(selected) != 1 {
		t.Fatalf("label selection returned %d objects, want 1", len(selected))
	}

	// The adoption annotation is required for the adopted state.
	adopted := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      "adopted",
			"namespace": harness.Namespace,
			"labels": map[string]any{
				markers.AdoptedBy:      "alice",
				markers.AdoptionReason: "existing",
			},
		},
		"spec": map[string]any{},
	}}
	cluster.Create(t, adopted)
	incomplete := cluster.Get(t, "v1beta1", "Application", "adopted")
	if err := markers.Validate(incomplete.GetLabels(), incomplete.GetAnnotations()); err == nil {
		t.Fatal("an adopted object without adopted-at validated")
	}
	annotations := incomplete.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[markers.AdoptedAt] = "2026-10-25T00:00:00Z"
	incomplete.SetAnnotations(annotations)
	complete, err := cluster.Direct(t, api.Group, "v1beta1", "Application", harness.Namespace).Update(ctx, incomplete, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("annotate adopted-at: %v", err)
	}
	if err := markers.Validate(complete.GetLabels(), complete.GetAnnotations()); err != nil {
		t.Fatalf("an adopted object with adopted-at did not validate: %v", err)
	}

	// Every object reads the same state through the projection and through a
	// label selector.
	for _, object := range cluster.ListByLabels(t, "v1beta1", "Application", "") {
		assertProjectionMatchesSelection(t, cluster, &object)
	}
}

// assertProjectionMatchesSelection compares the derived attribution with the
// selectors a projection would use.
func assertProjectionMatchesSelection(t *testing.T, cluster *harness.Cluster, object *unstructured.Unstructured) {
	t.Helper()
	state := attribution(t, object)
	managed := len(cluster.ListByLabels(t, "v1beta1", "Application", fmt.Sprintf("%s=kos-shell", markers.ManagedBy)))
	adopted := len(cluster.ListByLabels(t, "v1beta1", "Application", fmt.Sprintf("%s=alice", markers.AdoptedBy)))
	switch state {
	case markers.AttributionManaged:
		if managed == 0 {
			t.Fatalf("%s is managed but no managed-by selector returns it", object.GetName())
		}
	case markers.AttributionAdopted:
		if adopted == 0 {
			t.Fatalf("%s is adopted but no adopted-by selector returns it", object.GetName())
		}
	case markers.AttributionUnmanaged:
		if object.GetLabels()[markers.ManagedBy] != "" || object.GetLabels()[markers.AdoptedBy] != "" {
			t.Fatalf("%s is unmanaged yet carries an attribution label", object.GetName())
		}
	}
}

// attribution derives the state the projection shows.
func attribution(t *testing.T, object *unstructured.Unstructured) markers.Attribution {
	t.Helper()
	state, err := markers.AttributionOf(object.GetLabels())
	if err != nil {
		t.Fatalf("attribution of %s: %v", object.GetName(), err)
	}
	return state
}
