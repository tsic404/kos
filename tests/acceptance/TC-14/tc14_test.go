// Package tc14 verifies that a preference written by one account in one browser
// reads back in another browser of the same account, that undeclared keys are
// refused and stored unknown keys are ignored, and that preferences stay out of
// the object projection (AC-7, TC-14).
package tc14

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
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
