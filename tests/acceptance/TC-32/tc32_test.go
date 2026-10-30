// Package tc32 verifies F8/C-59: role and visibility never decide whether an
// entry is reachable. An application the panel hides from a member keeps its
// route, and an endpoint with no auth binding carries no role check on the
// route object (AC-16, TC-32).
//
// The route object is the readable fact this case judges: KOS does not run the
// data path (the run kernel and the ingress implementation do), so "the request
// reaches the application's own login page" is observed as "the route exists,
// points at the application and carries no auth binding" — the same observation
// TC-76 uses for reachability.
package tc32

import (
	"net/http"
	"testing"

	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC32RolesDoNotGateEntry hides one application from a member and checks the
// entry it exposes is unaffected.
func TestTC32RolesDoNotGateEntry(t *testing.T) {
	cluster := harness.Start(t)
	shop := harness.Application("shop", nil)
	shop.Object["spec"] = map[string]any{"owner": "alice", "visibility": map[string]any{}}
	cluster.Create(t, shop)
	// The endpoint binds no auth provider: C-46 makes forward-auth an opt-in
	// reference, and this one has none.
	cluster.Create(t, harness.Endpoint("shop-entry", harness.ManagedLabels(), map[string]any{
		"kind":      "http.host",
		"targetApp": "shop",
		"host":      "shop.example.test",
	}))
	harness.ReconcileEndpoint(t, endpoint.NewReconciler(cluster.Writer(), harness.Declarations{}), "shop-entry")

	system := cluster.StartAccess(t, nil, nil)
	owner := system.Owner(t, "owner", "owner-password")
	carol := system.Account(t, owner, "carol", "carol-password", write.RoleMember)

	// The panel hides the application from carol (C-59) ...
	status, body := system.Call(t, carol, http.MethodGet, "/api/v1/resources/Application?namespace="+harness.Namespace, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list applications = %d %v", status, body)
	}
	for _, item := range body["items"].([]any) {
		object, _ := item.(map[string]any)
		if harness.String(object, "object", "metadata", "name") == "shop" {
			t.Fatalf("the application was visible to a member it does not list: %v", object)
		}
	}

	// ... while the entry stays readable and its route unchanged.
	status, body = system.Call(t, carol, http.MethodGet, "/api/v1/resources/Endpoint?namespace="+harness.Namespace, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list endpoints = %d %v", status, body)
	}
	found := false
	for _, item := range body["items"].([]any) {
		object, _ := item.(map[string]any)
		if harness.String(object, "object", "metadata", "name") == "shop-entry" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a hidden application's endpoint disappeared from the projection: %v", body)
	}

	ingress := harness.IngressOf(t, cluster, "shop-entry")
	if ingress == nil {
		t.Fatal("the entry has no route object")
	}
	entry := cluster.Get(t, "v1beta1", "Endpoint", "shop-entry")
	if got := ingress.Labels[markers.OwnerUID]; got != string(entry.GetUID()) {
		t.Fatalf("route owner-uid = %q, want the endpoint UID %q", got, entry.GetUID())
	}
	if len(ingress.Spec.Rules) != 1 || ingress.Spec.Rules[0].Host != "shop.example.test" {
		t.Fatalf("route rules = %v, want one rule for shop.example.test", ingress.Spec.Rules)
	}
	backend := ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	if backend == nil || backend.Name != "shop" || backend.Port.Name != "http" {
		t.Fatalf("route backend = %v, want the shop service's http port", backend)
	}
	// No role or visibility fact reaches the route: nothing in its metadata or
	// rules names the session, the role or the member list, and no auth binding
	// appears that was not in the endpoint's spec (C-59, C-46).
	for key := range ingress.Annotations {
		switch key {
		case "nginx.ingress.kubernetes.io/auth-url", "nginx.ingress.kubernetes.io/auth-signin":
			t.Fatalf("the route carries an auth binding the endpoint did not declare: %s", key)
		}
	}
	if _, found := ingress.GetLabels()[markers.ManagedBy]; !found {
		t.Fatal("the route lost its attribution")
	}
}
