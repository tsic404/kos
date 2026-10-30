// Package tc7 verifies that a declared setting item is readable with its
// current value, source and modification entry, that a write changes the
// current value and leaves one audit record, and that a refused write leaves a
// refused record in the same trail (AC-3, AC-18, TC-7).
package tc7

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
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
	// The accepted write left exactly one record naming the operator, the item
	// and the outcome (C-14).
	entries := settingAudit(t, shell)
	if len(entries) != 1 {
		t.Fatalf("audit after one accepted write = %+v, want one record", entries)
	}
	requireAudit(t, entries[0], "kos.tsic.top/Setting/"+settingID, controlplane.ResultAccepted)

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
	// The refused write left its own record: a rejection is as visible as an
	// accepted write, and neither write left a second one.
	entries = settingAudit(t, shell)
	if len(entries) != 2 {
		t.Fatalf("audit after one accepted and one refused write = %+v, want two records", entries)
	}
	requireAudit(t, entries[1], "kos.tsic.top/Setting/maintenance-owner", controlplane.ResultRejected)

	// An item id the audit record cannot hold is refused as invalid input and
	// leaves the trail unchanged, so a malformed request cannot grow it (C-14).
	status, body = shell.Call(t, http.MethodPut,
		"/api/v1/settings/"+strings.Repeat("a", controlplane.MaxRecordNameLength+1),
		map[string]any{"value": "1"}, token)
	if status != http.StatusBadRequest {
		t.Fatalf("write an over-long item id = %d %v, want 400", status, body)
	}
	if entries := settingAudit(t, shell); len(entries) != 2 {
		t.Fatalf("audit after an over-long item id = %+v, want the trail unchanged", entries)
	}
}

// settingAudit returns the audit records of setting writes, oldest first.
func settingAudit(t *testing.T, shell *harness.Shell) []identity.AuditEntry {
	t.Helper()
	entries, err := shell.Accounts.Store.AuditEntries(context.Background(), 50)
	if err != nil {
		t.Fatalf("read the audit trail: %v", err)
	}
	settings := make([]identity.AuditEntry, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Object, "kos.tsic.top/Setting/") {
			settings = append(settings, entry)
		}
	}
	return settings
}

// requireAudit checks that one record names the operator, the object, the write
// verb and the outcome, and carries the surface and the time (C-14).
func requireAudit(t *testing.T, entry identity.AuditEntry, object, result string) {
	t.Helper()
	if entry.Actor != ownerName {
		t.Fatalf("audit actor = %q, want the signed-in account %q", entry.Actor, ownerName)
	}
	if entry.Object != object {
		t.Fatalf("audit object = %q, want %q", entry.Object, object)
	}
	if entry.Verb != "set" {
		t.Fatalf("audit verb = %q, want the write verb", entry.Verb)
	}
	if entry.Result != result {
		t.Fatalf("audit result = %q, want %q", entry.Result, result)
	}
	if entry.Source != "api" {
		t.Fatalf("audit source = %q, want the control-plane API", entry.Source)
	}
	if entry.OccurredAt.IsZero() {
		t.Fatalf("audit record = %+v, want the time it happened", entry)
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
