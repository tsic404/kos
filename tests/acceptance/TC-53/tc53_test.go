// Package tc53 verifies the external-change pause of C-35: an application
// whose upgrade declares an irreversible change is suspended after a write made
// outside KOS, a confirmation that does not name the target keeps the pause, the
// three-way agreement clears it, and a consumed record never fires again
// (AC-26, TC-53).
package tc53

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	// targetNamespace is where the package renders.
	targetNamespace = "apps"
	// applicationName is the application the case upgrades.
	applicationName = "web"
	// recordName is the one-shot confirmation record of the application.
	recordPrefix = "kos-action-"
)

// template renders one ConfigMap whose data the package version names.
func template(version string) string {
	return `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  version: "` + version + `"
`
}

func TestTC53ExternalChangeSuspendsUntilConfirmed(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)

	first := harness.DefinitionContent(t, "web-one", "1", template("one"), nil, nil)
	second := harness.DefinitionContent(t, "web-two", "2", template("two"), nil, nil)
	cluster.InstallDefinition(t, first)
	cluster.InstallDefinition(t, second)

	gateway := cluster.StartControlPlane(t, nil)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.ApplicationManifest(applicationName, harness.Namespace, first.Name, targetNamespace, nil, nil), nil); status != http.StatusAccepted {
		t.Fatalf("create application: status %d, body %v", status, body)
	}
	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)

	// The package declares a data format change, so KOS refuses the upgrade and
	// the user upgrades the workload outside the product (C-34).
	requestUpgrade(t, cluster, gateway, second.Name)
	reconcileOnce(t, controller)
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q, want the refused target to be unapplied", version)
	}
	upgradeDownstream(t, cluster)

	reconcileOnce(t, controller)
	if version := downstreamVersion(t, cluster); version != "two" {
		t.Fatalf("downstream version = %q, want the externally written %q", version, "two")
	}
	assertPaused(t, cluster)

	target := statusMap(t, cluster, "upgrade")["renderDigest"]
	if target == nil || target == "" {
		t.Fatalf("the preflight reports no target render digest: %v", statusMap(t, cluster, "upgrade"))
	}

	// A confirmation of another digest is accepted as a record and consumed
	// without clearing the pause (C-35).
	confirm(t, cluster, gateway, "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	reconcileOnce(t, controller)
	assertPaused(t, cluster)
	if version := downstreamVersion(t, cluster); version != "two" {
		t.Fatalf("a wrong confirmation moved the downstream to %q", version)
	}
	if recordExists(t, cluster) {
		t.Fatal("the consumed confirmation record is still in the KOS namespace")
	}

	// The three-way agreement clears the pause and keeps the external version as
	// the new baseline.
	confirm(t, cluster, gateway, target.(string))
	reconcileOnce(t, controller)
	conditions := harness.Conditions(t, cluster.Get(t, "v1beta1", "Application", applicationName))
	reconciled := state.ReconciledOf(conditions)
	if reconciled.Status != metav1.ConditionTrue {
		t.Fatalf("Reconciled = %s (%s), want True after the confirmation", reconciled.Status, reconciled.Reason)
	}
	recorded := statusMap(t, cluster, "applied")
	if recorded["definitionDigest"] != second.Spec.Package.Digest {
		t.Fatalf("the confirmed baseline names %v, want %s", recorded["definitionDigest"], second.Spec.Package.Digest)
	}
	if recordExists(t, cluster) {
		t.Fatal("the consumed confirmation record is still in the KOS namespace")
	}

	// The consumed record does not fire twice: a later round keeps the external
	// version.
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)
	if version := downstreamVersion(t, cluster); version != "two" {
		t.Fatalf("a later round moved the downstream to %q, want the confirmed %q", version, "two")
	}
	if reconciled := state.ReconciledOf(harness.Conditions(t, cluster.Get(t, "v1beta1", "Application", applicationName))); reconciled.Status != metav1.ConditionTrue {
		t.Fatalf("a later round paused the confirmed application again: %+v", reconciled)
	}
}

