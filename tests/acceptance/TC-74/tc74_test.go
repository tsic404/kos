// Package tc74 verifies that an endpoint's target application is fixed at
// creation, that the reverse lookup lists exactly the endpoints that expose an
// application, and that deleting the application removes the taken-over
// endpoints while the unmanaged ones stay (AC-37, TC-74; C-45).
package tc74

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// requestTimeout bounds one direct API call.
const requestTimeout = 30 * time.Second

func TestTC74TargetAppIsImmutableAndReverseLookupMatchesDeletion(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))
	cluster.Create(t, harness.Application("other", nil))

	// Two taken-over endpoints and one KOS never adopted.
	for _, object := range []*unstructured.Unstructured{
		harness.Endpoint("site", harness.ManagedLabels(), hostShape("web", "site.test")),
		harness.Endpoint("extra", harness.ManagedLabels(), hostShape("other", "extra.test")),
		harness.Endpoint("legacy", nil, hostShape("web", "legacy.test")),
	} {
		cluster.Create(t, object)
	}
	reconciler := endpoint.NewReconciler(cluster.Writer(), nil)
	harness.ReconcileEndpoint(t, reconciler, "site")
	harness.ReconcileEndpoint(t, reconciler, "extra")
	harness.ReconcileEndpoint(t, reconciler, "legacy")
	harness.RequireReconciled(t, cluster, "Endpoint", "site", metav1.ConditionTrue)
	harness.RequireReconciled(t, cluster, "Endpoint", "extra", metav1.ConditionTrue)
	if ingress := harness.IngressOf(t, cluster, "site"); ingress == nil {
		t.Fatal("the admitted endpoint has no route object")
	}

	gateway := cluster.StartControlPlane(t, nil)

	// The reverse lookup lists the endpoints that expose "web", taken over or not.
	if got := reverse(t, gateway, "web"); !equal(got, []string{"legacy", "site"}) {
		t.Fatalf("reverse lookup of web = %v, want [legacy site]", got)
	}
	if got := reverse(t, gateway, "other"); !equal(got, []string{"extra"}) {
		t.Fatalf("reverse lookup of other = %v, want [extra]", got)
	}

	// A native write of the target application is refused by the CRD's
	// transition rule, with a reason naming the field.
	current := cluster.Get(t, "v1beta1", "Endpoint", "site")
	if err := unstructured.SetNestedField(current.Object, "other", "spec", "targetApp"); err != nil {
		t.Fatalf("set targetApp: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "Endpoint", harness.Namespace).Update(ctx, current, metav1.UpdateOptions{FieldManager: "kubectl"}); err == nil {
		t.Fatal("a native write changed the target application")
	} else if !strings.Contains(err.Error(), "spec.targetApp") {
		t.Fatalf("refusal %v does not name spec.targetApp", err)
	}

	// The control plane refuses the same rewrite before it reaches the cluster.
	update := harness.Endpoint("site", harness.ManagedLabels(), hostShape("other", "site.test"))
	update.Object["metadata"].(map[string]any)["resourceVersion"] = current.GetResourceVersion()
	status, body := gateway.Call(t, http.MethodPut, "/api/v1/resources/Endpoint/site?namespace="+harness.Namespace, update.Object, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("update status = %d, want 400: %v", status, body)
	}
	if code := harness.String(body, "error", "code"); code != controlplane.CodeInvalidField {
		t.Fatalf("code = %s, want %s", code, controlplane.CodeInvalidField)
	}
	if message := harness.String(body, "error", "message"); !strings.Contains(message, "spec.targetApp") {
		t.Fatalf("message %q does not name the field", message)
	}
	if kept := cluster.Get(t, "v1beta1", "Endpoint", "site").Object["spec"].(map[string]any)["targetApp"]; kept != "web" {
		t.Fatalf("targetApp = %v after a refused write, want web", kept)
	}

	// Deleting the application removes the taken-over endpoints and keeps the
	// unmanaged one, which the projection now marks as pointing at a missing
	// application.
	harness.Delete(t, cluster, "Application", "web")
	actions := endpoint.NewActions(cluster.Writer())
	result, err := actions.DeleteForApplication(ctx, endpoint.ApplicationRef(harness.Namespace, "web"))
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0].Name != "site" {
		t.Fatalf("deleted = %v, want site", result.Deleted)
	}
	if len(result.Retained) != 1 || result.Retained[0].Name != "legacy" {
		t.Fatalf("retained = %v, want legacy", result.Retained)
	}
	if got := reverse(t, gateway, "web"); !equal(got, []string{"legacy"}) {
		t.Fatalf("reverse lookup after deletion = %v, want [legacy]", got)
	}
	if got := reverse(t, gateway, "other"); !equal(got, []string{"extra"}) {
		t.Fatalf("the unrelated application lost its endpoint: %v", got)
	}
	harness.ReconcileEndpoint(t, reconciler, "site")
	// The application's deletion is also an event of the endpoint reconciler: it
	// marks the endpoints that still point at the application.
	harness.ReconcileApplication(t, reconciler, "web")
	if ingress := harness.IngressOf(t, cluster, "site"); ingress != nil {
		t.Fatal("the route object survived the cascade")
	}
	if ingress := harness.IngressOf(t, cluster, "legacy"); ingress != nil {
		t.Fatal("an unmanaged endpoint owns a route object")
	}

	legacy := cluster.Get(t, "v1beta1", "Endpoint", "legacy")
	if legacy.GetLabels() != nil {
		t.Fatalf("the unmanaged endpoint gained markers: %v", legacy.GetLabels())
	}
	if kept := legacy.Object["spec"].(map[string]any)["targetApp"]; kept != "web" {
		t.Fatalf("the unmanaged endpoint spec changed: %v", legacy.Object["spec"])
	}
	row := projectionOf(t, reconciler, "legacy")
	if row.Attribution != "unmanaged" {
		t.Fatalf("legacy attribution = %s, want unmanaged", row.Attribution)
	}
	if len(row.Conflicts) != 1 || row.Conflicts[0].Kind != endpoint.ConflictTargetMissing {
		t.Fatalf("legacy conflicts = %v, want a missing target", row.Conflicts)
	}
}

