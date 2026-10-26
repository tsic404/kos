// Package tc76 verifies the two declarations an http.path endpoint needs: the
// path-prefix capability and a package that declares path-prefix use safe. A
// write missing either is refused with a reason naming it, and an object that
// bypassed the API is marked invalid instead of admitted (AC-38, TC-76; C-43).
package tc76

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC76PathEndpointNeedsCapabilityAndDeclaration(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("safe", nil))
	cluster.Create(t, harness.Application("unsafe", nil))

	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.EndpointFacts = harness.Declarations{Capable: true, Safe: []string{"safe"}}
	})

	// The declared package is accepted and the endpoint is admitted.
	status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Endpoint?namespace="+harness.Namespace,
		pathObject("safe", "safe-path", "safe.test", "/safe"), nil)
	if status != http.StatusAccepted {
		t.Fatalf("declared path endpoint: status %d, want 202: %v", status, body)
	}
	reconciler := endpoint.NewReconciler(cluster.Writer(), harness.Declarations{Capable: true, Safe: []string{"safe"}})
	harness.ReconcileEndpoint(t, reconciler, "safe-path")
	harness.RequireReconciled(t, cluster, "Endpoint", "safe-path", metav1.ConditionTrue)
	if health := harness.HealthOf(t, cluster, "Endpoint", "safe-path"); health != "Healthy" {
		t.Fatalf("health = %q, want Healthy", health)
	}

	// 路由可达 is a readable object fact: the Ingress exists, carries the
	// endpoint's owner marker and points at the target application (C-43).
	requireIngress(t, cluster, "safe-path", "safe.test", "/safe", "safe")

	// An undeclared package is refused, and the reason points at the declaration.
	status, body = gateway.Call(t, http.MethodPost, "/api/v1/resources/Endpoint?namespace="+harness.Namespace,
		pathObject("unsafe", "unsafe-path", "unsafe.test", "/unsafe"), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("undeclared path endpoint: status %d, want 400: %v", status, body)
	}
	failure := body["error"].(map[string]any)
	if harness.String(failure, "code") != controlplane.CodeInvalidField {
		t.Fatalf("code = %s, want %s", harness.String(failure, "code"), controlplane.CodeInvalidField)
	}
	if message := harness.String(failure, "message"); !strings.Contains(message, "spec.path") || !strings.Contains(message, "未声明") {
		t.Fatalf("message %q does not name the missing declaration", message)
	}

	// A cluster without the capability is refused the same way, naming it.
	withoutCapability := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.EndpointFacts = harness.Declarations{Capable: false, Safe: []string{"safe"}}
	})
	status, body = withoutCapability.Call(t, http.MethodPost, "/api/v1/resources/Endpoint?namespace="+harness.Namespace,
		pathObject("safe", "no-capability", "cap.test", "/cap"), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("missing capability: status %d, want 400: %v", status, body)
	}
	if message := harness.String(body, "error", "message"); !strings.Contains(message, endpoint.PathPrefixCapability) {
		t.Fatalf("message %q does not name %s", message, endpoint.PathPrefixCapability)
	}

	// An object written around the API is not admitted: the reconciler rejects
	// the same shape with a condition instead of assuming the declarations.
	cluster.Create(t, harness.Endpoint("bypassed", harness.ManagedLabels(), map[string]any{
		"kind":      "http.path",
		"targetApp": "unsafe",
		"host":      "bypassed.test",
		"path":      "/bypass",
	}))
	harness.ReconcileEndpoint(t, reconciler, "bypassed")
	invalid := harness.RequireCondition(t, cluster, "Endpoint", "bypassed", endpoint.ConditionInvalidSpec)
	if invalid.Status != metav1.ConditionTrue || !strings.Contains(invalid.Message, "spec.path") {
		t.Fatalf("%s = %v, want a reason naming spec.path", endpoint.ConditionInvalidSpec, invalid)
	}
	harness.RequireReconciled(t, cluster, "Endpoint", "bypassed", metav1.ConditionFalse)
	if ingress := harness.IngressOf(t, cluster, "bypassed"); ingress != nil {
		t.Fatal("an invalid path entry produced a route object")
	}

	// Deleting the entry removes its route object.
	harness.Delete(t, cluster, "Endpoint", "safe-path")
	harness.ReconcileEndpoint(t, reconciler, "safe-path")
	if ingress := harness.IngressOf(t, cluster, "safe-path"); ingress != nil {
		t.Fatal("the route object survived its endpoint")
	}
}