// workloadTemplate renders one Deployment whose image names the package
// version, so an upgrade performed outside the product is a write KOS observes
// on a workload the lifecycle actions drive.
func workloadTemplate(version string) string {
	return `apiVersion: apps/v1
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
          image: web.example/` + version + `:latest
`
}

// TestTC53ConfirmAcceptsAStoppedApplication covers the third condition of C-35
// for an application whose lifecycle facts already moved the live objects: the
// reported target carries the zero replica count of a stop, so the confirmation
// of a stopped application is accepted and the stop survives the new baseline.
func TestTC53ConfirmAcceptsAStoppedApplication(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)

	first := harness.DefinitionContent(t, "stopped-one", "1", workloadTemplate("one"), nil, nil)
	second := harness.DefinitionContent(t, "stopped-two", "2", workloadTemplate("two"), nil, nil)
	cluster.InstallDefinition(t, first)
	cluster.InstallDefinition(t, second)

	gateway := cluster.StartControlPlane(t, nil)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.ApplicationManifest(applicationName, harness.Namespace, first.Name, targetNamespace, nil, nil), nil); status != http.StatusAccepted {
		t.Fatalf("create application: status %d, body %v", status, body)
	}
	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)
	if replicas := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, applicationName); replicas != 2 {
		t.Fatalf("installed replicas = %d, want 2", replicas)
	}

	// The application is stopped before the external change: the workload holds
	// zero while the spec keeps the count the package renders (C-66).
	action(t, gateway, map[string]any{"action": app.ActionStop})
	reconcileOnce(t, controller)
	if replicas := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, applicationName); replicas != 0 {
		t.Fatalf("stopped replicas = %d, want 0", replicas)
	}

	// The package declares a data format change, so KOS refuses the upgrade and
	// the user writes the new version outside the product (C-34).
	requestUpgradeKeepingTheStop(t, cluster, gateway, second.Name)
	reconcileOnce(t, controller)
	if version := workloadVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q, want the refused target to be unapplied", version)
	}
	writeWorkloadVersion(t, cluster, "two")
	reconcileOnce(t, controller)
	assertPaused(t, cluster)

	// The reported target carries the stopped replica count, so the confirmation
	// of a stopped application meets all three conditions of C-35.
	target := statusMap(t, cluster, "upgrade")["renderDigest"]
	if target == nil || target == "" {
		t.Fatalf("the preflight reports no target render digest: %v", statusMap(t, cluster, "upgrade"))
	}
	confirm(t, cluster, gateway, target.(string))
	reconcileOnce(t, controller)
	if reconciled := state.ReconciledOf(harness.Conditions(t, cluster.Get(t, "v1beta1", "Application", applicationName))); reconciled.Status != metav1.ConditionTrue {
		t.Fatalf("Reconciled = %s (%s), want True after the confirmation", reconciled.Status, reconciled.Reason)
	}
	recorded := statusMap(t, cluster, "applied")
	if recorded["definitionDigest"] != second.Spec.Package.Digest {
		t.Fatalf("the confirmed baseline names %v, want %s", recorded["definitionDigest"], second.Spec.Package.Digest)
	}

	// The stop survives the confirmed baseline: the later rounds keep the
	// workload at zero (C-35, C-66).
	reconcileOnce(t, controller)
	if replicas := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, applicationName); replicas != 0 {
		t.Fatalf("replicas after the confirmation = %d, want 0", replicas)
	}
}

// action posts one lifecycle action of the application and requires acceptance.
func action(t *testing.T, gateway *harness.ControlPlane, extra map[string]any) {
	t.Helper()
	body := map[string]any{"kind": "Application", "namespace": harness.Namespace, "name": applicationName}
	for key, value := range extra {
		body[key] = value
	}
	if status, response := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", body, nil); status != http.StatusAccepted {
		t.Fatalf("action %v: status %d, body %v", extra, status, response)
	}
}

