// Package tc73 verifies that the endpoint key space is mutually exclusive: a
// hostname carried by an http.host endpoint keeps every http.path on that host
// out, and deleting the holder releases the keys (AC-37, TC-73).
package tc73

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC73KeyMutexAndRelease(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))

	// Both shapes are legal on their own: the path endpoint has the capability
	// and the package declares path safety, so only the host mutex can refuse it.
	reconciler := endpoint.NewReconciler(cluster.Writer(), harness.Declarations{Capable: true, Safe: []string{"web"}})

	cluster.Create(t, harness.Endpoint("site", harness.ManagedLabels(), map[string]any{
		"kind":      "http.host",
		"targetApp": "web",
		"host":      "site.test",
	}))
	harness.ReconcileEndpoint(t, reconciler, "site")
	harness.RequireReconciled(t, cluster, "Endpoint", "site", metav1.ConditionTrue)

	// The same hostname now refuses a path endpoint.
	cluster.Create(t, harness.Endpoint("site-path", harness.ManagedLabels(), map[string]any{
		"kind":      "http.path",
		"targetApp": "web",
		"host":      "site.test",
		"path":      "/api",
	}))
	harness.ReconcileEndpoint(t, reconciler, "site-path")
	conflict := harness.RequireCondition(t, cluster, "Endpoint", "site-path", endpoint.ConditionConflict)
	if conflict.Status != metav1.ConditionTrue {
		t.Fatalf("%s = %s, want True", endpoint.ConditionConflict, conflict.Status)
	}
	if !strings.Contains(conflict.Message, endpoint.ConflictHostExclusive) && !strings.Contains(conflict.Message, "site.test") {
		t.Fatalf("conflict message %q does not name the exclusive host", conflict.Message)
	}
	if !strings.Contains(conflict.Message, "site") {
		t.Fatalf("conflict message %q does not list the holding endpoint", conflict.Message)
	}
	harness.RequireReconciled(t, cluster, "Endpoint", "site-path", metav1.ConditionFalse)
	if health := harness.HealthOf(t, cluster, "Endpoint", "site-path"); health != "Unavailable" {
		t.Fatalf("health = %q, want Unavailable", health)
	}

	// Deleting the holder releases the key; the delete event settles the
	// survivor in the same round, with no replay for the path endpoint.
	harness.Delete(t, cluster, "Endpoint", "site")
	harness.ReconcileEndpoint(t, reconciler, "site")
	if condition, ok := harness.FindCondition(t, cluster, "Endpoint", "site-path", endpoint.ConditionConflict); ok {
		t.Fatalf("the host conflict survived the release: %v", condition)
	}
	harness.RequireReconciled(t, cluster, "Endpoint", "site-path", metav1.ConditionTrue)
	if rows := reconciler.Ledger().Projection(); len(rows) != 1 || rows[0].Ref.Name != "site-path" {
		t.Fatalf("projection = %v, want the remaining endpoint alone", rows)
	}
}