// requireIngress asserts the route facts TC-76's 「路由可达」 names.
func requireIngress(t *testing.T, cluster *harness.Cluster, name, host, path, app string) {
	t.Helper()
	ingress := harness.IngressOf(t, cluster, name)
	if ingress == nil {
		t.Fatalf("%s has no route object", name)
	}
	endpoint := cluster.Get(t, "v1beta1", "Endpoint", name)
	if got := ingress.Labels[markers.OwnerUID]; got != string(endpoint.GetUID()) {
		t.Fatalf("ingress owner-uid = %q, want the endpoint UID %q", got, endpoint.GetUID())
	}
	if ingress.Labels[markers.ManagedBy] == "" {
		t.Fatal("the ingress carries no managed-by marker")
	}
	if len(ingress.Spec.Rules) != 1 || ingress.Spec.Rules[0].Host != host {
		t.Fatalf("rules = %v, want one rule for %s", ingress.Spec.Rules, host)
	}
	paths := ingress.Spec.Rules[0].HTTP.Paths
	if len(paths) != 1 {
		t.Fatalf("paths = %v, want one", paths)
	}
	if paths[0].Path != path || paths[0].PathType == nil || *paths[0].PathType != networkingv1.PathTypePrefix {
		t.Fatalf("path = %v, want a %s prefix", paths[0], path)
	}
	backend := paths[0].Backend.Service
	if backend == nil || backend.Name != app || backend.Port.Name != "http" {
		t.Fatalf("backend = %v, want service %s port http", backend, app)
	}
}

// pathObject builds the write body of an http.path endpoint.
func pathObject(app, name, host, path string) map[string]any {
	object := harness.Endpoint(name, harness.ManagedLabels(), map[string]any{
		"kind":      "http.path",
		"targetApp": app,
		"host":      host,
		"path":      path,
	})
	return object.Object
}

// TestTC76RouteBlockedIsAVisibleFailure covers the endpoint whose route object
// cannot be written: a same-name object KOS does not own stays in the way, and
// the endpoint must not claim the desired state is applied (C-07, TC-76).
func TestTC76RouteBlockedIsAVisibleFailure(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("safe", nil))
	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.EndpointFacts = harness.Declarations{Capable: true, Safe: []string{"safe"}}
	})
	status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Endpoint?namespace="+harness.Namespace,
		pathObject("safe", "safe-path", "safe.test", "/safe"), nil)
	if status != http.StatusAccepted {
		t.Fatalf("path endpoint: status %d: %v", status, body)
	}
	harness.CreateUnmanagedIngress(t, cluster, harness.Namespace, "safe-path")

	reconciler := endpoint.NewReconciler(cluster.Writer(), harness.Declarations{Capable: true, Safe: []string{"safe"}})
	result, err := reconciler.Reconcile(context.Background(), reconcile.Request{Kind: "Endpoint", Namespace: harness.Namespace, Name: "safe-path"})
	if err != nil {
		t.Fatalf("a permanent refusal surfaced as a round error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("a refused route did not schedule a re-observation")
	}

	blocked := harness.RequireCondition(t, cluster, "Endpoint", "safe-path", endpoint.ConditionRouteBlocked)
	if blocked.Status != metav1.ConditionTrue || !strings.Contains(blocked.Message, "attribution") {
		t.Fatalf("%s = %v, want the refusal of the unowned object", endpoint.ConditionRouteBlocked, blocked)
	}
	harness.RequireReconciled(t, cluster, "Endpoint", "safe-path", metav1.ConditionFalse)
	if health := harness.HealthOf(t, cluster, "Endpoint", "safe-path"); health == "Healthy" {
		t.Fatal("an endpoint whose route object is missing reports Healthy")
	}

	// The unmanaged object stays untouched; clearing it lets the route land.
	harness.RemoveIngress(t, cluster, harness.Namespace, "safe-path")
	harness.ReconcileEndpoint(t, reconciler, "safe-path")
	harness.RequireReconciled(t, cluster, "Endpoint", "safe-path", metav1.ConditionTrue)
	if _, ok := harness.FindCondition(t, cluster, "Endpoint", "safe-path", endpoint.ConditionRouteBlocked); ok {
		t.Fatal("the route failure condition survived the successful write")
	}
	requireIngress(t, cluster, "safe-path", "safe.test", "/safe", "safe")
}

