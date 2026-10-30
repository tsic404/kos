// Package tc14 verifies that a preference written by one account in one browser
// reads back in another browser of the same account, that undeclared keys are
// refused and stored unknown keys are ignored, that preferences stay out of the
// object projection, and that both the accepted and the refused write leave one
// audit record in the same trail (AC-7, AC-18, TC-14).
package tc14

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	extensionID = "demo.dock"
	ownerName   = "owner"
	ownerPass   = "owner-password"
	preferences = "/api/v1/preferences/" + extensionID
)

// declaration declares the preference keys of a desktop extension.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: extensionID,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Preferences: []extension.PreferenceSchema{{
				ID: "dock", Keys: []string{"view", "order"},
			}},
		},
	}
}

// TestTC14PreferenceFollowsTheAccount signs in twice, writes with one session
// and reads with the other, then checks the object projection is untouched.
func TestTC14PreferenceFollowsTheAccount(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	first := shell.SignInOwner(t, ownerName, ownerPass)
	second, _ := shell.Accounts.SignIn(t, ownerName, ownerPass)
	artifacts := extension.MemoryArtifacts{}
	installation := cluster.InstallPackageWith(t, "dock", declaration(), artifacts, shell.Retention())
	shell.Use(installation)

	// One application gives the projection something that must not change.
	status, body := shell.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.ApplicationManifest("shell", harness.Namespace, "", harness.Namespace, nil, nil), first)
	if status != http.StatusAccepted {
		t.Fatalf("create the application = %d %v", status, body)
	}
	before := cluster.Get(t, "v1beta1", "Application", "shell")

	status, body = shell.Call(t, http.MethodPut, preferences+"/view", map[string]any{"value": "grid"}, first)
	if status != http.StatusNoContent {
		t.Fatalf("write the preference = %d %v", status, body)
	}
	// The accepted write left one record in the same trail the settings domain
	// writes to (C-14).
	entries := preferenceAudit(t, shell)
	if len(entries) != 1 {
		t.Fatalf("audit after one accepted write = %+v, want one record", entries)
	}
	requireAudit(t, entries[0], "kos.tsic.top/Preference/"+extensionID+"/view", controlplane.ResultAccepted)
	status, body = shell.Call(t, http.MethodGet, preferences, nil, second)
	if status != http.StatusOK {
		t.Fatalf("read the preference in another browser = %d %v", status, body)
	}
	if view := harness.String(body, "items", "view"); view != "grid" {
		t.Fatalf("view = %q, want the value written by the first browser", view)
	}

	// A key the extension did not declare is refused on write and a stored key
	// outside the schema is ignored on read (C-27).
	status, body = shell.Call(t, http.MethodPut, preferences+"/unknown", map[string]any{"value": "x"}, first)
	if status != http.StatusBadRequest {
		t.Fatalf("write an undeclared key = %d %v, want 400", status, body)
	}
	if code := harness.String(body, "error", "code"); code != controlplane.CodeInvalidField {
		t.Fatalf("error code = %q, want %q", code, controlplane.CodeInvalidField)
	}
	// The refused write left its own record, and neither write left a second
	// one.
	entries = preferenceAudit(t, shell)
	if len(entries) != 2 {
		t.Fatalf("audit after one accepted and one refused write = %+v, want two records", entries)
	}
	requireAudit(t, entries[1], "kos.tsic.top/Preference/"+extensionID+"/unknown", controlplane.ResultRejected)

	// A key the audit record cannot hold is refused as invalid input and leaves
	// the trail unchanged, so a malformed request cannot grow it (C-14).
	status, body = shell.Call(t, http.MethodPut,
		preferences+"/"+strings.Repeat("a", controlplane.MaxRecordNameLength+1),
		map[string]any{"value": "x"}, first)
	if status != http.StatusBadRequest {
		t.Fatalf("write an over-long preference key = %d %v, want 400", status, body)
	}
	if entries := preferenceAudit(t, shell); len(entries) != 2 {
		t.Fatalf("audit after an over-long preference key = %+v, want the trail unchanged", entries)
	}
	if err := shell.Accounts.Store.SetScopedPreference(context.Background(), ownerName, extensionID, "dropped", "x"); err != nil {
		t.Fatalf("store an unknown key: %v", err)
	}
	status, body = shell.Call(t, http.MethodGet, preferences, nil, second)
	if status != http.StatusOK {
		t.Fatalf("read preferences again = %d %v", status, body)
	}
	if stored := harness.String(body, "items", "dropped"); stored != "" {
		t.Fatalf("unknown key came back as %q, want it ignored", stored)
	}

	// The application object is unchanged and carries no preference value.
	after := cluster.Get(t, "v1beta1", "Application", "shell")
	if after.GetResourceVersion() != before.GetResourceVersion() {
		t.Fatalf("application resourceVersion = %q, want %q", after.GetResourceVersion(), before.GetResourceVersion())
	}
	status, body = shell.Call(t, http.MethodGet, "/api/v1/resources/Application?namespace="+harness.Namespace, nil, second)
	if status != http.StatusOK {
		t.Fatalf("read the application projection = %d %v", status, body)
	}
	if strings.Contains(fmt.Sprint(body), "grid") {
		t.Fatalf("preference appears in the object projection: %v", body)
	}
}

// preferenceAudit returns the audit records of preference writes, oldest first.
func preferenceAudit(t *testing.T, shell *harness.Shell) []identity.AuditEntry {
	t.Helper()
	entries, err := shell.Accounts.Store.AuditEntries(context.Background(), 50)
	if err != nil {
		t.Fatalf("read the audit trail: %v", err)
	}
	writes := make([]identity.AuditEntry, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Object, "kos.tsic.top/Preference/") {
			writes = append(writes, entry)
		}
	}
	return writes
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
