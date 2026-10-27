// Package tc125 verifies C-67's materialization and override rules: package
// defaults land in the application spec and on the workload at installation, an
// application that declares nothing is projected as unlimited with the
// namespace LimitRange named as the remaining source, and an override writes
// only the submitted field (AC-62, TC-125).
package tc125

import (
	"context"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	targetNamespace = "apps"
	defaultedName   = "defaulted"
	undeclaredName  = "undeclared"
)

// deploymentTemplate renders one Deployment with no container resources, so the
// application spec is the only source of a request or a limit.
const deploymentTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ .Name }}
  template:
    metadata:
      labels:
        app: {{ .Name }}
    spec:
      containers:
        - name: app
          image: nginx:1.27
`

// packageDefaults is what the defaulting package declares.
func packageDefaults() *v1beta1.ResourceRequirements {
	return &v1beta1.ResourceRequirements{
		Requests: v1beta1.ResourceList{CPU: "100m", Memory: "128Mi"},
		Limits:   v1beta1.ResourceList{CPU: "500m", Memory: "512Mi"},
	}
}

func TestTC125DefaultsAndOverride(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "with-defaults", deploymentTemplate, packageDefaults()))
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "without-defaults", deploymentTemplate, nil))
	createLimitRange(t, cluster)

	gateway := cluster.StartControlPlane(t, nil)
	for name, definition := range map[string]string{defaultedName: "with-defaults", undeclaredName: "without-defaults"} {
		manifest := harness.ApplicationManifest(name, harness.Namespace, definition, targetNamespace, nil, nil)
		if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
			t.Fatalf("create %s: status %d, body %v", name, status, body)
		}
	}

	// The defaults are part of the application spec once installed (C-67).
	stored := resourcesOf(t, cluster.Get(t, "v1beta1", "Application", defaultedName))
	if stored.CPU != "100m" || stored.Memory != "128Mi" {
		t.Fatalf("materialized requests = %+v, want 100m/128Mi", stored)
	}
	if limits := limitsOf(t, cluster.Get(t, "v1beta1", "Application", defaultedName)); limits.CPU != "500m" || limits.Memory != "512Mi" {
		t.Fatalf("materialized limits = %+v, want 500m/512Mi", limits)
	}
	if _, found, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", undeclaredName).Object, "spec", "resources"); found {
		t.Fatal("an application from a package without defaults carries resources")
	}

	controller := cluster.ApplicationController(t, app.Options{})
	for _, name := range []string{defaultedName, undeclaredName} {
		reconcileOnce(t, controller, name)
		reconcileAppliedRound(t, controller, cluster, name)
	}

	// The workload carries the same values (C-67).
	limits := containerResources(t, cluster, defaultedName)
	if limits["limits"].(map[string]any)["cpu"] != "500m" || limits["limits"].(map[string]any)["memory"] != "512Mi" {
		t.Fatalf("workload limits = %v, want 500m/512Mi", limits["limits"])
	}
	if limits["requests"].(map[string]any)["cpu"] != "100m" || limits["requests"].(map[string]any)["memory"] != "128Mi" {
		t.Fatalf("workload requests = %v, want 100m/128Mi", limits["requests"])
	}

	// The undeclared application is projected as unlimited and names the
	// LimitRange that still applies (C-67).
	resourceStatus, found, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", undeclaredName).Object, "status", "resources")
	if !found {
		t.Fatal("the undeclared application carries no resource projection")
	}
	if unlimited, _ := resourceStatus["unlimited"].(bool); !unlimited {
		t.Fatalf("unlimited = %v, want true", resourceStatus["unlimited"])
	}
	if source, _ := resourceStatus["limitRange"].(string); source != "apps-defaults" {
		t.Fatalf("LimitRange source = %q, want apps-defaults", source)
	}
	if _, present := containerResources(t, cluster, undeclaredName)["limits"]; present {
		t.Fatal("the undeclared package invented a limit on its workload")
	}

	// An override writes only the submitted field; the rest keeps the
	// materialized default (C-67).
	putApplication(t, gateway, cluster, defaultedName, map[string]any{
		"limits": map[string]any{"cpu": "1"},
	})
	overridden := cluster.Get(t, "v1beta1", "Application", defaultedName)
	if got := resourcesOf(t, overridden); got.CPU != "100m" || got.Memory != "128Mi" {
		t.Fatalf("override changed the requests to %+v", got)
	}
	if got := limitsOf(t, overridden); got.CPU != "1" || got.Memory != "512Mi" {
		t.Fatalf("override limits = %+v, want 1/512Mi", got)
	}
	reconcileAppliedRound(t, controller, cluster, defaultedName)
	workload := containerResources(t, cluster, defaultedName)
	if workload["limits"].(map[string]any)["cpu"] != "1" {
		t.Fatalf("workload limits after the override = %v, want cpu 1", workload["limits"])
	}
	if workload["limits"].(map[string]any)["memory"] != "512Mi" {
		t.Fatalf("workload lost the default memory limit: %v", workload["limits"])
	}
	projected, _, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", defaultedName).Object, "status", "resources")
	if projected == nil {
		t.Fatal("the overridden application carries no resource projection")
	}
	projectedLimits, _ := projected["limits"].(map[string]any)
	if projectedLimits["cpu"] != "1" {
		t.Fatalf("projected limit cpu = %v, want 1", projectedLimits["cpu"])
	}
}

// TestTC125NativeInstallKeepsPackageDefaults covers an application created
// through the native interface, where no control-plane write materializes the
// package defaults: installation puts them on the workload without writing the
// user spec, and a later definition update does not reach the application that
// already exists (C-32, C-67).
func TestTC125NativeInstallKeepsPackageDefaults(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "with-defaults", deploymentTemplate, packageDefaults()))

	nativeName := "native"
	manifest := harness.ApplicationManifest(nativeName, harness.Namespace, "with-defaults", targetNamespace, nil, nil)
	cluster.Create(t, &unstructured.Unstructured{Object: manifest})

	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller, nativeName)
	reconcileAppliedRound(t, controller, cluster, nativeName)

	// The workload carries the defaults while the user spec stays untouched.
	workload := containerResources(t, cluster, nativeName)
	requests, _ := workload["requests"].(map[string]any)
	limits, _ := workload["limits"].(map[string]any)
	if textOf(requests, "cpu") != "100m" || textOf(requests, "memory") != "128Mi" {
		t.Fatalf("native workload requests = %v, want the package defaults", requests)
	}
	if textOf(limits, "cpu") != "500m" || textOf(limits, "memory") != "512Mi" {
		t.Fatalf("native workload limits = %v, want the package defaults", limits)
	}
	if _, found, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", nativeName).Object, "spec", "resources"); found {
		t.Fatal("installation wrote the package defaults into the user spec")
	}

	// The first-install defaults are frozen in the status snapshot.
	snapshot, found, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", nativeName).Object, "status", "resourceDefaults")
	if !found {
		t.Fatal("the application carries no frozen default snapshot")
	}
	snapshotRequests, _ := snapshot["requests"].(map[string]any)
	if textOf(snapshotRequests, "cpu") != "100m" || textOf(snapshotRequests, "memory") != "128Mi" {
		t.Fatalf("frozen requests = %v, want the first-install defaults", snapshotRequests)
	}

	// A definition update leaves the workload on the frozen defaults (C-32).
	changed := packageDefaults()
	changed.Requests.CPU = "250m"
	changed.Limits.CPU = "2"
	updateDefinitionResources(t, cluster, "with-defaults", changed)
	// The definition really moved, so the values below are evidence of C-32 and
	// not of an update that never landed.
	published, _, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "ApplicationDefinition", "with-defaults").Object,
		"spec", "resources", "requests")
	if textOf(published, "cpu") != "250m" {
		t.Fatalf("the definition update did not land: requests %v", published)
	}
	reconcileAppliedRound(t, controller, cluster, nativeName)
	workload = containerResources(t, cluster, nativeName)
	requests, _ = workload["requests"].(map[string]any)
	limits, _ = workload["limits"].(map[string]any)
	if textOf(requests, "cpu") != "100m" || textOf(limits, "cpu") != "500m" {
		t.Fatalf("a definition update reached the existing workload: requests %v, limits %v", requests, limits)
	}
}

// TestTC125NativeApplicationRefusesALimitBelowTheFrozenDefault covers C-67 on
// the path where the defaults never entered the user spec: an application
// created through the native interface carries its package defaults in the
// status snapshot alone, and a partial API override is judged against that
// snapshot, so a limit below the frozen request is refused before the write.
func TestTC125NativeApplicationRefusesALimitBelowTheFrozenDefault(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "with-defaults", deploymentTemplate, packageDefaults()))

	name := "native-override"
	cluster.Create(t, &unstructured.Unstructured{Object: harness.ApplicationManifest(name, harness.Namespace, "with-defaults", targetNamespace, nil, nil)})
	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller, name)
	reconcileAppliedRound(t, controller, cluster, name)
	if _, found, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", name).Object, "spec", "resources"); found {
		t.Fatal("installation wrote the package defaults into the user spec")
	}

	gateway := cluster.StartControlPlane(t, nil)

	// The stored spec declares no request, yet the workload keeps the frozen
	// 100m: a 50m limit would make it invalid, so the write is refused and names
	// the field (C-67).
	status, body := submitResources(t, gateway, cluster, name, map[string]any{"limits": map[string]any{"cpu": "50m"}})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %v", status, body)
	}
	failure, _ := body["error"].(map[string]any)
	if failure["code"] != "invalid_field" {
		t.Fatalf("code = %v, want invalid_field", failure["code"])
	}
	if message, _ := failure["message"].(string); !strings.Contains(message, "spec.resources.limits.cpu") {
		t.Fatalf("message %q does not name spec.resources.limits.cpu", message)
	}
	if _, found, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", name).Object, "spec", "resources"); found {
		t.Fatal("the refused write stored the limit")
	}

	// A limit above the frozen request is accepted and only the submitted field
	// is written; the requests keep coming from the frozen snapshot (C-67).
	if status, body := submitResources(t, gateway, cluster, name, map[string]any{"limits": map[string]any{"cpu": "1"}}); status != http.StatusAccepted {
		t.Fatalf("override: status %d, body %v", status, body)
	}
	storedResources, _, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", name).Object, "spec", "resources")
	if requests, _ := storedResources["requests"].(map[string]any); len(requests) != 0 {
		t.Fatalf("the override materialized the frozen requests into the spec: %v", requests)
	}
	if limits, _ := storedResources["limits"].(map[string]any); textOf(limits, "cpu") != "1" {
		t.Fatalf("stored limits = %v, want the submitted cpu 1", storedResources["limits"])
	}
	reconcileAppliedRound(t, controller, cluster, name)
	workload := containerResources(t, cluster, name)
	requests, _ := workload["requests"].(map[string]any)
	limits, _ := workload["limits"].(map[string]any)
	if textOf(requests, "cpu") != "100m" || textOf(requests, "memory") != "128Mi" {
		t.Fatalf("workload requests = %v, want the frozen package defaults", requests)
	}
	if textOf(limits, "cpu") != "1" || textOf(limits, "memory") != "512Mi" {
		t.Fatalf("workload limits = %v, want the submitted cpu over the default memory", limits)
	}
}

// TestTC125OverrideBeforeTheFirstRoundIsJudgedAgainstTheDefinition covers the
// window before an installation freezes the package defaults: an application
// created through the native interface has no snapshot and no spec value yet, so
// the definition is the only source of the request, and a limit below it is still
// refused before the write (C-67).
func TestTC125OverrideBeforeTheFirstRoundIsJudgedAgainstTheDefinition(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "with-defaults", deploymentTemplate, packageDefaults()))

	name := "pending-override"
	cluster.Create(t, &unstructured.Unstructured{Object: harness.ApplicationManifest(name, harness.Namespace, "with-defaults", targetNamespace, nil, nil)})
	gateway := cluster.StartControlPlane(t, nil)

	// No round has run, so neither the status snapshot nor the spec carries the
	// 100m request the installation will converge to.
	stored := cluster.Get(t, "v1beta1", "Application", name)
	if _, found, _ := unstructured.NestedMap(stored.Object, "status", "resourceDefaults"); found {
		t.Fatal("an unreconciled application already carries a frozen default snapshot")
	}
	status, body := submitResources(t, gateway, cluster, name, map[string]any{"limits": map[string]any{"cpu": "50m"}})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %v", status, body)
	}
	failure, _ := body["error"].(map[string]any)
	if failure["code"] != "invalid_field" {
		t.Fatalf("code = %v, want invalid_field", failure["code"])
	}
	if message, _ := failure["message"].(string); !strings.Contains(message, "spec.resources.limits.cpu") {
		t.Fatalf("message %q does not name spec.resources.limits.cpu", message)
	}
	if _, found, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", name).Object, "spec", "resources"); found {
		t.Fatal("the refused write stored the limit")
	}

	// A limit above the package request is accepted while the application is
	// still Pending, and the first installation converges the frozen defaults
	// beside it.
	if status, body := submitResources(t, gateway, cluster, name, map[string]any{"limits": map[string]any{"cpu": "1"}}); status != http.StatusAccepted {
		t.Fatalf("override: status %d, body %v", status, body)
	}
	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller, name)
	reconcileAppliedRound(t, controller, cluster, name)
	workload := containerResources(t, cluster, name)
	requests, _ := workload["requests"].(map[string]any)
	limits, _ := workload["limits"].(map[string]any)
	if textOf(requests, "cpu") != "100m" || textOf(requests, "memory") != "128Mi" {
		t.Fatalf("workload requests = %v, want the package defaults", requests)
	}
	if textOf(limits, "cpu") != "1" || textOf(limits, "memory") != "512Mi" {
		t.Fatalf("workload limits = %v, want the submitted cpu over the default memory", limits)
	}
}

// TestTC125EmptySnapshotKeepsTheDefinitionOut covers the other side of the
// snapshot boundary: an application installed from a package without defaults
// freezes an empty set, and that frozen set keeps a later definition change out
// of the judgement. The override below carries a limit the definition would call
// too small, and it is accepted because the application never froze that
// request (C-32, C-67).
func TestTC125EmptySnapshotKeepsTheDefinitionOut(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "frozen-empty", deploymentTemplate, nil))

	name := "frozen-empty-app"
	cluster.Create(t, &unstructured.Unstructured{Object: harness.ApplicationManifest(name, harness.Namespace, "frozen-empty", targetNamespace, nil, nil)})
	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller, name)
	reconcileAppliedRound(t, controller, cluster, name)

	// The installation froze an empty set: the snapshot exists, it just declares
	// nothing.
	stored := cluster.Get(t, "v1beta1", "Application", name)
	if _, found, err := unstructured.NestedFieldNoCopy(stored.Object, "status", "resourceDefaults"); err != nil || !found {
		t.Fatalf("the installation froze no default snapshot: found %v (%v)", found, err)
	}

	// The catalogue then publishes the same package with defaults; an application
	// that already exists keeps the set it froze (C-32).
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "frozen-empty", deploymentTemplate, packageDefaults()))

	// The definition now asks for 100m, but this application froze nothing, so a
	// 50m limit is legal for it and the write is accepted.
	gateway := cluster.StartControlPlane(t, nil)
	if status, body := submitResources(t, gateway, cluster, name, map[string]any{"limits": map[string]any{"cpu": "50m"}}); status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %v", status, body)
	}
	reconcileAppliedRound(t, controller, cluster, name)
	workload := containerResources(t, cluster, name)
	if requests, present := workload["requests"]; present {
		t.Fatalf("the definition defaults reached the workload: requests %v", requests)
	}
	limits, _ := workload["limits"].(map[string]any)
	if textOf(limits, "cpu") != "50m" {
		t.Fatalf("workload limits = %v, want the submitted cpu 50m", limits)
	}
}

// updateDefinitionResources rewrites the package default set as a catalogue
// update would, leaving the rest of the definition alone. The package digest
// covers the whole definition spec, so it is recomputed with the new values: a
// stale digest makes the definition invalid, and the round that follows would
// fail before rendering anything instead of showing what the update does to an
// installed application (C-32).
func updateDefinitionResources(t *testing.T, cluster *harness.Cluster, name string, resources *v1beta1.ResourceRequirements) {
	t.Helper()
	stored := cluster.Get(t, "v1beta1", "ApplicationDefinition", name)
	definition := &v1beta1.ApplicationDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, definition); err != nil {
		t.Fatalf("decode definition %q: %v", name, err)
	}
	definition.Spec.Resources = resources
	digest, err := app.DefinitionDigest(definition.Spec)
	if err != nil {
		t.Fatalf("digest definition %q: %v", name, err)
	}
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(resources)
	if err != nil {
		t.Fatalf("encode the definition resources: %v", err)
	}
	if err := unstructured.SetNestedMap(stored.Object, encoded, "spec", "resources"); err != nil {
		t.Fatalf("write the definition resources: %v", err)
	}
	if err := unstructured.SetNestedField(stored.Object, digest, "spec", "package", "digest"); err != nil {
		t.Fatalf("write the definition digest: %v", err)
	}
	resource := cluster.Dynamic.Resource(schema.GroupVersionResource{
		Group: api.Group, Version: "v1beta1", Resource: "applicationdefinitions",
	})
	if _, err := resource.Update(context.Background(), stored, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update definition %q: %v", name, err)
	}
}

// putApplication rewrites the resources of one application through the control
// plane, with the stored version as the precondition.
func putApplication(t *testing.T, gateway *harness.ControlPlane, cluster *harness.Cluster, name string, resources map[string]any) {
	t.Helper()
	if status, response := submitResources(t, gateway, cluster, name, resources); status != http.StatusAccepted {
		t.Fatalf("override %s: status %d, body %v", name, status, response)
	}
}

// submitResources submits one resource override through the control plane and
// returns the answer, with the stored version as the precondition.
func submitResources(t *testing.T, gateway *harness.ControlPlane, cluster *harness.Cluster, name string, resources map[string]any) (int, map[string]any) {
	t.Helper()
	stored := cluster.Get(t, "v1beta1", "Application", name)
	spec, _, _ := unstructured.NestedMap(stored.Object, "spec")
	spec["resources"] = resources
	metadata := map[string]any{"name": name, "namespace": harness.Namespace, "resourceVersion": stored.GetResourceVersion()}
	if labels := stored.GetLabels(); len(labels) > 0 {
		encoded := map[string]any{}
		for key, value := range labels {
			encoded[key] = value
		}
		metadata["labels"] = encoded
	}
	body := map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   metadata,
		"spec":       spec,
	}
	return gateway.Call(t, http.MethodPut, "/api/v1/resources/Application/"+name+"?namespace="+harness.Namespace, body, nil)
}

// createLimitRange installs the namespace LimitRange whose defaults still apply
// to an application that declares nothing.
func createLimitRange(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	limitRange := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "LimitRange",
		"metadata":   map[string]any{"name": "apps-defaults", "namespace": targetNamespace},
		"spec": map[string]any{"limits": []any{map[string]any{
			"type":           "Container",
			"default":        map[string]any{"cpu": "200m", "memory": "256Mi"},
			"defaultRequest": map[string]any{"cpu": "50m", "memory": "64Mi"},
		}}},
	}}
	if _, err := cluster.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "limitranges"}).
		Namespace(targetNamespace).Create(context.Background(), limitRange, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the LimitRange: %v", err)
	}
}

// resourcesOf reads the stored request set.
func resourcesOf(t *testing.T, object *unstructured.Unstructured) v1beta1.ResourceList {
	t.Helper()
	resources, _, _ := unstructured.NestedMap(object.Object, "spec", "resources", "requests")
	return v1beta1.ResourceList{CPU: textOf(resources, "cpu"), Memory: textOf(resources, "memory")}
}

// limitsOf reads the stored limit set.
func limitsOf(t *testing.T, object *unstructured.Unstructured) v1beta1.ResourceList {
	t.Helper()
	limits, _, _ := unstructured.NestedMap(object.Object, "spec", "resources", "limits")
	return v1beta1.ResourceList{CPU: textOf(limits, "cpu"), Memory: textOf(limits, "memory")}
}

// textOf reads one value of a decoded map.
func textOf(values map[string]any, key string) string {
	text, _ := values[key].(string)
	return text
}

// containerResources reads the first container's resource block of one
// application's workload.
func containerResources(t *testing.T, cluster *harness.Cluster, name string) map[string]any {
	t.Helper()
	workload := cluster.Workload(t, "Deployment", targetNamespace, name)
	containers, _, _ := unstructured.NestedSlice(workload.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		t.Fatalf("%s has no container", name)
	}
	resources, _ := containers[0].(map[string]any)["resources"].(map[string]any)
	if resources == nil {
		resources = map[string]any{}
	}
	return resources
}

// reconcileAppliedRound drives one round and requires it to end with the desired
// state applied. A round that fails before writing anything — an invalid package,
// a rejected config — returns no error and leaves the downstream objects as they
// were, so a "nothing changed" assertion after it would hold for the wrong
// reason.
func reconcileAppliedRound(t *testing.T, controller *app.Controller, cluster *harness.Cluster, name string) {
	t.Helper()
	reconcileOnce(t, controller, name)
	condition, present := state.Find(harness.Conditions(t, cluster.Get(t, "v1beta1", "Application", name)), api.ConditionReconciled)
	if !present || condition.Status != metav1.ConditionTrue {
		t.Fatalf("%s: the round applied nothing: Reconciled = %+v", name, condition)
	}
}

// reconcileOnce drives one application round.
func reconcileOnce(t *testing.T, controller *app.Controller, name string) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: harness.Namespace, Name: name,
	}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}