// TestTC76RefusedSurvivorRecoversOnTheReobserveInterval covers the recovery of
// a survivor refused inside a peer's round: no later event names that entry, so
// the fixed re-observation interval has to bring it back once the blocking
// object goes away (C-06, C-07).
func TestTC76RefusedSurvivorRecoversOnTheReobserveInterval(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))
	cluster.Create(t, harness.Endpoint("holder", harness.ManagedLabels(), map[string]any{
		"kind": "http.host", "targetApp": "web", "host": "shared.test",
	}))
	cluster.Create(t, harness.Endpoint("survivor", harness.ManagedLabels(), map[string]any{
		"kind": "http.host", "targetApp": "web", "host": "shared.test",
	}))

	clock := &stepClock{now: time.Now()}
	reconciler := endpoint.NewReconciler(cluster.Writer(), nil)
	reconciler.Now = clock.Now
	ctx := context.Background()

	// Both entries are in the ledger before the deletion: the holder wins the
	// host and the survivor reports the conflict.
	harness.ReconcileEndpoint(t, reconciler, "holder")
	harness.ReconcileEndpoint(t, reconciler, "survivor")
	harness.RequireCondition(t, cluster, "Endpoint", "survivor", endpoint.ConditionConflict)

	// An object KOS does not own sits on the survivor's route name.
	harness.CreateUnmanagedIngress(t, cluster, harness.Namespace, "survivor")

	// Deleting the holder hands the host to the survivor inside the holder's
	// round, where the unowned object refuses its route write.
	harness.Delete(t, cluster, "Endpoint", "holder")
	result, err := reconciler.Reconcile(ctx, reconcile.Request{Kind: "Endpoint", Namespace: harness.Namespace, Name: "holder"})
	if err != nil {
		t.Fatalf("the holder's deletion round: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("the refused survivor scheduled no re-observation")
	}
	harness.RequireCondition(t, cluster, "Endpoint", "survivor", endpoint.ConditionRouteBlocked)
	harness.RequireReconciled(t, cluster, "Endpoint", "survivor", metav1.ConditionFalse)

	// Clearing the blocker produces no event about the survivor: only the clock
	// moves, and a round of the deleted holder has to bring the survivor back.
	harness.RemoveIngress(t, cluster, harness.Namespace, "survivor")
	clock.advance(time.Minute)
	if _, err := reconciler.Reconcile(ctx, reconcile.Request{Kind: "Endpoint", Namespace: harness.Namespace, Name: "holder"}); err != nil {
		t.Fatalf("the re-observation round: %v", err)
	}
	harness.RequireReconciled(t, cluster, "Endpoint", "survivor", metav1.ConditionTrue)
	if _, ok := harness.FindCondition(t, cluster, "Endpoint", "survivor", endpoint.ConditionRouteBlocked); ok {
		t.Fatal("the survivor stayed blocked after its re-observation")
	}
	if ingress := harness.IngressOf(t, cluster, "survivor"); ingress == nil {
		t.Fatal("the recovered survivor has no route object")
	}
}

// TestTC76RouteInTheKOSNamespaceIsBlocked covers an endpoint whose namespace
// belongs to the installer: its route object is refused with a readable reason
// instead of failing silently (C-04).
func TestTC76RouteInTheKOSNamespaceIsBlocked(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	cluster.CreateIn(t, api.SystemNamespace, harness.ApplicationIn(api.SystemNamespace, "web", nil))
	cluster.CreateIn(t, api.SystemNamespace, harness.EndpointIn(api.SystemNamespace, "system-site", harness.ManagedLabels(), map[string]any{
		"kind":      "http.host",
		"targetApp": "web",
		"host":      "system.test",
	}))

	reconciler := endpoint.NewReconciler(cluster.Writer(), nil)
	result, err := reconciler.Reconcile(context.Background(), reconcile.Request{Kind: "Endpoint", Namespace: api.SystemNamespace, Name: "system-site"})
	if err != nil {
		t.Fatalf("the KOS namespace refusal surfaced as a round error: %v", err)
	}
	// The refusal is final, not a route waiting for another object: the reason
	// is the endpoint's own sentinel and no round is scheduled.
	if result.RequeueAfter != 0 {
		t.Fatalf("a final refusal scheduled a replay in %v", result.RequeueAfter)
	}

	stored := cluster.GetIn(t, api.SystemNamespace, "v1beta1", "Endpoint", "system-site")
	conditions := harness.Conditions(t, stored)
	blocked, ok := state.Find(conditions, endpoint.ConditionRouteBlocked)
	if !ok || blocked.Status != metav1.ConditionTrue || !strings.Contains(blocked.Message, "belongs to the installer") {
		t.Fatalf("%s = %v, want the endpoint's own namespace reason", endpoint.ConditionRouteBlocked, blocked)
	}
	if reconciled, _ := state.Find(conditions, api.ConditionReconciled); reconciled.Status != metav1.ConditionFalse {
		t.Fatalf("Reconciled = %s, want False", reconciled.Status)
	}
	if health, _ := harness.Nested(stored.Object, "status", "health", "state").(string); health == "Healthy" {
		t.Fatal("an endpoint without a route object reports Healthy")
	}
	if ingress := harness.IngressOfIn(t, cluster, api.SystemNamespace, "system-site"); ingress != nil {
		t.Fatal("a route object was written into the KOS namespace")
	}
}

