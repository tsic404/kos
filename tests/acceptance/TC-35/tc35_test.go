// Package tc35 verifies that a plan request reports the three deviation classes
// without touching the cluster, and that the execution of the same set creates,
// updates and deletes exactly what the plan announced (AC-17, TC-35).
package tc35

import (
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/tests/acceptance/harness"
)

// ownerUID is the install scope the plan is bounded to.
const ownerUID = "install-uid"

// settleTimeout bounds the wait for accepted writes to reach their terminal
// state.
const settleTimeout = 60 * time.Second

// TestTC35PlanMatchesExecution plans an install, executes it, then plans and
// executes the same install one object smaller.
func TestTC35PlanMatchesExecution(t *testing.T) {
	cluster := harness.Start(t)
	// A volume the install owns but never names: releasing data needs its own
	// confirmation, so the plan must keep it and say so (C-39, F5).
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Manifest(
		"Volume", harness.Namespace, "data",
		map[string]string{"managed-by": "kos-shell", "owner-uid": ownerUID}, nil)})

	gateway := cluster.StartControlPlane(t, nil)

	first := setRequest(
		object("alpha", map[string]any{"example.test/version": "v1"}),
		object("beta", nil),
	)
	before := snapshot(t, cluster)
	planStatus, planned := gateway.Call(t, http.MethodPost, "/api/v1/plan-requests", first, nil)
	if planStatus != http.StatusOK {
		t.Fatalf("plan request: status %d, body %v", planStatus, planned)
	}
	if after := snapshot(t, cluster); !equal(before, after) {
		t.Fatalf("the plan changed the cluster:\nbefore %v\nafter  %v", before, after)
	}

	plan := planBody(t, planned)
	created := plannedObjects(t, plan, "create")
	wantCreated := []string{"kos.tsic.top/SystemComponent//alpha", "kos.tsic.top/SystemComponent//beta"}
	if !equal(sortedKeys(created), wantCreated) {
		t.Fatalf("planned creates = %v, want %v", sortedKeys(created), wantCreated)
	}
	if updates := plannedUpdates(t, plan); len(updates) != 0 {
		t.Fatalf("planned updates = %v, want none against an empty install", updates)
	}
	if deletes := plannedObjects(t, plan, "delete"); len(deletes) != 0 {
		t.Fatalf("planned deletes = %v, want none", sortedKeys(deletes))
	}
	assertRetainedVolume(t, plan)

	// Execute the same set: the plan the apply reports must be the one the plan
	// request announced.
	applyStatus, applied := gateway.Call(t, http.MethodPost, "/api/v1/apply-requests", first, nil)
	if applyStatus != http.StatusAccepted {
		t.Fatalf("apply request: status %d, body %v", applyStatus, applied)
	}
	if accepted, _ := applied["accepted"].(bool); !accepted {
		t.Fatalf("a write must answer with acceptance: %v", applied)
	}
	if _, present := applied["ref"]; present {
		t.Fatalf("an apply names its objects through the plan, not an empty reference: %v", applied["ref"])
	}
	appliedPlan := planBody(t, applied)
	if !equal(sortedKeys(plannedObjects(t, appliedPlan, "create")), sortedKeys(created)) {
		t.Fatalf("apply planned creates %v, the plan request announced %v",
			sortedKeys(plannedObjects(t, appliedPlan, "create")), sortedKeys(created))
	}
	harness.Eventually(t, settleTimeout, "the planned creates to exist", func() bool {
		return len(cluster.List(t, "v1beta1", "SystemComponent")) == len(created)
	})
	for _, name := range []string{"alpha", "beta"} {
		component := cluster.Get(t, "v1beta1", "SystemComponent", name)
		if component.GetLabels()["owner-uid"] != ownerUID {
			t.Fatalf("%s carries labels %v", name, component.GetLabels())
		}
	}

	// The second plan drops beta and changes one annotation of alpha.
	second := setRequest(object("alpha", map[string]any{"example.test/version": "v2"}))
	planStatus, plannedSecond := gateway.Call(t, http.MethodPost, "/api/v1/plan-requests", second, nil)
	if planStatus != http.StatusOK {
		t.Fatalf("second plan request: status %d, body %v", planStatus, plannedSecond)
	}
	secondPlan := planBody(t, plannedSecond)
	if creates := plannedObjects(t, secondPlan, "create"); len(creates) != 0 {
		t.Fatalf("second plan creates = %v, want none", sortedKeys(creates))
	}
	updates := plannedUpdates(t, secondPlan)
	if len(updates) != 1 {
		t.Fatalf("second plan updates = %v, want one", updates)
	}
	if changes, _ := updates[0]["changes"].([]any); len(changes) != 1 {
		t.Fatalf("second plan changes = %v, want one field", updates[0]["changes"])
	} else if change, _ := changes[0].(map[string]any); harness.String(change, "path") != "metadata.annotations.example.test/version" {
		t.Fatalf("second plan change = %v", change)
	}
	if deletes := plannedObjects(t, secondPlan, "delete"); !equal(sortedKeys(deletes), []string{"kos.tsic.top/SystemComponent//beta"}) {
		t.Fatalf("second plan deletes = %v, want beta", sortedKeys(deletes))
	}
	assertRetainedVolume(t, secondPlan)

	applyStatus, appliedSecond := gateway.Call(t, http.MethodPost, "/api/v1/apply-requests", second, nil)
	if applyStatus != http.StatusAccepted {
		t.Fatalf("second apply request: status %d, body %v", applyStatus, appliedSecond)
	}
	if !equal(sortedKeys(plannedObjects(t, planBody(t, appliedSecond), "update")), updateKeys(updates)) {
		t.Fatal("the second apply planned different updates than the plan request announced")
	}

	harness.Eventually(t, settleTimeout, "the planned update and delete to reach the cluster", func() bool {
		components := cluster.List(t, "v1beta1", "SystemComponent")
		if len(components) != 1 {
			return false
		}
		if components[0].GetName() != "alpha" {
			return false
		}
		return components[0].GetAnnotations()["example.test/version"] == "v2"
	})
	if volume := cluster.Get(t, "v1beta1", "Volume", "data"); volume.GetName() != "data" {
		t.Fatal("the retained volume disappeared")
	}
}

