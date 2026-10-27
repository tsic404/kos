// Package tc79 verifies that deleting a system component another object
// depends on is refused: the component stays visible, carries a blocked
// condition that lists every dependent, and only leaves once nothing refers to
// it. A component nothing depends on is removed (AC-40, TC-79; C-17, C-46,
// C-48).
package tc79

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC79DependentComponentDeletionIsBlocked(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))
	reconciler := endpoint.NewReconciler(cluster.Writer(), nil)

	// Two endpoints name the issuer component in their spec; a third omits the
	// reference, so its dependency edge comes from the issuer its status
	// resolved (C-46). A fourth names the component that is deleted before the
	// gate observes it, so that component does have a dependent.
	cluster.Create(t, harness.Endpoint("site-a", harness.ManagedLabels(), tlsShape("a.test", "issuer")))
	cluster.Create(t, harness.Endpoint("site-b", harness.ManagedLabels(), tlsShape("b.test", "issuer")))
	cluster.Create(t, harness.Endpoint("auto", harness.ManagedLabels(), tlsShape("auto.test", "")))
	cluster.Create(t, harness.Endpoint("held-site", harness.ManagedLabels(), tlsShape("held.test", "unwatched")))
	for _, name := range []string{"site-a", "site-b", "auto", "held-site"} {
		harness.ReconcileEndpoint(t, reconciler, name)
	}
	if issuer := statusIssuer(t, cluster, "auto"); issuer != endpoint.DefaultIssuer {
		t.Fatalf("auto recorded issuer %q, want the built-in issuer", issuer)
	}

	artifacts := extension.MemoryArtifacts{}
	for _, name := range []string{"issuer", endpoint.DefaultIssuer, "unused"} {
		cluster.Create(t, componentObject(name))
	}
	// A component deleted before the gate ever observed it cannot be held: the
	// API server refuses new finalizers on a deleting object. Another finalizer
	// keeps it visible so the case can read what the gate does with it.
	createHeldComponent(t, cluster, "unwatched")
	cluster.DeleteComponent(t, "unwatched")
	installation := cluster.ExtensionState(t, artifacts)

	// The component reconciler puts the deletion gate on every managed
	// component, so a deletion is observed as a state instead of vanishing.
	for _, name := range []string{"issuer", endpoint.DefaultIssuer, "unused"} {
		requireFinalizer(t, cluster, name)
	}
	// A dependent references it, yet the unobserved deletion is not claimed as
	// blocked and leaves the load set.
	requireMissingCertificate(t, cluster, "held-site", "unwatched")
	if _, present := installation.Component("unwatched"); present {
		t.Fatal("a component the gate cannot hold stayed in the load set")
	}
	if condition, found := harness.FindCondition(t, cluster, "SystemComponent", "unwatched", extension.ConditionDeprovisionBlocked); found {
		t.Fatalf("a component the gate cannot hold reports %s: %v", extension.ConditionDeprovisionBlocked, condition)
	}
	releaseHeldComponent(t, cluster, "unwatched")
	EventuallyGone(t, cluster, "unwatched")

	cluster.DeleteComponent(t, "issuer")
	cluster.DeleteComponent(t, endpoint.DefaultIssuer)
	cluster.DeleteComponent(t, "unused")
	cluster.ExtensionState(t, artifacts)

	// The depended-on components stay, and each blocked condition lists its
	// dependents one by one.
	requireBlocked(t, cluster, "issuer", "default/site-a", "default/site-b")
	requireBlocked(t, cluster, endpoint.DefaultIssuer, "default/auto")

	// The component nothing depends on is gone.
	EventuallyGone(t, cluster, "unused")

	// Removing the dependents releases the blocked components.
	for _, name := range []string{"site-a", "site-b", "auto"} {
		harness.Delete(t, cluster, "Endpoint", name)
	}
	cluster.ExtensionState(t, artifacts)
	for _, name := range []string{"issuer", endpoint.DefaultIssuer} {
		EventuallyGone(t, cluster, name)
	}
}

// componentObject is a managed system component without a package, so the case
// exercises the gate rather than the install pipeline.
func componentObject(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: harness.Component(name, "", "")}
}