// requestUpgradeKeepingTheStop writes the new definition reference into the
// application while the stop marker the user already wrote stays in the spec.
func requestUpgradeKeepingTheStop(t *testing.T, cluster *harness.Cluster, gateway *harness.ControlPlane, definition string) {
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

// workloadVersion reads the image of the rendered Deployment, which names the
// package version the cluster holds.
func workloadVersion(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	containers, _, _ := unstructured.NestedSlice(cluster.Workload(t, "Deployment", targetNamespace, applicationName).Object,
		"spec", "template", "spec", "containers")
	if len(containers) == 0 {
		t.Fatal("the rendered Deployment has no container")
	}
	image, _ := containers[0].(map[string]any)["image"].(string)
	return strings.TrimSuffix(strings.TrimPrefix(image, "web.example/"), ":latest")
}

// writeWorkloadVersion writes one version into the rendered Deployment the way
// a user working outside KOS would (C-35).
func writeWorkloadVersion(t *testing.T, cluster *harness.Cluster, version string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resource := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	stored, err := cluster.Dynamic.Resource(resource).Namespace(targetNamespace).Get(ctx, applicationName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the downstream Deployment: %v", err)
	}
	containers, found, err := unstructured.NestedSlice(stored.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		t.Fatalf("read the downstream containers: %v (%v)", err, found)
	}
	container, _ := containers[0].(map[string]any)
	container["image"] = "web.example/" + version + ":latest"
	if err := unstructured.SetNestedSlice(stored.Object, containers, "spec", "template", "spec", "containers"); err != nil {
		t.Fatalf("write the downstream image: %v", err)
	}
	if _, err := cluster.Dynamic.Resource(resource).Namespace(targetNamespace).Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write the downstream Deployment outside KOS: %v", err)
	}
}

// assertPaused checks the forced pause of C-35 and that the application keeps
// its phase while it waits.
func assertPaused(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	reconciled := state.ReconciledOf(harness.Conditions(t, cluster.Get(t, "v1beta1", "Application", applicationName)))
	if reconciled.Status != metav1.ConditionFalse || reconciled.Reason != app.ReasonExternalChangePending {
		t.Fatalf("Reconciled = %s/%s, want False/%s", reconciled.Status, reconciled.Reason, app.ReasonExternalChangePending)
	}
	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q while the external change waits", phase, app.PhaseReady)
	}
}

// confirm submits one confirmation of the given digest and checks the record the
// API wrote for it.
func confirm(t *testing.T, cluster *harness.Cluster, gateway *harness.ControlPlane, digest string) {
	t.Helper()
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", map[string]any{
		"kind":         "Application",
		"namespace":    harness.Namespace,
		"name":         applicationName,
		"action":       app.ActionConfirm,
		"targetDigest": digest,
	}, nil); status != http.StatusAccepted {
		t.Fatalf("submit the confirmation: status %d, body %v", status, body)
	}
	if !recordExists(t, cluster) {
		t.Fatal("the confirmation wrote no record in the KOS namespace")
	}
}

// recordExists reports whether a confirmation record of the application is in
// the KOS namespace.
func recordExists(t *testing.T, cluster *harness.Cluster) bool {
	t.Helper()
	application := cluster.Get(t, "v1beta1", "Application", applicationName)
	records, err := cluster.Kube.CoreV1().ConfigMaps(api.SystemNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: app.ActionOwnerLabel + "=" + string(application.GetUID()),
	})
	if err != nil {
		t.Fatalf("list the action records: %v", err)
	}
	for _, record := range records.Items {
		if action := record.Labels[app.ActionLabel]; action == app.ActionConfirm {
			return true
		}
	}
	return false
}

// upgradeDownstream writes the target package's render into the cluster the way
// a user working outside KOS would (C-35).
func upgradeDownstream(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	writeDownstreamVersion(t, cluster, "two")
}