// TestTC76ABlockedRouteDoesNotStallAnotherEndpoint covers a route write that
// fails for a reason other than the allocation tables: the entry is rescheduled
// inside a retry budget, and a round of another endpoint still converges
// (C-06).
func TestTC76ABlockedRouteDoesNotStallAnotherEndpoint(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))
	cluster.Create(t, harness.Endpoint("blocked", harness.ManagedLabels(), map[string]any{
		"kind": "http.host", "targetApp": "web", "host": "blocked.test",
	}))
	cluster.Create(t, harness.Endpoint("healthy", harness.ManagedLabels(), map[string]any{
		"kind": "http.host", "targetApp": "web", "host": "healthy.test",
	}))

	// Another field manager owns the blocking object's rules: the route write is
	// a field conflict, not an allocation refusal.
	blockedEndpoint := cluster.Get(t, "v1beta1", "Endpoint", "blocked")
	preowned := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata": map[string]any{
			"name":      "blocked",
			"namespace": harness.Namespace,
			"labels": map[string]any{
				markers.ManagedBy: "kos-shell",
				markers.OwnerUID:  string(blockedEndpoint.GetUID()),
			},
		},
		"spec": map[string]any{"rules": []any{map[string]any{"host": "elsewhere.test"}}},
	}}
	ctx := context.Background()
	ingresses := cluster.Dynamic.Resource(schema.GroupVersionResource{
		Group: "networking.k8s.io", Version: "v1", Resource: "ingresses",
	}).Namespace(harness.Namespace)
	if _, err := ingresses.Apply(ctx, "blocked", preowned, metav1.ApplyOptions{FieldManager: "kubectl"}); err != nil {
		t.Fatalf("own the blocking ingress: %v", err)
	}

	clock := &stepClock{now: time.Now()}
	reconciler := endpoint.NewReconciler(cluster.Writer(), nil)
	reconciler.Now = clock.Now

	// The blocked round fails on its own request and schedules the replay.
	if _, err := reconciler.Reconcile(ctx, reconcile.Request{Kind: "Endpoint", Namespace: harness.Namespace, Name: "blocked"}); err == nil {
		t.Fatal("the conflicting route write did not surface")
	}
	harness.RequireCondition(t, cluster, "Endpoint", "blocked", endpoint.ConditionRouteBlocked)

	// A later round replays the failed entry first and still settles its own
	// request, so one entry cannot starve the others.
	clock.advance(6 * time.Minute)
	result, err := reconciler.Reconcile(ctx, reconcile.Request{Kind: "Endpoint", Namespace: harness.Namespace, Name: "healthy"})
	if err != nil {
		t.Fatalf("the pending replay stalled another endpoint: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("the scheduled replay did not ask for a further round")
	}
	if condition, ok := harness.FindCondition(t, cluster, "Endpoint", "healthy", endpoint.ConditionRouteBlocked); ok {
		t.Fatalf("the unrelated endpoint carries a route failure: %v", condition)
	}
	harness.RequireReconciled(t, cluster, "Endpoint", "healthy", metav1.ConditionTrue)
	if ingress := harness.IngressOf(t, cluster, "healthy"); ingress == nil {
		t.Fatal("the healthy endpoint has no route object")
	}
}

// stepClock is a clock a case can advance.
type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }

func (c *stepClock) advance(delay time.Duration) { c.now = c.now.Add(delay) }