// createHeldComponent creates a managed component another finalizer keeps
// visible, so it can be deleted before the gate observes it.
func createHeldComponent(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	object := componentObject(name)
	object.SetFinalizers([]string{"acceptance.kos.tsic.top/hold"})
	cluster.Create(t, object)
}

// releaseHeldComponent drops the foreign finalizer that keeps a deleted
// component visible.
func releaseHeldComponent(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	object := cluster.Get(t, "v1beta1", "SystemComponent", name)
	object.SetFinalizers(nil)
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "SystemComponent", "").Update(context.Background(), object, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("release %s: %v", name, err)
	}
}

// statusIssuer reads the issuer an endpoint status recorded (C-46).
func statusIssuer(t *testing.T, cluster *harness.Cluster, name string) string {
	t.Helper()
	object := cluster.Get(t, "v1beta1", "Endpoint", name)
	issuer, _, err := unstructured.NestedString(object.Object, "status", "certificate", "issuer")
	if err != nil {
		t.Fatalf("read issuer of %s: %v", name, err)
	}
	return issuer
}

// requireMissingCertificate requires an endpoint whose issuer wrote nothing:
// the certificate state is unready and still names the component it points at.
func requireMissingCertificate(t *testing.T, cluster *harness.Cluster, name, issuer string) {
	t.Helper()
	object := cluster.Get(t, "v1beta1", "Endpoint", name)
	ready, _, err := unstructured.NestedBool(object.Object, "status", "certificate", "ready")
	if err != nil {
		t.Fatalf("read certificate of %s: %v", name, err)
	}
	if ready {
		t.Fatalf("%s reports a ready certificate, want a missing one", name)
	}
	if got := statusIssuer(t, cluster, name); got != issuer {
		t.Fatalf("%s recorded issuer %q, want %q", name, got, issuer)
	}
	condition := harness.RequireCondition(t, cluster, "Endpoint", name, endpoint.ConditionCertificateReady)
	if condition.Status != metav1.ConditionFalse || condition.Reason != endpoint.ReasonCertificateMissing {
		t.Fatalf("%s CertificateReady = %s/%s, want False/%s", name, condition.Status, condition.Reason, endpoint.ReasonCertificateMissing)
	}
}

// requireFinalizer requires the gate on a component.
func requireFinalizer(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	object := cluster.Get(t, "v1beta1", "SystemComponent", name)
	for _, finalizer := range object.GetFinalizers() {
		if finalizer == extension.ComponentFinalizer {
			return
		}
	}
	t.Fatalf("component %s carries no deletion gate: %v", name, object.GetFinalizers())
}

// requireBlocked requires a component that stayed and lists its dependents.
func requireBlocked(t *testing.T, cluster *harness.Cluster, name string, dependents ...string) {
	t.Helper()
	object := cluster.Get(t, "v1beta1", "SystemComponent", name)
	if object.GetDeletionTimestamp() == nil {
		t.Fatalf("component %s is not being deleted", name)
	}
	condition := harness.RequireCondition(t, cluster, "SystemComponent", name, extension.ConditionDeprovisionBlocked)
	if condition.Status != metav1.ConditionTrue {
		t.Fatalf("%s %s = %s, want True", name, extension.ConditionDeprovisionBlocked, condition.Status)
	}
	for _, dependent := range dependents {
		if !strings.Contains(condition.Message, dependent) {
			t.Fatalf("blocked condition of %s does not list %s: %q", name, dependent, condition.Message)
		}
	}
}

// EventuallyGone requires a component to be removed once nothing blocks it.
func EventuallyGone(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	harness.Eventually(t, 15*time.Second, "component "+name+" to be removed", func() bool {
		_, err := cluster.GetObject(context.Background(), v1beta1.GroupVersion.Version, "SystemComponent", "", name)
		return apierrors.IsNotFound(err)
	})
}

// tlsShape builds an HTTP host endpoint with TLS; an empty issuer omits the
// reference so the reconciler resolves the built-in issuer.
func tlsShape(host, issuer string) map[string]any {
	tls := map[string]any{}
	if issuer != "" {
		tls["issuerRef"] = issuer
	}
	return map[string]any{
		"kind": "http.host", "targetApp": "web", "host": host, "tls": tls,
	}
}