// setRequest builds a plan or apply body.
func setRequest(objects ...map[string]any) map[string]any {
	return map[string]any{
		"scope":   map[string]any{"ownerUID": ownerUID},
		"objects": objects,
	}
}

// object builds a desired system component of the install.
func object(name string, annotations map[string]any) map[string]any {
	manifest := harness.Manifest("SystemComponent", "", name,
		map[string]string{"managed-by": "kos-shell", "owner-uid": ownerUID}, nil)
	if annotations != nil {
		manifest["metadata"].(map[string]any)["annotations"] = annotations
	}
	return manifest
}

// planBody reads the plan out of a plan or apply response.
func planBody(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	plan, ok := body["plan"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no plan: %v", body)
	}
	return plan
}

// plannedObjects reads one deviation class and returns its references keyed by
// identity.
func plannedObjects(t *testing.T, plan map[string]any, class string) map[string]map[string]any {
	t.Helper()
	raw, ok := plan[class].([]any)
	if !ok {
		t.Fatalf("plan carries no %s list: %v", class, plan)
	}
	objects := make(map[string]map[string]any, len(raw))
	for _, item := range raw {
		parsed, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s entry is not an object: %v", class, item)
		}
		reference, ok := parsed["ref"].(map[string]any)
		if !ok {
			t.Fatalf("%s entry carries no reference: %v", class, parsed)
		}
		objects[harness.Reference(reference)] = parsed
	}
	return objects
}

// plannedUpdates reads the update class, which carries field changes.
func plannedUpdates(t *testing.T, plan map[string]any) []map[string]any {
	t.Helper()
	updates := plannedObjects(t, plan, "update")
	ordered := make([]map[string]any, 0, len(updates))
	for _, update := range updates {
		ordered = append(ordered, update)
	}
	sort.Slice(ordered, func(left, right int) bool {
		leftRef, _ := ordered[left]["ref"].(map[string]any)
		rightRef, _ := ordered[right]["ref"].(map[string]any)
		return harness.Reference(leftRef) < harness.Reference(rightRef)
	})
	return ordered
}

// assertRetainedVolume checks that the plan keeps the volume and warns about it.
func assertRetainedVolume(t *testing.T, plan map[string]any) {
	t.Helper()
	raw, ok := plan["warnings"].([]any)
	if !ok {
		t.Fatalf("plan carries no warnings: %v", plan)
	}
	for _, item := range raw {
		warning, _ := item.(map[string]any)
		if warning["code"] != "retained" {
			continue
		}
		if harness.String(warning, "object", "name") == "data" {
			return
		}
	}
	t.Fatalf("plan does not retain the volume: %v", raw)
}

// updateKeys names the updated objects in sorted order.
func updateKeys(updates []map[string]any) []string {
	keys := make([]string, 0, len(updates))
	for _, update := range updates {
		reference, _ := update["ref"].(map[string]any)
		keys = append(keys, harness.Reference(reference))
	}
	sort.Strings(keys)
	return keys
}

// sortedKeys returns the identities of a planned class in sorted order.
func sortedKeys(objects map[string]map[string]any) []string {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// snapshot renders every kernel object so two reads can be compared.
func snapshot(t *testing.T, cluster *harness.Cluster) []string {
	t.Helper()
	rendered := make([]string, 0)
	for _, kind := range kernelKinds() {
		for _, object := range cluster.List(t, "v1beta1", kind) {
			rendered = append(rendered, kind+": "+fmt.Sprint(object.Object))
		}
	}
	sort.Strings(rendered)
	return rendered
}

// kernelKinds lists the ten kernel kinds.
func kernelKinds() []string {
	return []string{
		"Application", "ApplicationDefinition", "BackupPolicy", "Endpoint", "NodeDevice",
		"Provider", "RestoreJob", "SystemComponent", "Volume", "VolumeMigration",
	}
}

// equal compares two ordered string lists.
func equal(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