// writeDownstreamVersion writes one value into the rendered object outside KOS.
func writeDownstreamVersion(t *testing.T, cluster *harness.Cluster, version string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	configMap, err := cluster.Kube.CoreV1().ConfigMaps(targetNamespace).Get(ctx, applicationName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the downstream ConfigMap: %v", err)
	}
	configMap.Data["version"] = version
	if _, err := cluster.Kube.CoreV1().ConfigMaps(targetNamespace).Update(ctx, configMap, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write the downstream ConfigMap outside KOS: %v", err)
	}
}

// requestUpgrade writes the new definition reference into the application.
func requestUpgrade(t *testing.T, cluster *harness.Cluster, gateway *harness.ControlPlane, definition string) {
	t.Helper()
	update := harness.ApplicationManifest(applicationName, harness.Namespace, definition, targetNamespace, nil, nil)
	update["metadata"] = map[string]any{
		"name":            applicationName,
		"namespace":       harness.Namespace,
		"labels":          map[string]any{"managed-by": "kos-shell"},
		"resourceVersion": cluster.Get(t, "v1beta1", "Application", applicationName).GetResourceVersion(),
	}
	if status, body := gateway.Call(t, http.MethodPut,
		"/api/v1/resources/Application/"+applicationName+"?namespace="+harness.Namespace, update, nil); status != http.StatusAccepted {
		t.Fatalf("request the upgrade: status %d, body %v", status, body)
	}
}

// phaseOf reads the stored phase of the application.
func phaseOf(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	phase, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "phase")
	if err != nil {
		t.Fatalf("read phase: %v", err)
	}
	return phase
}

// statusMap reads one status section of the application.
func statusMap(t *testing.T, cluster *harness.Cluster, field string) map[string]any {
	t.Helper()
	value, found, err := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", field)
	if err != nil || !found {
		t.Fatalf("read status.%s: found=%v err=%v", field, found, err)
	}
	return value
}

// downstreamVersion reads the version the rendered ConfigMap carries.
func downstreamVersion(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	configMap, err := cluster.Kube.CoreV1().ConfigMaps(targetNamespace).Get(context.Background(), applicationName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ""
		}
		t.Fatalf("read the downstream ConfigMap: %v", err)
	}
	return configMap.Data["version"]
}

