// Package tc8 verifies that a setting item disappears with the component that
// declared it and that its stored value survives until the component returns
// (AC-3, TC-8).
package tc8

import (
	"net/http"
	"testing"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	settingID   = "cluster-drain-timeout"
	extensionID = "demo.settings"
	ownerName   = "owner"
	ownerPass   = "owner-password"
)

// declaration declares one editable cluster setting.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: extensionID,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Settings: []extension.Setting{{
				ID: settingID, Scope: extension.SettingScopeCluster,
				Writer: extension.SettingWriterUser, Editable: true,
			}},
		},
	}
}

// TestTC8SettingDisappearsAndReturns checks the item is refused while the
// component is withdrawn and its value is back after the component returns.
func TestTC8SettingDisappearsAndReturns(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	token := shell.SignInOwner(t, ownerName, ownerPass)
	artifacts := extension.MemoryArtifacts{}
	installation := cluster.InstallPackageWith(t, "settings", declaration(), artifacts, shell.Retention())
	shell.Use(installation)

	status, body := shell.Call(t, http.MethodPut, "/api/v1/settings/"+settingID, map[string]any{"value": "900"}, token)
	if status != http.StatusOK {
		t.Fatalf("modify the setting = %d %v", status, body)
	}

	// Disabling the component withdraws its contributions; the setting is gone
	// and its API refuses the request instead of serving a stale item.
	cluster.DeleteComponent(t, "settings")
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))

	status, body = shell.Call(t, http.MethodGet, "/api/v1/settings", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list settings after the withdrawal = %d %v", status, body)
	}
	if items := harness.Items(t, body); len(items) != 0 {
		t.Fatalf("settings after the withdrawal = %v, want none", items)
	}
	status, body = shell.Call(t, http.MethodPut, "/api/v1/settings/"+settingID, map[string]any{"value": "1200"}, token)
	if status != http.StatusNotFound {
		t.Fatalf("modify a withdrawn setting = %d %v, want 404", status, body)
	}
	if code := harness.String(body, "error", "code"); code != controlplane.CodeNotFound {
		t.Fatalf("error code = %q, want %q", code, controlplane.CodeNotFound)
	}

	// Re-enabling the component brings the item back with the value written
	// before the withdrawal.
	installation = cluster.InstallPackageWith(t, "settings", declaration(), artifacts, shell.Retention())
	shell.Use(installation)
	status, body = shell.Call(t, http.MethodGet, "/api/v1/settings", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list settings after the return = %d %v", status, body)
	}
	items := harness.Items(t, body)
	if len(items) != 1 || harness.String(items[0], "id") != settingID {
		t.Fatalf("settings after the return = %v, want the declared item", items)
	}
	if value := harness.String(items[0], "value"); value != "900" {
		t.Fatalf("value after the return = %q, want the value kept across the withdrawal", value)
	}
}
