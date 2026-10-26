// Package tc77 verifies the L4 key space: TCP and UDP ports are separate,
// each is unique, a duplicate is a conflict, and a deleted endpoint releases its
// port for the next allocation (AC-39, TC-77; C-44).
package tc77

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"

	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC77L4PortAllocation(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))
	reconciler := endpoint.NewReconciler(cluster.Writer(), nil)

	cluster.Create(t, harness.Endpoint("tcp", harness.ManagedLabels(), map[string]any{
		"kind": "l4.tcp", "targetApp": "web", "port": 8080,
	}))
	cluster.Create(t, harness.Endpoint("udp", harness.ManagedLabels(), map[string]any{
		"kind": "l4.udp", "targetApp": "web", "port": 8080,
	}))
	harness.ReconcileEndpoint(t, reconciler, "tcp")
	harness.ReconcileEndpoint(t, reconciler, "udp")
	harness.RequireReconciled(t, cluster, "Endpoint", "tcp", metav1.ConditionTrue)
	harness.RequireReconciled(t, cluster, "Endpoint", "udp", metav1.ConditionTrue)
	if _, ok := harness.FindCondition(t, cluster, "Endpoint", "udp", endpoint.ConditionConflict); ok {
		t.Fatal("a UDP port conflicted with the same number on TCP")
	}

	// Each admitted L4 entry owns a Service route object carrying its port and
	// the target application's owner marker (C-43).
	application := cluster.Get(t, "v1beta1", "Application", "web")
	for name, protocol := range map[string]corev1.Protocol{"tcp": corev1.ProtocolTCP, "udp": corev1.ProtocolUDP} {
		service := requireService(t, cluster, name)
		if service.Spec.Type != corev1.ServiceTypeLoadBalancer {
			t.Fatalf("%s service type = %s, want LoadBalancer", name, service.Spec.Type)
		}
		if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 8080 || service.Spec.Ports[0].Protocol != protocol {
			t.Fatalf("%s service ports = %v, want one %s 8080", name, service.Spec.Ports, protocol)
		}
		if got := service.Spec.Selector[markers.OwnerUID]; got != string(application.GetUID()) {
			t.Fatalf("%s selector owner-uid = %q, want the target application UID %q", name, got, application.GetUID())
		}
	}

	// The same TCP port is refused, and the conflict names the holder.
	cluster.Create(t, harness.Endpoint("tcp-again", harness.ManagedLabels(), map[string]any{
		"kind": "l4.tcp", "targetApp": "web", "port": 8080,
	}))
	harness.ReconcileEndpoint(t, reconciler, "tcp-again")
	conflict := harness.RequireCondition(t, cluster, "Endpoint", "tcp-again", endpoint.ConditionConflict)
	if conflict.Status != metav1.ConditionTrue || !strings.Contains(conflict.Message, "tcp/8080") {
		t.Fatalf("conflict = %v, want the taken tcp/8080 key", conflict)
	}
	if !strings.Contains(conflict.Message, harness.Namespace+"/tcp") {
		t.Fatalf("conflict %q does not list the holding endpoint by name", conflict.Message)
	}
	if service := harness.ServiceOf(t, cluster, "tcp-again"); service != nil {
		t.Fatal("a conflicted overlapping endpoint produced a route object")
	}

	// Deleting the holder releases the port, and the delete event settles the
	// survivor without a replay of its own round.
	harness.Delete(t, cluster, "Endpoint", "tcp")
	harness.ReconcileEndpoint(t, reconciler, "tcp")
	if condition, ok := harness.FindCondition(t, cluster, "Endpoint", "tcp-again", endpoint.ConditionConflict); ok {
		t.Fatalf("the port stayed taken after a deletion: %v", condition)
	}
	harness.RequireReconciled(t, cluster, "Endpoint", "tcp-again", metav1.ConditionTrue)
	if service := harness.ServiceOf(t, cluster, "tcp"); service != nil {
		t.Fatal("the route service survived its endpoint")
	}
	requireService(t, cluster, "tcp-again")

	// A port on the other protocol stays independent.
	if _, ok := harness.FindCondition(t, cluster, "Endpoint", "udp", endpoint.ConditionConflict); ok {
		t.Fatal("the UDP endpoint became conflicted when a TCP endpoint was deleted")
	}
}

// requireService returns the route service of an admitted L4 entry and checks
// the attribution the route object carries.
func requireService(t *testing.T, cluster *harness.Cluster, name string) *corev1.Service {
	t.Helper()
	service := harness.ServiceOf(t, cluster, name)
	if service == nil {
		t.Fatalf("%s has no route service", name)
	}
	endpointObject := cluster.Get(t, "v1beta1", "Endpoint", name)
	if got := service.Labels[markers.OwnerUID]; got != string(endpointObject.GetUID()) {
		t.Fatalf("%s service owner-uid = %q, want the endpoint UID %q", name, got, endpointObject.GetUID())
	}
	if service.Labels[markers.ManagedBy] == "" {
		t.Fatalf("%s service carries no managed-by marker", name)
	}
	return service
}
