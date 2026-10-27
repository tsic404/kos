// Package tc6 verifies that a missing dependency activates nothing and lists
// the missing edges, and that a controller that keeps failing makes only the
// objects it manages unobservable (AC-2, TC-6).
package tc6

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// failingGroup and healthyGroup keep the two extensions out of each other's
// classes.
const (
	failingGroup = "failing.example.test"
	healthyGroup = "healthy.example.test"
)

// requestTimeout bounds every direct cluster call of the case.
const requestTimeout = 30 * time.Second

// brokenDeclaration declares a capability and a component that do not exist.
func brokenDeclaration() extension.Declaration {
	declaration := extension.Declaration{
		Identifier: "demo.broken",
		Version:    "1.0.0",
		APIGroup:   failingGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: failingGroup, Kind: "Widget", Resource: "widgets", Versions: []string{"v1"},
			}},
			Capabilities: extension.CapabilityContributions{Items: []extension.Capability{{Name: "demo.broken/panel", Version: "v1"}}},
			Pages:        []extension.Page{{ID: "broken", Title: "Broken"}},
			Settings:     []extension.Setting{{ID: "broken-endpoint", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser}},
		},
	}
	declaration.Dependencies = extension.Dependencies{
		Capabilities: []extension.CapabilityRequirement{{Name: "demo.absent/api", Versions: ">=v2"}},
		Components:   []extension.ComponentRequirement{{Identifier: "demo.absent"}},
	}
	return declaration
}

// controlledDeclaration declares one class with a controller that serves it.
func controlledDeclaration(identifier, group, kind, plural, controllerID string) extension.Declaration {
	return extension.Declaration{
		Identifier: identifier,
		Version:    "1.0.0",
		APIGroup:   group,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: group, Kind: kind, Resource: plural, Versions: []string{"v1"}, Namespaced: true,
			}},
			Controllers: []extension.Controller{{
				ID: controllerID, Manages: []extension.GroupKind{{Group: group, Kind: kind}},
			}},
		},
	}
}

