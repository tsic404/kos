// A data format declaration that disappears is a change like any other: the
// upgrade is refused, because the running version wrote its data under the
// declaration the new package no longer names (AC-26, TC-52).
package tc52

import (
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC52DroppedDataFormatDeclarationIsRefused(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)

	first := harness.DefinitionContent(t, "web-one", "1", template("one"), nil, nil)
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

	requestUpgrade(t, cluster, gateway, second.Name)
	reconcileOnce(t, controller)

	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want the pre-upgrade %q", phase, app.PhaseReady)
	}
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q, want the refused target to be unapplied", version)
	}
	condition := conditionOf(t, cluster, app.ConditionIrreversibleUpgrade)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("the irreversible condition is missing: %+v", condition)
	}
	items, _ := statusMap(t, cluster, "upgrade")["irreversible"].([]any)
	if len(items) != 1 {
		t.Fatalf("irreversible = %v, want the dropped declaration", items)
	}
	item, _ := items[0].(map[string]any)
	if item["kind"] != "dataFormat" {
		t.Fatalf("irreversible item = %v, want dataFormat", item)
	}
	if detail, _ := item["detail"].(string); detail == "" {
		t.Fatal("the refusal does not say which declaration disappeared")
	}
}
