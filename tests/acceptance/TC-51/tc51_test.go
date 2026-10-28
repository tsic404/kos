// Package tc51 verifies the rollback of C-34: a package whose upgrade hook
// exits non-zero is reverted to the previous manifest set, the application
// returns to Ready and the event stream records the rollback (AC-25, TC-51).
package tc51

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/reconcile"
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

func TestTC51HookFailureRollsBack(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)

	attempts := filepath.Join(t.TempDir(), "attempts")
	failing := v1beta1.Hook{
		Name:    app.HookUpgrade,
		Command: []string{"sh", "-c", fmt.Sprintf("echo attempted >> %s; exit 1", attempts)},
	}
	first := harness.DefinitionContent(t, "web-one", "", template("one"), nil, nil)
	second := harness.DefinitionContent(t, "web-two", "", growingTemplate("two"), []v1beta1.Hook{failing}, nil)
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
	previous := appliedRenderDigest(t, cluster)

	requestUpgrade(t, cluster, gateway, second.Name)
	reconcileOnce(t, controller)
	if phase := phaseOf(t, cluster); phase != app.PhaseUpgrading {
		t.Fatalf("phase = %q, want %q after the preflight", phase, app.PhaseUpgrading)
	}
	reconcileOnce(t, controller)

	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q after the rollback", phase, app.PhaseReady)
	}
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q, want the previous %q", version, "one")
	}
	if missing := downstreamMissing(t, cluster, applicationName+"-extra"); !missing {
		t.Fatalf("%s survived the rollback: the cluster holds both versions", applicationName+"-extra")
	}
	if got := appliedRenderDigest(t, cluster); got != previous {
		t.Fatalf("the rollback baseline digest = %q, want the previous %q", previous, got)
	}
	if !hasEvent(t, cluster, app.EventUpgradeRolledBack) {
		t.Fatalf("the event stream has no %s record", app.EventUpgradeRolledBack)
	}
	rolledBack, _, err := unstructured.NestedBool(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "upgrade", "rolledBack")
	if err != nil || !rolledBack {
		t.Fatalf("status.upgrade.rolledBack = %v (%v), want true", rolledBack, err)
	}
	if got := attemptCount(t, attempts); got != 1 {
		t.Fatalf("the upgrade hook ran %d times, want 1", got)
	}

	// The same target is not applied again on its own: a reverted upgrade waits
	// for the user instead of flapping the workloads (C-34).
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q after the repeated rounds, want %q", version, "one")
	}
	if got := attemptCount(t, attempts); got != 1 {
		t.Fatalf("the hook ran %d times without a new request, want 1", got)
	}

	// A user retry is the way back: the record clears the reverted attempt and
	// the next round runs the upgrade again.
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", map[string]any{
		"kind":      "Application",
		"namespace": harness.Namespace,
		"name":      applicationName,
		"action":    app.ActionRetry,
	}, nil); status != http.StatusAccepted {
		t.Fatalf("request the retry: status %d, body %v", status, body)
	}
	reconcileOnce(t, controller)
	if value, _, err := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "upgrade"); err != nil || value != nil {
		t.Fatalf("the retry left the upgrade record in place: %v (%v)", value, err)
	}
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)
	if got := attemptCount(t, attempts); got != 2 {
		t.Fatalf("the hook ran %d times after the retry, want 2", got)
	}
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q after the retried rollback, want %q", version, "one")
	}
}

// growingTemplate renders the version the package carries and one object the
// previous package did not have, which is what a rollback has to remove.
func growingTemplate(version string) string {
	return template(version) + `---
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}-extra
  namespace: {{ .Namespace }}
data:
  version: "` + version + `"
`
}

// appliedRenderDigest reads the manifest digest the baseline records.
func appliedRenderDigest(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	digest, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "applied", "renderDigest")
	if err != nil {
		t.Fatalf("read the baseline digest: %v", err)
	}
	return digest
}

// downstreamMissing reports whether one rendered object is gone.
func downstreamMissing(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	_, err := cluster.Kube.CoreV1().ConfigMaps(targetNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return false
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

// attemptCount counts the runs the hook recorded.
func attemptCount(t *testing.T, path string) int {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read the hook log: %v", err)
	}
	return len(strings.Fields(string(content)))
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

// downstreamVersion reads the version the rendered ConfigMap carries.
func downstreamVersion(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	configMap, err := cluster.Kube.CoreV1().ConfigMaps(targetNamespace).Get(context.Background(), applicationName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the downstream ConfigMap: %v", err)
	}
	return configMap.Data["version"]
}

// hasEvent reports whether the application's event stream carries one reason.
func hasEvent(t *testing.T, cluster *harness.Cluster, reason string) bool {
	t.Helper()
	events, err := cluster.Kube.CoreV1().Events(harness.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, event := range events.Items {
		if event.Reason == reason && event.InvolvedObject.Name == applicationName {
			return true
		}
	}
	return false
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
