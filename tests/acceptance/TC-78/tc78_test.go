// Package tc78 verifies the multicast shape: its projection carries the
// shared-medium warning, the field matrix refuses the fields it forbids, and
// the service name plus port is a unique key while two ports may coexist
// (AC-39, TC-78; C-43, C-44).
package tc78

import (
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC78MulticastShape(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))
	reconciler := endpoint.NewReconciler(cluster.Writer(), nil)

	cluster.Create(t, harness.Endpoint("cast", harness.ManagedLabels(), mdnsShape("cast", 5353)))
	harness.ReconcileEndpoint(t, reconciler, "cast")
	harness.RequireReconciled(t, cluster, "Endpoint", "cast", metav1.ConditionTrue)

	// The projection carries the security impact the panel must show.
	row := projectionOf(t, reconciler, "cast")
	if !row.SharedMedium || row.Note != endpoint.SharedMediumNote {
		t.Fatalf("projection row = %+v, want the shared-medium note", row)
	}

	// mdns maps to no downstream object: the multicast provider observes the
	// entry itself (C-43).
	requireNoRoute(t, cluster, "cast")

	// The matrix refuses a host on the multicast shape at the API.
	gateway := cluster.StartControlPlane(t, nil)
	illegal := harness.Endpoint("cast-host", harness.ManagedLabels(), mdnsShape("cast-host", 5353))
	illegal.Object["spec"].(map[string]any)["host"] = "cast.test"
	status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Endpoint?namespace="+harness.Namespace, illegal.Object, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("mdns with host: status %d, want 400: %v", status, body)
	}
	if message := harness.String(body, "error", "message"); !strings.Contains(message, "spec.host") {
		t.Fatalf("message %q does not name spec.host", message)
	}

	// A shape written around the API is marked invalid instead of admitted.
	bypassed := harness.Endpoint("bypassed", harness.ManagedLabels(), mdnsShape("bypassed", 5353))
	bypassed.Object["spec"].(map[string]any)["tls"] = map[string]any{"issuerRef": "internal"}
	cluster.Create(t, bypassed)
	harness.ReconcileEndpoint(t, reconciler, "bypassed")
	invalid := harness.RequireCondition(t, cluster, "Endpoint", "bypassed", endpoint.ConditionInvalidSpec)
	if invalid.Status != metav1.ConditionTrue || !strings.Contains(invalid.Message, "spec.tls") {
		t.Fatalf("%s = %v, want a reason naming spec.tls", endpoint.ConditionInvalidSpec, invalid)
	}
	requireNoRoute(t, cluster, "bypassed")

	// The same service name and port is one key: the second holder is a conflict.
	cluster.Create(t, harness.Endpoint("cast-again", harness.ManagedLabels(), mdnsShape("cast", 5353)))
	harness.ReconcileEndpoint(t, reconciler, "cast-again")
	conflict := harness.RequireCondition(t, cluster, "Endpoint", "cast-again", endpoint.ConditionConflict)
	if conflict.Status != metav1.ConditionTrue || !strings.Contains(conflict.Message, "mdns/cast:5353") {
		t.Fatalf("conflict = %v, want the taken mdns/cast:5353 key", conflict)
	}

	// The same service name on another port coexists.
	cluster.Create(t, harness.Endpoint("cast-alt", harness.ManagedLabels(), mdnsShape("cast", 5354)))
	harness.ReconcileEndpoint(t, reconciler, "cast-alt")
	harness.RequireReconciled(t, cluster, "Endpoint", "cast-alt", metav1.ConditionTrue)
	if condition, ok := harness.FindCondition(t, cluster, "Endpoint", "cast-alt", endpoint.ConditionConflict); ok {
		t.Fatalf("a different port conflicted: %v", condition)
	}
	requireNoRoute(t, cluster, "cast-alt")
}

// mdnsShape builds a multicast spec.
func mdnsShape(service string, port int) map[string]any {
	return map[string]any{"kind": "mdns", "targetApp": "web", "serviceName": service, "port": port}
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

// requireNoRoute asserts an entry owned no downstream object.
func requireNoRoute(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	if ingress := harness.IngressOf(t, cluster, name); ingress != nil {
		t.Fatalf("%s owns an ingress", name)
	}
	if service := harness.ServiceOf(t, cluster, name); service != nil {
		t.Fatalf("%s owns a service", name)
	}
}
