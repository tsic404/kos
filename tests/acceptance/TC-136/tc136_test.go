// Package tc136 verifies the maintenance window as a policy item: it is a
// cluster-scoped setting whose current value comes from the control-plane
// database and whose writer is the user, it reads empty before any write, and it
// reflects the written window afterwards (AC-65, TC-136).
package tc136

import (
	"net/http"
	"testing"

	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/settings/kossettings"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	ownerName = "owner"
	ownerPass = "owner-password"
)

// TestTC136MaintenanceWindowItem reads the window item before and after a write
// and checks its scope, source, writer and entry.
func TestTC136MaintenanceWindowItem(t *testing.T) {
	cluster := harness.Start(t)
	readings := &kossettings.Readings{Dynamic: cluster.Dynamic}
	shell := cluster.StartShell(t, func(config *pages.Config) {
		config.Readings = pages.Router{kossettings.Identifier: readings}
	})
	readings.Settings = shell.Settings
	installation := cluster.InstallPackageWith(t, "settings", kossettings.Declaration(), extension.MemoryArtifacts{}, shell.Retention())
	shell.Use(installation)
	token := shell.SignInOwner(t, ownerName, ownerPass)

	// The product declares the window as a cluster-scoped policy item: the user
	// writes it, the database holds the value, and an entry is offered (C-72).
	_, body := shell.Call(t, http.MethodGet, "/api/v1/settings", nil, token)
	item := findItem(t, body, kossettings.ClusterMaintenanceWindow)
	if scope := harness.String(item, "scope"); scope != "Cluster" {
		t.Fatalf("window scope = %q, want Cluster", scope)
	}
	if writer := harness.String(item, "writer"); writer != "User" {
		t.Fatalf("window writer = %q, want User", writer)
	}
	if source := harness.String(item, "source"); source != "User" {
		t.Fatalf("window source = %q, want the user writer", source)
	}
	if editable, _ := item["editable"].(bool); !editable {
		t.Fatalf("window item = %v, want it editable", item)
	}
	if entry := harness.String(item, "entry", "path"); entry == "" {
		t.Fatalf("window item = %v, want a modification entry", item)
	}
	// Nothing is written yet: the item reads empty rather than failing, which is
	// the unset state the policy leaves unrestricted (C-72).
	if value := harness.String(item, "value"); value != "" {
		t.Fatalf("window value = %q, want the unwritten state", value)
	}

	// The entry the item renders is the request that writes it.
	entry := harness.String(item, "entry", "path")
	status, body := shell.Call(t, http.MethodPut, entry, map[string]any{"value": "Sun 02:00-04:00"}, token)
	if status != http.StatusOK {
		t.Fatalf("write the window = %d %v", status, body)
	}
	_, body = shell.Call(t, http.MethodGet, "/api/v1/settings", nil, token)
	if value := harness.String(findItem(t, body, kossettings.ClusterMaintenanceWindow), "value"); value != "Sun 02:00-04:00" {
		t.Fatalf("window value = %q, want the written window", value)
	}

	// The settings page reads the same value from the database source.
	status, body = shell.Call(t, http.MethodGet, "/api/v1/pages/"+kossettings.PageSettings, nil, token)
	if status != http.StatusOK {
		t.Fatalf("read the settings page = %d %v", status, body)
	}
	for _, raw := range itemsOf(t, body) {
		if harness.String(raw, "id") != kossettings.ClusterMaintenanceWindow {
			continue
		}
		if source := harness.String(raw, "source"); source != "database" {
			t.Fatalf("window page source = %q, want database", source)
		}
		if value := harness.String(raw, "value"); value != "Sun 02:00-04:00" {
			t.Fatalf("window page value = %q, want the written window", value)
		}
		return
	}
	t.Fatalf("the settings page carries no window item: %v", body["items"])
}

// findItem returns one item of the settings response.
func findItem(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()
	for _, item := range itemsOf(t, body) {
		if harness.String(item, "id") == id {
			return item
		}
	}
	t.Fatalf("setting %q is absent from %v", id, body["items"])
	return nil
}

// itemsOf reads the item list of a settings or page response.
func itemsOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	items, _ := body["items"].([]any)
	parsed := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		parsed = append(parsed, item)
	}
	return parsed
}
