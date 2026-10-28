// Package tc52 verifies the refusal of C-34: a package that declares a data
// format change is never upgraded by KOS, the phase and the downstream version
// stay where they were, the reason names the irreversible item and no request
// path bypasses the refusal (AC-26, TC-52).
package tc52

import (
	"context"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	// targetNamespace is where the package renders.
	targetNamespace = "apps"
	// applicationName is the application the case upgrades.
	applicationName = "web"
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

func TestTC52IrreversibleDeclarationIsRefused(t *testing.T) {
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
	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q after the install", phase, app.PhaseReady)
	}
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q, want %q", version, "one")
	}

	requestUpgrade(t, cluster, gateway, second.Name)
	reconcileOnce(t, controller)

	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want the pre-upgrade %q", phase, app.PhaseReady)
	}
	condition := conditionOf(t, cluster, app.ConditionIrreversibleUpgrade)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("the irreversible condition is missing or not True: %+v", condition)
	}
	if condition.Reason != app.ConditionIrreversibleUpgrade || condition.Message == "" {
		t.Fatalf("the condition does not name the irreversible upgrade: %+v", condition)
	}
	report := statusMap(t, cluster, "upgrade")
	items, _ := report["irreversible"].([]any)
	if len(items) == 0 {
		t.Fatalf("the preflight reported no irreversible item: %v", report)
	}
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q, want the unchanged %q", version, "one")
	}

	// Every path that could carry a bypass is refused: the confirmation the
	// external-change pause would accept is not available here, and repeated
	// rounds keep the refusal (C-34).
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", map[string]any{
		"kind":         "Application",
		"namespace":    harness.Namespace,
		"name":         applicationName,
		"action":       app.ActionConfirm,
		"targetDigest": report["renderDigest"],
	}, nil); status != http.StatusBadRequest {
		t.Fatalf("a confirmation bypassed the refusal: status %d, body %v", status, body)
	}
	for range 3 {
		reconcileOnce(t, controller)
	}
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("a repeated round applied the refused upgrade: downstream version %q", version)
	}
	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q after the repeated rounds, want %q", phase, app.PhaseReady)
	}
}

// TestTC52DroppedVolumeDeclarationIsRefused covers the second irreversible
// category of C-34: an upgrade that drops a volume declaration is refused
// before anything is applied.
func TestTC52DroppedVolumeDeclarationIsRefused(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)

	first := harness.DefinitionContent(t, "web-one", "", template("one"), nil, nil)
	second := harness.DefinitionContent(t, "web-two", "", template("two"), nil, nil)
	cluster.InstallDefinition(t, first)
	cluster.InstallDefinition(t, second)

	gateway := cluster.StartControlPlane(t, nil)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.ApplicationManifest(applicationName, harness.Namespace, first.Name, targetNamespace, nil, []string{"data"}), nil); status != http.StatusAccepted {
		t.Fatalf("create application: status %d, body %v", status, body)
	}
	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)
	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q after the install", phase, app.PhaseReady)
	}

	// The new spec names the new package and drops the volume declaration.
	update := harness.ApplicationManifest(applicationName, harness.Namespace, second.Name, targetNamespace, nil, nil)
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
	reconcileOnce(t, controller)

	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want the pre-upgrade %q", phase, app.PhaseReady)
	}
	condition := conditionOf(t, cluster, app.ConditionIrreversibleUpgrade)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("the irreversible condition is missing or not True: %+v", condition)
	}
	items, _ := statusMap(t, cluster, "upgrade")["irreversible"].([]any)
	found := false
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["kind"] == "volumeDelete" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the preflight refused no volume deletion: %v", items)
	}
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q, want the unchanged %q", version, "one")
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

// conditionOf reads one condition of the application.
func conditionOf(t *testing.T, cluster *harness.Cluster, conditionType string) *metav1.Condition {
	t.Helper()
	conditions := harness.Conditions(t, cluster.Get(t, "v1beta1", "Application", applicationName))
	condition, present := state.Find(conditions, conditionType)
	if !present {
		return nil
	}
	return &condition
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
