// Package tc123 verifies C-66's stop and start shape: a stop keeps the replica
// count, volumes and route in place while the workload goes to zero, health
// stays Healthy and the phase stays where it was; starting restores the count
// and clears the endpoint's paused condition (AC-61, TC-123).
package tc123

import (
	"context"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	targetNamespace = "apps"
	applicationName = "web"
)

// lifecycleTemplate renders a Deployment that mounts the declared volume and a
// Service in front of it. Lifecycle cases use it where the package content is
// not itself under test.
const lifecycleTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: 2
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
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: {{ index .Volumes "data" }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  selector:
    app: {{ .Name }}
  ports:
    - port: 80
      targetPort: 80
`

func TestTC123StopAndStart(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	claim := createClaim(t, cluster, targetNamespace, "data")
	definition := harness.TemplateDefinition(t, "lifecycle", lifecycleTemplate, nil)
	cluster.InstallDefinition(t, definition)

	gateway := cluster.StartControlPlane(t, nil)
	manifest := harness.ApplicationManifest(applicationName, harness.Namespace, "lifecycle", targetNamespace, nil, []string{"data"})
	manifest["spec"].(map[string]any)["replicas"] = int64(2)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
		t.Fatalf("create the application: status %d, body %v", status, body)
	}

	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)
	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q", phase, app.PhaseReady)
	}
	if got := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, applicationName); got != 2 {
		t.Fatalf("running replicas = %d, want 2", got)
	}
	cluster.SetWorkloadStatus(t, "Deployment", targetNamespace, applicationName, 2, 2)
	reconcileOnce(t, controller)

	routes := newRouteReconciler(cluster)
	cluster.CreateIn(t, harness.Namespace, harness.EndpointIn(harness.Namespace, applicationName, harness.ManagedLabels(), map[string]any{
		"kind": "http.host", "targetApp": applicationName, "host": "web.example.test",
	}))
	harness.ReconcileEndpointIn(t, routes, harness.Namespace, applicationName)
	if harness.IngressOf(t, cluster, applicationName) == nil {
		t.Fatal("the endpoint has no route before the stop")
	}
	if got := healthOf(t, cluster); got != string(v1beta1.HealthStateHealthy) {
		t.Fatalf("running health = %q, want Healthy", got)
	}

	// Stop: the pause marker lands, the replica count does not move (C-66).
	callAction(t, gateway, map[string]any{"action": "stop"})
	stopped := cluster.Get(t, "v1beta1", "Application", applicationName)
	if paused, _, _ := unstructured.NestedBool(stopped.Object, "spec", "paused"); !paused {
		t.Fatal("the stop wrote no pause marker")
	}
	if replicas, _, _ := unstructured.NestedInt64(stopped.Object, "spec", "replicas"); replicas != 2 {
		t.Fatalf("the stop changed the replica count to %d", replicas)
	}
	reconcileOnce(t, controller)

	if got := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, applicationName); got != 0 {
		t.Fatalf("stopped replicas = %d, want 0", got)
	}
	if !claimExists(t, cluster, targetNamespace, "data") {
		t.Fatal("the stop removed the claim")
	}
	if got := claimVersion(t, cluster, targetNamespace, "data"); got != claim.GetResourceVersion() {
		t.Fatalf("the stop rewrote the claim (%s -> %s)", claim.GetResourceVersion(), got)
	}
	if harness.IngressOf(t, cluster, applicationName) == nil {
		t.Fatal("the stop removed the route")
	}
	if !serviceExists(t, cluster, targetNamespace, applicationName) {
		t.Fatal("the stop removed the service")
	}
	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("stopped phase = %q, want the pre-stop %q", phase, app.PhaseReady)
	}
	if got := healthOf(t, cluster); got != string(v1beta1.HealthStateHealthy) {
		t.Fatalf("stopped health = %q, want Healthy", got)
	}
	harness.ReconcileEndpointIn(t, routes, harness.Namespace, applicationName)
	requireCondition(t, cluster, endpoint.ConditionApplicationPaused, metav1.ConditionTrue)

	// Start: the same replica count is restored and the route is reachable
	// again (C-66).
	callAction(t, gateway, map[string]any{"action": "start"})
	if paused, _, _ := unstructured.NestedBool(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "spec", "paused"); paused {
		t.Fatal("the start did not clear the pause marker")
	}
	reconcileOnce(t, controller)
	if got := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, applicationName); got != 2 {
		t.Fatalf("resumed replicas = %d, want 2", got)
	}
	harness.ReconcileEndpointIn(t, routes, harness.Namespace, applicationName)
	if _, found := harness.FindCondition(t, cluster, "Endpoint", applicationName, endpoint.ConditionApplicationPaused); found {
		t.Fatal("the start left the paused condition on the endpoint")
	}
	if got := claimVersion(t, cluster, targetNamespace, "data"); got != claim.GetResourceVersion() {
		t.Fatalf("the start rewrote the claim (%s -> %s)", claim.GetResourceVersion(), got)
	}
	cluster.SetWorkloadStatus(t, "Deployment", targetNamespace, applicationName, 2, 2)
	reconcileOnce(t, controller)
	if got := healthOf(t, cluster); got != string(v1beta1.HealthStateHealthy) {
		t.Fatalf("resumed health = %q, want Healthy", got)
	}
	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("resumed phase = %q, want %q", phase, app.PhaseReady)
	}
}

// versionTemplate renders one Deployment whose annotation names the package
// content, so an upgrade is observable on a workload a stop drove to zero.
func versionTemplate(version string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
  annotations:
    example.tsic/version: "` + version + `"
spec:
  replicas: 2
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
}

// TestTC123StopSurvivesUpgradeAndRollback covers the lifecycle facts across the
// upgrade path: every apply of C-34, including the rollback that follows a
// failed hook, converges the stopped count before it writes, so an upgrade never
// resurrects a workload the user stopped (C-34, C-66).
func TestTC123StopSurvivesUpgradeAndRollback(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)

	base := harness.DefinitionContent(t, "content-one", "data", versionTemplate("one"), nil, nil)
	failing := []v1beta1.Hook{{Name: app.HookUpgrade, Command: []string{"sh", "-c", "exit 1"}}}
	broken := harness.DefinitionContent(t, "content-two", "data", versionTemplate("two"), failing, nil)
	next := harness.DefinitionContent(t, "content-three", "data", versionTemplate("three"), nil, nil)
	cluster.InstallDefinition(t, base)
	cluster.InstallDefinition(t, broken)
	cluster.InstallDefinition(t, next)

	gateway := cluster.StartControlPlane(t, nil)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.ApplicationManifest(applicationName, harness.Namespace, base.Name, targetNamespace, nil, nil), nil); status != http.StatusAccepted {
		t.Fatalf("create the application: status %d, body %v", status, body)
	}
	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)
	if version := workloadContent(t, cluster); version != "one" {
		t.Fatalf("installed content = %q, want %q", version, "one")
	}

	// The application is stopped before the package moves (C-66).
	callAction(t, gateway, map[string]any{"action": "stop"})
	reconcileOnce(t, controller)
	if got := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, applicationName); got != 0 {
		t.Fatalf("stopped replicas = %d, want 0", got)
	}

	// The target's upgrade hook fails, so the previous manifests are re-applied:
	// the rollback carries the stopped count instead of the count the package
	// renders (C-34, C-66).
	requestUpgrade(t, cluster, gateway, broken.Name)
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)
	if version := workloadContent(t, cluster); version != "one" {
		t.Fatalf("content after the rollback = %q, want %q", version, "one")
	}
	if got := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, applicationName); got != 0 {
		t.Fatalf("replicas after the rollback = %d, want 0", got)
	}
	if upgrade, _, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "upgrade"); upgrade["rolledBack"] != true {
		t.Fatalf("status.upgrade = %v, want a reverted target", upgrade)
	}

	// A target the package keeps: the fresh render is converged to the stopped
	// count before it is written.
	requestUpgrade(t, cluster, gateway, next.Name)
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)
	if version := workloadContent(t, cluster); version != "three" {
		t.Fatalf("content after the upgrade = %q, want %q", version, "three")
	}
	if got := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, applicationName); got != 0 {
		t.Fatalf("replicas after the upgrade = %d, want 0", got)
	}
	applied, _, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "applied")
	if applied["definitionDigest"] != next.Spec.Package.Digest {
		t.Fatalf("the applied baseline names %v, want %s", applied["definitionDigest"], next.Spec.Package.Digest)
	}
}

// requestUpgrade writes another definition reference into the application while
// the stop marker the user wrote stays in the spec.
func requestUpgrade(t *testing.T, cluster *harness.Cluster, gateway *harness.ControlPlane, definition string) {
	t.Helper()
	stored := cluster.Get(t, "v1beta1", "Application", applicationName)
	update := harness.ApplicationManifest(applicationName, harness.Namespace, definition, targetNamespace, nil, nil)
	if paused, found, _ := unstructured.NestedBool(stored.Object, "spec", "paused"); found {
		update["spec"].(map[string]any)["paused"] = paused
	}
	update["metadata"] = map[string]any{
		"name":            applicationName,
		"namespace":       harness.Namespace,
		"labels":          map[string]any{"managed-by": "kos-shell"},
		"resourceVersion": stored.GetResourceVersion(),
	}
	if status, body := gateway.Call(t, http.MethodPut,
		"/api/v1/resources/Application/"+applicationName+"?namespace="+harness.Namespace, update, nil); status != http.StatusAccepted {
		t.Fatalf("request the upgrade: status %d, body %v", status, body)
	}
}

// workloadContent reads the version annotation of the rendered Deployment.
func workloadContent(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	version, _, _ := unstructured.NestedString(cluster.Workload(t, "Deployment", targetNamespace, applicationName).Object,
		"metadata", "annotations", "example.tsic/version")
	return version
}

// callAction posts one application action and requires acceptance.
func callAction(t *testing.T, gateway *harness.ControlPlane, extra map[string]any) {
	t.Helper()
	body := map[string]any{"kind": "Application", "namespace": harness.Namespace, "name": applicationName}
	for key, value := range extra {
		body[key] = value
	}
	if status, response := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", body, nil); status != http.StatusAccepted {
		t.Fatalf("action %v: status %d, body %v", extra, status, response)
	}
}

// reconcileOnce drives one application round.
func reconcileOnce(t *testing.T, controller *app.Controller) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: harness.Namespace, Name: applicationName,
	}); err != nil {
		t.Fatalf("reconcile %s: %v", applicationName, err)
	}
}

// newRouteReconciler builds the endpoint reconciler over the cluster.
func newRouteReconciler(cluster *harness.Cluster) *endpoint.Reconciler {
	return endpoint.NewReconciler(cluster.Writer(), harness.Declarations{})
}

// phaseOf reads the stored phase.
func phaseOf(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	phase, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "phase")
	if err != nil {
		t.Fatalf("read the phase: %v", err)
	}
	return phase
}

// healthOf reads the stored health state.
func healthOf(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	health, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "health", "state")
	if err != nil {
		t.Fatalf("read the health: %v", err)
	}
	return health
}

// requireCondition requires one condition of the endpoint.
func requireCondition(t *testing.T, cluster *harness.Cluster, conditionType string, want metav1.ConditionStatus) {
	t.Helper()
	condition, found := harness.FindCondition(t, cluster, "Endpoint", applicationName, conditionType)
	if !found {
		t.Fatalf("the endpoint carries no %s condition", conditionType)
	}
	if condition.Status != want {
		t.Fatalf("%s = %s, want %s", conditionType, condition.Status, want)
	}
}

// createClaim writes the claim the render references.
func createClaim(t *testing.T, cluster *harness.Cluster, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	claim := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
			"accessModes": []any{"ReadWriteOnce"},
			"resources":   map[string]any{"requests": map[string]any{"storage": "1Gi"}},
		},
	}}
	ctx := context.Background()
	created, err := cluster.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}).
		Namespace(namespace).Create(ctx, claim, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create the claim: %v", err)
	}
	return created
}

// claimExists reports whether the claim is still stored.
func claimExists(t *testing.T, cluster *harness.Cluster, namespace, name string) bool {
	t.Helper()
	ctx := context.Background()
	_, err := cluster.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}).
		Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	return true
}

// claimVersion reads the claim's resourceVersion, so an untouched object reads
// the same before and after a lifecycle action.
func claimVersion(t *testing.T, cluster *harness.Cluster, namespace, name string) string {
	t.Helper()
	ctx := context.Background()
	object, err := cluster.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}).
		Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	return object.GetResourceVersion()
}

// serviceExists reports whether the rendered Service is still stored.
func serviceExists(t *testing.T, cluster *harness.Cluster, namespace, name string) bool {
	t.Helper()
	ctx := context.Background()
	_, err := cluster.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "services"}).
		Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read the service: %v", err)
	}
	return true
}
