// Package tc75 verifies that a duplicate key created around the API is detected
// by reconciliation, listed as a conflict on every holder, and never left as a
// silent coexistence (AC-37, TC-75; C-44).
package tc75

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC75DuplicatesCreatedAroundTheAPIAreConflicts(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))

	// The control plane is not involved: both objects are written the way
	// kubectl would, so only reconciliation can see the duplicate.
	for _, name := range []string{"first", "second"} {
		cluster.Create(t, harness.Endpoint(name, harness.ManagedLabels(), map[string]any{
			"kind":      "http.path",
			"targetApp": "web",
			"host":      "dup.test",
			"path":      "/same",
		}))
	}
	reconciler := endpoint.NewReconciler(cluster.Writer(), harness.Declarations{Capable: true, Safe: []string{"web"}})
	// One event settles both holders: the round on "second" records the entry,
	// finds "first" as its peer and rewrites that status too, so the case never
	// replays a round by hand (C-06).
	harness.ReconcileEndpoint(t, reconciler, "second")
	first := harness.RequireCondition(t, cluster, "Endpoint", "first", endpoint.ConditionConflict)
	if !strings.Contains(first.Message, "second") {
		t.Fatalf("the peer was not settled by the same round: %q", first.Message)
	}

	for name, other := range map[string]string{"first": "second", "second": "first"} {
		conflict := harness.RequireCondition(t, cluster, "Endpoint", name, endpoint.ConditionConflict)
		if conflict.Status != metav1.ConditionTrue {
			t.Fatalf("%s %s = %s, want True", name, endpoint.ConditionConflict, conflict.Status)
		}
		if !strings.Contains(conflict.Message, "dup.test/same") {
			t.Fatalf("%s conflict %q does not name the shared key", name, conflict.Message)
		}
		if !strings.Contains(conflict.Message, other) {
			t.Fatalf("%s conflict %q does not list %s", name, conflict.Message, other)
		}
		harness.RequireReconciled(t, cluster, "Endpoint", name, metav1.ConditionFalse)
	}

	rows := reconciler.Ledger().Projection()
	if len(rows) != 2 {
		t.Fatalf("projection = %v, want both holders", rows)
	}
	for _, row := range rows {
		if len(row.Conflicts) != 1 || row.Conflicts[0].Kind != endpoint.ConflictDuplicateKey {
			t.Fatalf("%s projection conflicts = %v", row.Ref.Name, row.Conflicts)
		}
		if len(row.Conflicts[0].With) != 1 {
			t.Fatalf("%s names %v as conflicting, want the other holder", row.Ref.Name, row.Conflicts[0].With)
		}
	}
}

// TestTC75ConflictsReachTheEarlierHolder drives the real event order: the first
// holder is admitted before the duplicate exists, and one round on the newcomer
// must mark both. Deleting the holder then clears the survivor in the round
// that observes the deletion, with no replay (C-06).
func TestTC75ConflictsReachTheEarlierHolder(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))
	reconciler := endpoint.NewReconciler(cluster.Writer(), harness.Declarations{Capable: true, Safe: []string{"web"}})

	duplicate := func(name string) *unstructured.Unstructured {
		return harness.Endpoint(name, harness.ManagedLabels(), map[string]any{
			"kind":      "http.host",
			"targetApp": "web",
			"host":      "order.test",
		})
	}

	cluster.Create(t, duplicate("first"))
	harness.ReconcileEndpoint(t, reconciler, "first")
	harness.RequireReconciled(t, cluster, "Endpoint", "first", metav1.ConditionTrue)
	if _, ok := harness.FindCondition(t, cluster, "Endpoint", "first", endpoint.ConditionConflict); ok {
		t.Fatal("the only holder carries a conflict")
	}

	cluster.Create(t, duplicate("second"))
	harness.ReconcileEndpoint(t, reconciler, "second")
	for name, other := range map[string]string{"first": "second", "second": "first"} {
		conflict := harness.RequireCondition(t, cluster, "Endpoint", name, endpoint.ConditionConflict)
		if conflict.Status != metav1.ConditionTrue || !strings.Contains(conflict.Message, other) {
			t.Fatalf("%s conflict = %v, want the duplicate with %s", name, conflict, other)
		}
	}

	harness.Delete(t, cluster, "Endpoint", "first")
	harness.ReconcileEndpoint(t, reconciler, "first")
	if condition, ok := harness.FindCondition(t, cluster, "Endpoint", "second", endpoint.ConditionConflict); ok {
		t.Fatalf("the survivor kept a stale conflict: %v", condition)
	}
	harness.RequireReconciled(t, cluster, "Endpoint", "second", metav1.ConditionTrue)
}