// TestTC6MissingDependencyAndControllerFailure covers both halves of the case.
func TestTC6MissingDependencyAndControllerFailure(t *testing.T) {
	cluster := harness.Start(t)
	artifacts := extension.MemoryArtifacts{}

	t.Run("a missing dependency activates nothing and lists the edges", func(t *testing.T) {
		installation := cluster.InstallPackage(t, "broken", brokenDeclaration(), artifacts)
		result, found := installation.Component("broken")
		if !found {
			t.Fatal("the installed component is missing from the pass")
		}
		if result.Phase != extension.PhaseUnavailable || result.Reason != extension.ReasonDependencyMissing {
			t.Fatalf("result = %s/%s, want unavailable on a dependency", result.Phase, result.Reason)
		}
		want := []string{"capability demo.absent/api >=v2", "component demo.absent any"}
		if !reflect.DeepEqual(result.Missing, want) {
			t.Fatalf("missing = %v, want %v", result.Missing, want)
		}
		registry := installation.Registry()
		if registry.IsServing(failingGroup, "Widget") {
			t.Fatal("an unavailable component still serves its object class")
		}
		if len(registry.Pages()) != 0 || len(registry.Settings()) != 0 || len(registry.ExtensionCapabilities()) != 0 {
			t.Fatal("an unavailable component activated a contribution")
		}

		status := cluster.ComponentStatus(t, "broken")
		if phase := harness.String(status, "phase"); phase != extension.PhaseUnavailable {
			t.Fatalf("stored phase = %q", phase)
		}
		if health := harness.String(status, "health", "state"); health != "Unavailable" {
			t.Fatalf("stored health = %q", health)
		}
		conditions := harness.Conditions(t, cluster.Get(t, "v1beta1", "SystemComponent", "broken"))
		reconciled, _ := conditionOf(conditions, api.ConditionReconciled)
		if reconciled.Status != metav1.ConditionFalse || reconciled.Reason != extension.ReasonDependencyMissing {
			t.Fatalf("Reconciled = %+v", reconciled)
		}
		dependencies, _ := conditionOf(conditions, extension.ConditionDependenciesMet)
		if dependencies.Status != metav1.ConditionFalse {
			t.Fatalf("DependenciesMet = %+v", dependencies)
		}
		for _, item := range want {
			if !strings.Contains(dependencies.Message, item) {
				t.Errorf("DependenciesMet message %q does not list %q", dependencies.Message, item)
			}
		}

		control := cluster.StartControlPlane(t, func(config *controlplane.Config) {
			config.Extensions = registry
		})
		statusCode, body := control.Call(t, http.MethodGet,
			"/api/v1/resources/Widget?apiVersion="+failingGroup+"/v1&namespace="+harness.Namespace, nil, nil)
		if statusCode != http.StatusUnprocessableEntity || harness.String(body, "error", "code") != controlplane.CodeCapabilityMissing {
			t.Fatalf("read the unavailable component's class: status %d, body %v", statusCode, body)
		}
	})

	t.Run("one failing controller leaves other objects reconciling", func(t *testing.T) {
		cluster.InstallCRD(t, failingGroup, "v1", "Widget", "widgets", true)
		cluster.InstallCRD(t, healthyGroup, "v1", "Thing", "things", true)
		cluster.InstallPackage(t, "flaky", controlledDeclaration("demo.flaky", failingGroup, "Widget", "widgets", "flaky"), artifacts)
		installation := cluster.InstallPackage(t, "steady", controlledDeclaration("demo.steady", healthyGroup, "Thing", "things", "steady"), artifacts)
		if !installation.Registry().IsServing(failingGroup, "Widget") {
			t.Fatal("the active component's class stopped being served")
		}
		if !installation.Registry().IsServing(healthyGroup, "Thing") {
			t.Fatal("the second active component's class stopped being served")
		}

		kernelApplication := harness.Application("steady", nil)
		kernelApplication.Object["status"] = harness.ObjectStatus()
		stored := cluster.Create(t, kernelApplication)
		kernelStatus, _, err := unstructured.NestedMap(stored.Object, "status")
		if err != nil {
			t.Fatalf("read kernel status: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		for _, class := range []struct{ group, kind, plural string }{
			{failingGroup, "Widget", "widgets"},
			{healthyGroup, "Thing", "things"},
		} {
			resource := cluster.ExtensionResource(class.group, "v1", class.plural, harness.Namespace)
			if _, err := resource.Create(ctx, extensionObject(class.group, class.kind, "one"), metav1.CreateOptions{}); err != nil {
				t.Fatalf("store %s %s: %v", class.kind, "one", err)
			}
		}

		failing := functionReconciler(func(context.Context, reconcile.Request) (reconcile.Result, error) {
			return reconcile.Result{}, errors.New("controller keeps failing")
		})
		var healthyRounds atomic.Int64
		healthy := functionReconciler(func(context.Context, reconcile.Request) (reconcile.Result, error) {
			healthyRounds.Add(1)
			return reconcile.Result{}, nil
		})
		controllers := extension.NewControllerSet(2)
		if err := controllers.Register("flaky", extension.Controller{
			ID: "flaky", Manages: []extension.GroupKind{{Group: failingGroup, Kind: "Widget"}},
		}, failing); err != nil {
			t.Fatalf("register failing controller: %v", err)
		}
		if err := controllers.Register("steady", extension.Controller{
			ID: "steady", Manages: []extension.GroupKind{{Group: healthyGroup, Kind: "Thing"}},
		}, healthy); err != nil {
			t.Fatalf("register healthy controller: %v", err)
		}
		runContext, stop := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			controllers.Run(runContext)
		}()
		defer func() {
			stop()
			<-done
		}()

		if err := controllers.Enqueue("flaky", "flaky", reconcile.Request{Kind: "Widget", Namespace: harness.Namespace, Name: "one"}); err != nil {
			t.Fatalf("enqueue failing controller: %v", err)
		}
		waitFor(t, "the failing controller to become unavailable", func() bool {
			observed, ok := controllers.State("flaky", "flaky")
			return ok && !observed.IsAvailable
		})

		// Read through the control plane: only the failing controller's class is
		// projected Unknown; the other extension and the kernel keep their
		// stored status (C-20).
		control := cluster.StartControlPlane(t, func(config *controlplane.Config) {
			config.Extensions = installation.Registry()
			config.Controllers = controllers
		})
		_, listed := control.Call(t, http.MethodGet,
			"/api/v1/resources/Widget?apiVersion="+failingGroup+"/v1&namespace="+harness.Namespace, nil, nil)
		items := harness.Items(t, listed)
		if len(items) != 1 {
			t.Fatalf("listed %d objects of the failing class, want 1", len(items))
		}
		unobservable := objectStatus(t, items[0])
		if unobservable.Health.State != v1beta1.HealthStateUnknown {
			t.Fatalf("listed health = %s, want Unknown", unobservable.Health.State)
		}
		if reconciled, ok := conditionOf(unobservable.Conditions, api.ConditionReconciled); !ok || reconciled.Status != metav1.ConditionUnknown {
			t.Fatalf("listed Reconciled = %+v, want Unknown", reconciled)
		}

		_, read := control.Call(t, http.MethodGet,
			"/api/v1/resources/Widget/one?apiVersion="+failingGroup+"/v1&namespace="+harness.Namespace, nil, nil)
		if observed := objectStatus(t, read); observed.Health.State != v1beta1.HealthStateUnknown {
			t.Fatalf("read health = %s, want Unknown", observed.Health.State)
		}
		_, other := control.Call(t, http.MethodGet,
			"/api/v1/resources/Thing/one?apiVersion="+healthyGroup+"/v1&namespace="+harness.Namespace, nil, nil)
		if observed := objectStatus(t, other); observed.Health.State != v1beta1.HealthStateHealthy {
			t.Fatalf("another extension's object = %s, want Healthy", observed.Health.State)
		}
		_, kernelRead := control.Call(t, http.MethodGet,
			"/api/v1/resources/Application/steady?namespace="+harness.Namespace, nil, nil)
		if observed := objectStatus(t, kernelRead); observed.Health.State != v1beta1.HealthStateHealthy {
			t.Fatalf("a kernel object became %s", observed.Health.State)
		}

		// The projection is a read-time view: the stored Widget status is still
		// what its controller wrote.
		storedWidget, err := cluster.ExtensionResource(failingGroup, "v1", "widgets", harness.Namespace).
			Get(ctx, "one", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("re-read the stored Widget: %v", err)
		}
		if observed := harness.String(storedWidget.Object, "status", "health", "state"); observed != string(v1beta1.HealthStateHealthy) {
			t.Fatalf("the projection wrote the stored health: %s", observed)
		}

		// The healthy controller keeps working after the other one failed.
		before := healthyRounds.Load()
		if err := controllers.Enqueue("steady", "steady", reconcile.Request{Kind: "Thing", Namespace: harness.Namespace, Name: "one"}); err != nil {
			t.Fatalf("enqueue healthy controller: %v", err)
		}
		waitFor(t, "the healthy controller to keep reconciling", func() bool {
			return healthyRounds.Load() > before
		})

		// The kernel object's stored status is untouched.
		after := cluster.Get(t, "v1beta1", "Application", "steady")
		afterStatus, _, err := unstructured.NestedMap(after.Object, "status")
		if err != nil {
			t.Fatalf("read kernel status after the failure: %v", err)
		}
		if !reflect.DeepEqual(kernelStatus, afterStatus) {
			t.Fatalf("a kernel object's status changed: %v -> %v", kernelStatus, afterStatus)
		}
	})
}

// functionReconciler adapts a function to the reconcile interface.
type functionReconciler func(context.Context, reconcile.Request) (reconcile.Result, error)

// Reconcile runs the function.
func (f functionReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	return f(ctx, request)
}

// waitFor polls a condition with a deadline.
func waitFor(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// extensionObject builds one stored extension object carrying the healthy
// status its own controller wrote.
func extensionObject(group, kind, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/v1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec":       map[string]any{},
		"status":     harness.ObjectStatus(),
	}}
}

// objectStatus decodes the status a projection carries for one object.
func objectStatus(t *testing.T, projection map[string]any) v1beta1.ObjectStatus {
	t.Helper()
	encoded, err := json.Marshal(harness.Nested(projection, "object", "status"))
	if err != nil {
		t.Fatalf("encode projected status: %v", err)
	}
	status := v1beta1.ObjectStatus{}
	if err := json.Unmarshal(encoded, &status); err != nil {
		t.Fatalf("decode projected status: %v", err)
	}
	return status
}

// conditionOf returns the condition of one type.
func conditionOf(conditions []metav1.Condition, conditionType string) (metav1.Condition, bool) {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}
