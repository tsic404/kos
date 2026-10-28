// Package tc116 verifies the drift policies of C-35: reconcile writes the
// desired value back, adopt records the external value in
// Application.status.drift without touching the user's private config and stops
// reporting it once the user writes the value back, ignore keeps the external
// value and reports it, and a difference the config structure cannot express is
// refused a record while the drift condition stays (AC-57, TC-116).
package tc116

import (
	"context"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	// targetNamespace is where the package renders.
	targetNamespace = "apps"
	// definitionName names the package every application installs.
	definitionName = "drifty"
)

// template renders one ConfigMap with a config-driven field and a fixed one, so
// a case can produce a difference the config can express and one it cannot.
const template = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  mode: "{{ .Config.mode }}"
  fixed: "constant"
`

func TestTC116DriftPolicies(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)

	definition := harness.DefinitionContent(t, definitionName, "", template, nil, []v1beta1.ConfigField{
		{Name: "mode", Type: app.TypeString, Default: new("desired")},
	})
	cluster.InstallDefinition(t, definition)

	gateway := cluster.StartControlPlane(t, nil)
	controller := cluster.ApplicationController(t, app.Options{})

	cases := map[string]v1beta1.DriftPolicy{
		"reconciled":    v1beta1.DriftPolicyReconcile,
		"adopted":       v1beta1.DriftPolicyAdopt,
		"ignored":       v1beta1.DriftPolicyIgnore,
		"adopted-fixed": v1beta1.DriftPolicyAdopt,
	}
	for name, policy := range cases {
		create(t, cluster, gateway, name, policy)
		reconcileOnce(t, controller, name)
		reconcileOnce(t, controller, name)
		if phase := phaseOf(t, cluster, name); phase != app.PhaseReady {
			t.Fatalf("%s: phase = %q, want %q", name, phase, app.PhaseReady)
		}
	}

	// A write made outside KOS to the config-driven field (C-35).
	for name := range cases {
		writeDownstream(t, cluster, name, "mode", "external")
	}
	// A write the config structure cannot express: the fixed data field.
	writeDownstream(t, cluster, "adopted-fixed", "fixed", "changed")

	for name := range cases {
		reconcileOnce(t, controller, name)
	}

	if value := downstream(t, cluster, "reconciled", "mode"); value != "desired" {
		t.Fatalf("reconcile policy left the downstream at %q, want the desired %q", value, "desired")
	}
	if conditionOf(t, cluster, "reconciled", app.ConditionDrifted) != nil {
		t.Fatal("the reconcile policy reported a drift condition after writing the desired value back")
	}

	if value := downstream(t, cluster, "adopted", "mode"); value != "external" {
		t.Fatalf("adopt policy moved the downstream to %q, want the external %q", value, "external")
	}
	entries := driftOf(t, cluster, "adopted")
	if len(entries) != 1 || entries[0]["path"] != "data.mode" {
		t.Fatalf("status.drift = %v, want the data.mode entry", entries)
	}
	if entries[0]["external"] != `"external"` {
		t.Fatalf("status.drift records %v, want the external value", entries[0]["external"])
	}
	if mode, present := configOf(t, cluster, "adopted")["mode"]; present && mode != "desired" {
		t.Fatalf("adopt policy rewrote the private config to %v", mode)
	}
	if conditionOf(t, cluster, "adopted", app.ConditionDrifted) == nil {
		t.Fatal("adopt policy reported no drift condition")
	}

	if value := downstream(t, cluster, "ignored", "mode"); value != "external" {
		t.Fatalf("ignore policy moved the downstream to %q, want the external %q", value, "external")
	}
	if conditionOf(t, cluster, "ignored", app.ConditionDrifted) == nil {
		t.Fatal("ignore policy reported no drift condition")
	}

	// A difference the config structure cannot express is refused a record and
	// leaves the drift condition in place (C-35).
	if entries := driftOf(t, cluster, "adopted-fixed"); len(entries) != 0 {
		t.Fatalf("status.drift recorded an inexpressible difference: %v", entries)
	}
	if conditionOf(t, cluster, "adopted-fixed", app.ConditionDrifted) == nil {
		t.Fatal("the inexpressible difference cleared the drift condition")
	}

	// The user writes the external value back through the API: the condition
	// disappears and the value is what KOS now renders (C-35).
	writeBack(t, cluster, gateway, "adopted", "external")
	reconcileOnce(t, controller, "adopted")
	reconcileOnce(t, controller, "adopted")
	if entries := driftOf(t, cluster, "adopted"); len(entries) != 0 {
		t.Fatalf("status.drift still records %v after the user wrote the value back", entries)
	}
	if conditionOf(t, cluster, "adopted", app.ConditionDrifted) != nil {
		t.Fatal("the drift condition stayed after the user wrote the value back")
	}
	if value := downstream(t, cluster, "adopted", "mode"); value != "external" {
		t.Fatalf("the written-back value did not take effect: downstream mode = %q", value)
	}
}

// TestTC116RolledBackApplicationAnswersDrift covers the drift policy while an
// application waits on a reverted target: the wait does not exempt the policy,
// and the drift record of the round that reverted the target does not survive
// the round that found nothing to answer (C-35, AC-57).
func TestTC116RolledBackApplicationAnswersDrift(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)

	failing := v1beta1.Hook{Name: app.HookUpgrade, Command: []string{"sh", "-c", "exit 1"}}
	installed := harness.DefinitionContent(t, definitionName, "", template, nil, []v1beta1.ConfigField{
		{Name: "mode", Type: app.TypeString, Default: new("desired")},
	})
	broken := harness.DefinitionContent(t, definitionName+"-next", "", template, []v1beta1.Hook{failing}, []v1beta1.ConfigField{
		{Name: "mode", Type: app.TypeString, Default: new("desired")},
	})
	cluster.InstallDefinition(t, installed)
	cluster.InstallDefinition(t, broken)

	gateway := cluster.StartControlPlane(t, nil)
	controller := cluster.ApplicationController(t, app.Options{})
	create(t, cluster, gateway, "rolled-back", v1beta1.DriftPolicyAdopt)
	reconcileOnce(t, controller, "rolled-back")
	reconcileOnce(t, controller, "rolled-back")

	// The external write is recorded under adopt before the attempt, so the
	// record the reverted round inherits is the one it has to drop.
	writeDownstream(t, cluster, "rolled-back", "mode", "external")
	reconcileOnce(t, controller, "rolled-back")
	if entries := driftOf(t, cluster, "rolled-back"); len(entries) != 1 {
		t.Fatalf("status.drift = %v, want the recorded external value", entries)
	}

	// The target's upgrade hook fails, so the attempt is reverted and the
	// application waits for a new target (C-34).
	requestUpgrade(t, cluster, gateway, "rolled-back", broken.Name)
	reconcileOnce(t, controller, "rolled-back")
	reconcileOnce(t, controller, "rolled-back")
	reconcileOnce(t, controller, "rolled-back")
	upgrade, found, err := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", "rolled-back").Object, "status", "upgrade")
	if err != nil || !found || upgrade["rolledBack"] != true {
		t.Fatalf("status.upgrade = %v (%v), want a reverted target", upgrade, err)
	}
	if entries := driftOf(t, cluster, "rolled-back"); len(entries) != 0 {
		t.Fatalf("the reverted round carried a stale status.drift: %v", entries)
	}

	// A write made outside KOS while the application waits is answered by the
	// drift policy: adopt records it and leaves the external value in place.
	writeDownstream(t, cluster, "rolled-back", "mode", "edited")
	reconcileOnce(t, controller, "rolled-back")
	entries := driftOf(t, cluster, "rolled-back")
	if len(entries) != 1 || entries[0]["path"] != "data.mode" || entries[0]["external"] != `"edited"` {
		t.Fatalf("status.drift = %v, want the external write recorded while the target waits", entries)
	}
	if value := downstream(t, cluster, "rolled-back", "mode"); value != "edited" {
		t.Fatalf("adopt policy moved the waiting downstream to %q, want the external %q", value, "edited")
	}
	if conditionOf(t, cluster, "rolled-back", app.ConditionDrifted) == nil {
		t.Fatal("the external write produced no drift condition while the target waits")
	}
}

// requestUpgrade writes another definition reference into an application,
// keeping the drift policy the spec already carries.
func requestUpgrade(t *testing.T, cluster *harness.Cluster, gateway *harness.ControlPlane, name, definition string) {
	t.Helper()
	stored := cluster.Get(t, "v1beta1", "Application", name)
	update := harness.ApplicationManifest(name, harness.Namespace, definition, targetNamespace, nil, nil)
	if policy, found, _ := unstructured.NestedString(stored.Object, "spec", "driftPolicy"); found {
		update["spec"].(map[string]any)["driftPolicy"] = policy
	}
	update["metadata"] = map[string]any{
		"name":            name,
		"namespace":       harness.Namespace,
		"labels":          map[string]any{"managed-by": "kos-shell"},
		"resourceVersion": stored.GetResourceVersion(),
	}
	if status, body := gateway.Call(t, http.MethodPut,
		"/api/v1/resources/Application/"+name+"?namespace="+harness.Namespace, update, nil); status != http.StatusAccepted {
		t.Fatalf("%s: request the upgrade: status %d, body %v", name, status, body)
	}
}

// create installs one application with a drift policy through the API.
func create(t *testing.T, cluster *harness.Cluster, gateway *harness.ControlPlane, name string, policy v1beta1.DriftPolicy) {
	t.Helper()
	manifest := harness.ApplicationManifest(name, harness.Namespace, definitionName, targetNamespace, nil, nil)
	manifest["spec"].(map[string]any)["driftPolicy"] = string(policy)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
		t.Fatalf("%s: create application: status %d, body %v", name, status, body)
	}
}

// writeBack submits the user's own config write through the API (C-35).
func writeBack(t *testing.T, cluster *harness.Cluster, gateway *harness.ControlPlane, name, mode string) {
	t.Helper()
	update := harness.ApplicationManifest(name, harness.Namespace, definitionName, targetNamespace, map[string]any{"mode": mode}, nil)
	update["metadata"] = map[string]any{
		"name":            name,
		"namespace":       harness.Namespace,
		"labels":          map[string]any{"managed-by": "kos-shell"},
		"resourceVersion": cluster.Get(t, "v1beta1", "Application", name).GetResourceVersion(),
	}
	if status, body := gateway.Call(t, http.MethodPut,
		"/api/v1/resources/Application/"+name+"?namespace="+harness.Namespace, update, nil); status != http.StatusAccepted {
		t.Fatalf("%s: write the value back: status %d, body %v", name, status, body)
	}
}

// writeDownstream changes one data field the way a user working outside KOS
// would.
func writeDownstream(t *testing.T, cluster *harness.Cluster, name, field, value string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	configMap, err := cluster.Kube.CoreV1().ConfigMaps(targetNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("%s: read the downstream ConfigMap: %v", name, err)
	}
	configMap.Data[field] = value
	if _, err := cluster.Kube.CoreV1().ConfigMaps(targetNamespace).Update(ctx, configMap, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("%s: write the downstream ConfigMap outside KOS: %v", name, err)
	}
}

// downstream reads one data field of the rendered ConfigMap.
func downstream(t *testing.T, cluster *harness.Cluster, name, field string) string {
	t.Helper()
	configMap, err := cluster.Kube.CoreV1().ConfigMaps(targetNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("%s: read the downstream ConfigMap: %v", name, err)
	}
	return configMap.Data[field]
}

// driftOf reads status.drift of one application.
func driftOf(t *testing.T, cluster *harness.Cluster, name string) []map[string]any {
	t.Helper()
	raw, found, err := unstructured.NestedSlice(cluster.Get(t, "v1beta1", "Application", name).Object, "status", "drift")
	if err != nil {
		t.Fatalf("%s: read status.drift: %v", name, err)
	}
	if !found {
		return nil
	}
	entries := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s: drift entry is not an object: %v", name, item)
		}
		entries = append(entries, entry)
	}
	return entries
}

// configOf reads the stored private config of one application.
func configOf(t *testing.T, cluster *harness.Cluster, name string) map[string]any {
	t.Helper()
	config, found, err := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", name).Object, "spec", "config")
	if err != nil || !found {
		return map[string]any{}
	}
	return config
}

// phaseOf reads the stored phase of one application.
func phaseOf(t *testing.T, cluster *harness.Cluster, name string) string {
	t.Helper()
	phase, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", name).Object, "status", "phase")
	if err != nil {
		t.Fatalf("%s: read phase: %v", name, err)
	}
	return phase
}

// conditionOf reads one condition of one application; a missing condition is
// nil.
func conditionOf(t *testing.T, cluster *harness.Cluster, name, conditionType string) *metav1.Condition {
	t.Helper()
	condition, present := state.Find(harness.Conditions(t, cluster.Get(t, "v1beta1", "Application", name)), conditionType)
	if !present {
		return nil
	}
	return &condition
}

// reconcileOnce drives one round of one application.
func reconcileOnce(t *testing.T, controller *app.Controller, name string) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: harness.Namespace, Name: name,
	}); err != nil {
		t.Fatalf("%s: reconcile: %v", name, err)
	}
}