// hostShape builds an http.host spec.
func hostShape(app, host string) map[string]any {
	return map[string]any{"kind": "http.host", "targetApp": app, "host": host}
}

// reverse asks the control plane which endpoints expose one application.
func reverse(t *testing.T, gateway *harness.ControlPlane, app string) []string {
	t.Helper()
	status, body := gateway.Call(t, http.MethodGet,
		"/api/v1/resources/Endpoint?namespace="+harness.Namespace+"&targetApp="+app, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("reverse lookup status = %d: %v", status, body)
	}
	names := make([]string, 0)
	for _, item := range harness.Items(t, body) {
		names = append(names, harness.String(item, "ref", "name"))
	}
	sort.Strings(names)
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

// equal compares two string lists.
func equal(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestTC74CascadeSettlesTheSurvivor(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))
	cluster.Create(t, harness.Application("api", nil))
	for name, app := range map[string]string{"holder": "web", "survivor": "api"} {
		cluster.Create(t, harness.Endpoint(name, harness.ManagedLabels(), map[string]any{
			"kind":      "http.host",
			"targetApp": app,
			"host":      "shared.test",
		}))
	}
	reconciler := endpoint.NewReconciler(cluster.Writer(), nil)
	harness.ReconcileEndpoint(t, reconciler, "holder")
	harness.ReconcileEndpoint(t, reconciler, "survivor")
	for _, name := range []string{"holder", "survivor"} {
		harness.RequireCondition(t, cluster, "Endpoint", name, endpoint.ConditionConflict)
	}
	if ingress := harness.IngressOf(t, cluster, "survivor"); ingress != nil {
		t.Fatal("a conflicted endpoint owns a route object")
	}

	// The application cascade deletes the holder; it does not touch the ledger.
	harness.Delete(t, cluster, "Application", "web")
	actions := endpoint.NewActions(cluster.Writer())
	result, err := actions.DeleteForApplication(context.Background(), endpoint.ApplicationRef(harness.Namespace, "web"))
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0].Name != "holder" {
		t.Fatalf("deleted = %v, want the holder alone", result.Deleted)
	}

	// The holder's delete event releases the key and settles the survivor in the
	// same round: the survivor gets no round of its own (C-06, C-44).
	harness.ReconcileEndpoint(t, reconciler, "holder")
	if condition, ok := harness.FindCondition(t, cluster, "Endpoint", "survivor", endpoint.ConditionConflict); ok {
		t.Fatalf("the survivor kept a stale conflict: %v", condition)
	}
	harness.RequireReconciled(t, cluster, "Endpoint", "survivor", metav1.ConditionTrue)
	if ingress := harness.IngressOf(t, cluster, "survivor"); ingress == nil {
		t.Fatal("the admitted survivor has no route object")
	}
}