// reconcileOnce drives one round of the application.
func reconcileOnce(t *testing.T, controller *app.Controller) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: harness.Namespace, Name: applicationName,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// TestTC53ConfirmationNeedsTheObservedTarget covers the bypass C-34 and AC-26
// forbid: a confirmation may not stand in for an upgrade the preflight refused
// to run. Without the target actually written into the cluster the pause stays,
// nothing is applied and the package's upgrade hook never runs.
func TestTC53ConfirmationNeedsTheObservedTarget(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)

	attempts := filepath.Join(t.TempDir(), "attempts")
	hook := v1beta1.Hook{Name: app.HookUpgrade, Command: []string{"sh", "-c", "echo ran >> " + attempts}}
	first := harness.DefinitionContent(t, "web-one", "1", template("one"), nil, nil)
	second := harness.DefinitionContent(t, "web-two", "2", template("two"), []v1beta1.Hook{hook}, nil)
	cluster.InstallDefinition(t, first)
	cluster.InstallDefinition(t, second)

	gateway := cluster.StartControlPlane(t, nil)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.ApplicationManifest(applicationName, harness.Namespace, first.Name, targetNamespace, nil, nil), nil); status != http.StatusAccepted {
		t.Fatalf("create application: status %d, body %v", status, body)
	}
	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)

	// The package declares a data format change, so KOS refuses the upgrade and
	// leaves the downstream where it was (C-34).
	requestUpgrade(t, cluster, gateway, second.Name)
	reconcileOnce(t, controller)
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q, want the refused target to be unapplied", version)
	}

	// A write that is not the requested target still pauses the application, so
	// the confirmation below meets a pause (C-35).
	writeDownstreamVersion(t, cluster, "edited")
	reconcileOnce(t, controller)
	assertPaused(t, cluster)

	target := statusMap(t, cluster, "upgrade")["renderDigest"]
	if target == nil || target == "" {
		t.Fatalf("the preflight reports no target render digest: %v", statusMap(t, cluster, "upgrade"))
	}
	confirm(t, cluster, gateway, target.(string))
	reconcileOnce(t, controller)

	// The record names the requested target, but the cluster does not hold it:
	// the pause stays, the downstream keeps the external value and the refused
	// target never becomes the baseline (C-34, AC-26).
	assertPaused(t, cluster)
	if version := downstreamVersion(t, cluster); version != "edited" {
		t.Fatalf("downstream version = %q, want the externally written %q", version, "edited")
	}
	if recorded := statusMap(t, cluster, "applied")["definitionDigest"]; recorded != first.Spec.Package.Digest {
		t.Fatalf("the confirmed baseline names %v, want the refused target to stay out of it", recorded)
	}
	if _, err := os.Stat(attempts); !os.IsNotExist(err) {
		t.Fatalf("the refused upgrade ran its upgrade hook: %v", err)
	}
	if recordExists(t, cluster) {
		t.Fatal("the consumed confirmation record is still in the KOS namespace")
	}

	reconcileOnce(t, controller)
	if version := downstreamVersion(t, cluster); version != "edited" {
		t.Fatalf("a later round moved the downstream to %q", version)
	}
	if recorded := statusMap(t, cluster, "applied")["definitionDigest"]; recorded != first.Spec.Package.Digest {
		t.Fatalf("a later round applied the refused target: %v", recorded)
	}

	// A confirmation that names the digest the cluster holds after the external
	// write still is not the target the stored spec requests. The recorded
	// target has to be the render of the requested package, so a value that only
	// matches the observation is refused rather than turning the refused target
	// into the baseline (C-34, C-35, AC-26).
	live := observedRenderDigest(t, cluster, second, "edited")
	confirm(t, cluster, gateway, live)
	reconcileOnce(t, controller)
	assertPaused(t, cluster)
	if version := downstreamVersion(t, cluster); version != "edited" {
		t.Fatalf("the live-digest confirmation moved the downstream to %q", version)
	}
	if recorded := statusMap(t, cluster, "applied")["definitionDigest"]; recorded != first.Spec.Package.Digest {
		t.Fatalf("the live-digest confirmation made the refused target the baseline: %v", recorded)
	}
	reconcileOnce(t, controller)
	if version := downstreamVersion(t, cluster); version != "edited" {
		t.Fatalf("a later round applied the refused target after the live-digest confirmation: %q", version)
	}
	if recorded := statusMap(t, cluster, "applied")["definitionDigest"]; recorded != first.Spec.Package.Digest {
		t.Fatalf("a later round made the refused target the baseline: %v", recorded)
	}
}

// observedRenderDigest digests the target manifest set the way the cluster holds
// it after a write made outside KOS: the shape the requested package renders,
// with the value the live object carries at that path (C-35).
func observedRenderDigest(t *testing.T, cluster *harness.Cluster, definition *v1beta1.ApplicationDefinition, version string) string {
	t.Helper()
	application := cluster.Get(t, "v1beta1", "Application", applicationName)
	objects, err := app.Render(definition.Spec, app.RenderInput{Name: applicationName, Namespace: targetNamespace})
	if err != nil {
		t.Fatalf("render the target: %v", err)
	}
	for _, object := range objects {
		labels := object.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[markers.OwnerUID] = string(application.GetUID())
		object.SetLabels(labels)
		if err := unstructured.SetNestedField(object.Object, version, "data", "version"); err != nil {
			t.Fatalf("write the observed value into the target: %v", err)
		}
	}
	digest, err := app.RenderDigest(objects)
	if err != nil {
		t.Fatalf("digest the observed target: %v", err)
	}
	return digest
}
