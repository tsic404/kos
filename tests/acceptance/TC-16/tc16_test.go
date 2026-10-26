// Package tc16 verifies the endpoint half of AC-8: an endpoint KOS never
// adopted is read-only. Its spec and markers do not change, it produces no
// downstream object, and after its application is deleted it stays with a
// conflict the projection marks as unmanaged (TC-16; C-02, C-45).
package tc16

import (
	"context"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC16UnmanagedEndpointIsReadOnly(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))
	cluster.Create(t, harness.Endpoint("legacy", nil, map[string]any{
		"kind":      "http.host",
		"targetApp": "web",
		"host":      "legacy.test",
	}))
	before := snapshot(t, cluster, "legacy")

	reconciler := endpoint.NewReconciler(cluster.Writer(), nil)
	harness.ReconcileEndpoint(t, reconciler, "legacy")

	// KOS wrote nothing: no status, no condition, no marker, no downstream
	// object (C-02, F1).
	after := snapshot(t, cluster, "legacy")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the unmanaged endpoint changed:\nbefore %v\nafter  %v", before, after)
	}
	if _, ok := after["status.conditions"]; ok {
		t.Fatal("the unmanaged endpoint gained a condition")
	}
	if _, ok := after["status.health.state"]; ok {
		t.Fatal("the unmanaged endpoint gained a health state")
	}
	if ingresses := listIngresses(t, cluster); len(ingresses) != 0 {
		t.Fatalf("the reconciler rendered downstream objects: %v", ingresses)
	}
	if service := harness.ServiceOf(t, cluster, "legacy"); service != nil {
		t.Fatal("the unmanaged endpoint owns a downstream service")
	}
	row := projectionOf(t, reconciler, "legacy")
	if row.Attribution != "unmanaged" {
		t.Fatalf("projection attribution = %s, want unmanaged", row.Attribution)
	}
	if len(row.Conflicts) != 0 {
		t.Fatalf("a live target application produced a conflict: %v", row.Conflicts)
	}

	// Deleting the application keeps the endpoint and marks the conflict; the
	// object itself is untouched because C-45 skips what it does not manage.
	harness.Delete(t, cluster, "Application", "web")
	actions := endpoint.NewActions(cluster.Writer())
	result, err := actions.DeleteForApplication(context.Background(), endpoint.ApplicationRef(harness.Namespace, "web"))
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if len(result.Deleted) != 0 || len(result.Retained) != 1 || result.Retained[0].Name != "legacy" {
		t.Fatalf("cascade result = %+v, want legacy retained and nothing deleted", result)
	}
	if kept := snapshot(t, cluster, "legacy"); !reflect.DeepEqual(before, kept) {
		t.Fatalf("the cascade changed the unmanaged endpoint:\nbefore %v\nafter  %v", before, kept)
	}
	// The application's deletion is an event of the endpoint reconciler; it
	// records the missing target in the ledger, which stays unmanaged.
	harness.ReconcileApplication(t, reconciler, "web")
	row = projectionOf(t, reconciler, "legacy")
	if row.Attribution != "unmanaged" {
		t.Fatalf("projection attribution after deletion = %s, want unmanaged", row.Attribution)
	}
	if len(row.Conflicts) != 1 || row.Conflicts[0].Kind != endpoint.ConflictTargetMissing {
		t.Fatalf("projection conflicts = %v, want a missing target", row.Conflicts)
	}
	if len(row.Keys) != 1 || row.Keys[0].String() != "host/legacy.test" {
		t.Fatalf("projection keys = %v, want the held hostname", row.Keys)
	}
}

// snapshot flattens one endpoint so a write anywhere shows up as a difference.
func snapshot(t *testing.T, cluster *harness.Cluster, name string) map[string]string {
	t.Helper()
	return harness.Flatten(cluster.Get(t, "v1beta1", "Endpoint", name).Object)
}

// listIngresses reads the downstream kind the ingress provider would render.
func listIngresses(t *testing.T, cluster *harness.Cluster) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := cluster.Kube.NetworkingV1().Ingresses(harness.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list ingresses: %v", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.Name)
	}
	return names
}

// projectionOf reads one ledger row.
func projectionOf(t *testing.T, reconciler *endpoint.Reconciler, name string) endpoint.Projection {
	t.Helper()
	for _, row := range reconciler.Ledger().Projection() {
		if row.Ref.Name == name {
			return row
		}
	}
	t.Fatalf("%s is not in the ledger projection", name)
	return endpoint.Projection{}
}
