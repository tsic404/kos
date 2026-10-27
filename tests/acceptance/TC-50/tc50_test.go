// Package tc50 verifies the normal upgrade path of C-34: a spec write that
// names a new package runs the preflight, applies the new manifests and lands
// back in Ready with the new render as the baseline and an event recording it
// (AC-25, TC-50).
package tc50

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
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

func TestTC50NormalUpgrade(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)

	first := harness.DefinitionContent(t, "web-one", "", template("one"), nil, nil)
	second := harness.DefinitionContent(t, "web-two", "", template("two"), nil, nil)
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
	baseline := appliedDigest(t, cluster)

	// C-32: the catalogue may republish the definition a running application
	// names; installed objects change only when the application's own spec does.
	catalogue := harness.DefinitionContent(t, first.Name, "", template("catalogue"), nil, nil)
	cluster.InstallDefinition(t, catalogue)
	reconcileOnce(t, controller)
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("a definition update replaced the installed object with %q", version)
	}
	if digest := appliedDigest(t, cluster); digest != baseline {
		t.Fatalf("a definition update moved the baseline from %s to %s", baseline, digest)
	}

	// A spec write that names the new package is the upgrade request (C-34).
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
	if phase := phaseOf(t, cluster); phase != app.PhaseUpgrading {
		t.Fatalf("phase = %q, want %q after the preflight", phase, app.PhaseUpgrading)
	}
	report := statusMap(t, cluster, "upgrade")
	if changes, _ := report["renderChanges"].([]any); len(changes) == 0 {
		t.Fatalf("the preflight reported no rendered difference: %v", report)
	}
	if irreversible, _ := report["irreversible"].([]any); len(irreversible) != 0 {
		t.Fatalf("the preflight refused a plain upgrade: %v", irreversible)
	}

	reconcileOnce(t, controller)
	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q after the upgrade", phase, app.PhaseReady)
	}
	if version := downstreamVersion(t, cluster); version != "two" {
		t.Fatalf("downstream version = %q, want %q", version, "two")
	}
	recorded := statusMap(t, cluster, "applied")
	if recorded["definitionDigest"] != second.Spec.Package.Digest {
		t.Fatalf("the applied baseline names %v, want %s", recorded["definitionDigest"], second.Spec.Package.Digest)
	}
	if recorded["renderDigest"] == baseline {
		t.Fatal("the render digest did not move to the new package")
	}
	if kept := keptRenderDigest(t, cluster); kept != recorded["renderDigest"] {
		t.Fatalf("the recorded render digest %v does not match the kept manifests %s", recorded["renderDigest"], kept)
	}
	if !hasEvent(t, cluster, app.EventUpgradeCompleted) {
		t.Fatalf("the application's event stream has no %s record", app.EventUpgradeCompleted)
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

// appliedDigest reads the render digest of the applied baseline.
func appliedDigest(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	digest, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "applied", "renderDigest")
	if err != nil {
		t.Fatalf("read the applied digest: %v", err)
	}
	return digest
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

// keptRenderDigest recomputes the digest of the manifest set the baseline kept,
// so the recorded digest is proven to describe it (C-34).
func keptRenderDigest(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	raw, found, err := unstructured.NestedSlice(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "applied", "render")
	if err != nil || !found {
		t.Fatalf("read the kept manifests: found=%v err=%v", found, err)
	}
	objects := make([]*unstructured.Unstructured, 0, len(raw))
	for _, item := range raw {
		encoded, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("encode a kept manifest: %v", err)
		}
		decoded := map[string]any{}
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("decode a kept manifest: %v", err)
		}
		objects = append(objects, &unstructured.Unstructured{Object: decoded})
	}
	digest, err := app.RenderDigest(objects)
	if err != nil {
		t.Fatalf("digest the kept manifests: %v", err)
	}
	return digest
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
