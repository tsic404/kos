// Package tc7 verifies that a declared setting item is readable with its
// current value, source and modification entry, and that a write changes the
// current value (AC-3, TC-7).
package tc7

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

// declaration declares one editable cluster setting and one the kernel writes.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: extensionID,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Settings: []extension.Setting{
				{ID: settingID, Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser, Editable: true},
				{ID: "maintenance-owner", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterKernel},
			},
		},
	}
}

// TestTC7SettingIsReadableAndWritable reads one item, modifies it and reads it
// back, and checks the read-only item has a reason and no entry.
func TestTC7SettingIsReadableAndWritable(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	token := shell.SignInOwner(t, ownerName, ownerPass)
	artifacts := extension.MemoryArtifacts{}
	installation := cluster.InstallPackageWith(t, "settings", declaration(), artifacts, shell.Retention())
	shell.Use(installation)

	status, body := shell.Call(t, http.MethodGet, "/api/v1/settings", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list settings = %d %v", status, body)
	}
	item := findSetting(t, body, settingID)
	if value := harness.String(item, "value"); value != "" {
		t.Fatalf("current value = %q, want empty before the first write", value)
	}
	if source := harness.String(item, "source"); source != extension.SettingWriterUser {
		t.Fatalf("source = %q, want the declared writer %q", source, extension.SettingWriterUser)
	}
	if entry := harness.String(item, "entry", "path"); entry != "/api/v1/settings/"+settingID {
		t.Fatalf("entry = %q, want the modification path of the item", entry)
	}
	if editable, _ := item["editable"].(bool); !editable {
		t.Fatalf("item = %v, want editable", item)
	}

	status, body = shell.Call(t, http.MethodPut, "/api/v1/settings/"+settingID, map[string]any{"value": "600"}, token)
	if status != http.StatusOK {
		t.Fatalf("modify setting = %d %v", status, body)
	}
	if value := harness.String(body, "value"); value != "600" {
		t.Fatalf("answer value = %q, want the written value", value)
	}

	status, body = shell.Call(t, http.MethodGet, "/api/v1/settings", nil, token)
	if status != http.StatusOK {
		t.Fatalf("read settings again = %d %v", status, body)
	}
	if value := harness.String(findSetting(t, body, settingID), "value"); value != "600" {
		t.Fatalf("current value = %q, want the written value", value)
	}

	// The kernel-written item is readable but not modifiable: it names the
	// writer and carries a reason instead of an entry.
	readOnly := findSetting(t, body, "maintenance-owner")
	if reason := harness.String(readOnly, "reason"); reason == "" {
		t.Fatalf("read-only item = %v, want a reason", readOnly)
	}
	if entry := harness.Nested(readOnly, "entry"); entry != nil {
		t.Fatalf("read-only item = %v, want no modification entry", readOnly)
	}
	status, body = shell.Call(t, http.MethodPut, "/api/v1/settings/maintenance-owner",
		map[string]any{"value": "x"}, token)
	if status != http.StatusBadRequest {
		t.Fatalf("modify a kernel-written item = %d %v, want 400", status, body)
	}
	if code := harness.String(body, "error", "code"); code != controlplane.CodeInvalidField {
		t.Fatalf("error code = %q, want %q", code, controlplane.CodeInvalidField)
	}
}

// findSetting returns one item of a settings response.
func findSetting(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()
	for _, item := range harness.Items(t, body) {
		if harness.String(item, "id") == id {
			return item
		}
	}
	t.Fatalf("setting %q is absent from %v", id, body)
	return nil
}
