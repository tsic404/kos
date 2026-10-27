// Package tc37 verifies C-14's credential boundary on the catalogue metadata:
// the application definition carries the parameter and its sensitive marker
// but never the submitted value (AC-18, TC-37).
//
// The projection, log and store surfaces below belong to the sensitive-parameter
// case of AC-28 under the current design; they are kept here as the shared
// carrier until that case has its own executable. The diagnostics bundle is
// assembled from the same masked projections and the same store (its own export
// is AC-44), so a value absent here cannot appear in it.
package tc37

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// secrets are distinctive enough that a substring match is decisive.
const (
	storedSecret  = "s3cr3t-credential-value-9f"
	rewriteSecret = "an0ther-credential-value-7c"
)

// definition returns a definition with one sensitive field and one plain field.
func definition(t *testing.T) *v1beta1.ApplicationDefinition {
	t.Helper()
	definition := harness.Definition(t, "vault", nil)
	definition.Spec.Config = []v1beta1.ConfigField{
		{Name: "password", Type: "string", Sensitive: true},
		{Name: "user", Type: "string"},
	}
	digest, err := app.DefinitionDigest(definition.Spec)
	if err != nil {
		t.Fatalf("digest definition: %v", err)
	}
	definition.Spec.Package.Digest = digest
	if err := app.ValidateDefinition(definition.Spec); err != nil {
		t.Fatalf("definition is invalid: %v", err)
	}
	return definition
}

// configField returns the install parameter with that name from a definition
// read back through the control plane.
func configField(metadata map[string]any, name string) (map[string]any, bool) {
	fields, _ := harness.Nested(metadata, "object", "spec", "config").([]any)
	for _, item := range fields {
		field, _ := item.(map[string]any)
		if field["name"] == name {
			return field, true
		}
	}
	return nil, false
}

// TestTC37CredentialBoundary configures a sensitive value, then reads the
// catalog metadata and the presentation surfaces that must not carry it.
func TestTC37CredentialBoundary(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallDefinition(t, definition(t))

	system := cluster.StartAccess(t, nil, nil)
	owner := system.Owner(t, "owner", "owner-password")
	guest := system.Account(t, owner, "guest", "guest-password", "Guest")

	namespaceParam := "?namespace=" + harness.Namespace
	path := "/api/v1/resources/Application"
	body := harness.ApplicationManifest("vaulted", harness.Namespace, "vault", "apps",
		map[string]any{"password": storedSecret, "user": "svc"}, nil)
	if status, response := system.Call(t, owner, http.MethodPost, path+namespaceParam, body, nil); status != http.StatusAccepted {
		t.Fatalf("create the application = %d %v", status, response)
	}

	// The projection masks the value and still says it is set.
	status, raw, err := system.Identity.HTTP.Raw(http.MethodGet, path+"/vaulted"+namespaceParam, nil, harness.Bearer(owner))
	if err != nil || status != http.StatusOK {
		t.Fatalf("read the application = %d %v (%v)", status, raw, err)
	}
	if bytes.Contains(raw, []byte(storedSecret)) {
		t.Fatalf("the projection carried the credential value: %s", raw)
	}
	status, raw, err = system.Identity.HTTP.Raw(http.MethodGet, path+namespaceParam, nil, harness.Bearer(owner))
	if err != nil || status != http.StatusOK {
		t.Fatalf("list applications = %d %v (%v)", status, raw, err)
	}
	if bytes.Contains(raw, []byte(storedSecret)) {
		t.Fatalf("the list projection carried the credential value: %s", raw)
	}

	// A conflict answer carries the stored object, masked as well.
	rewrite := harness.ApplicationManifest("vaulted", harness.Namespace, "vault", "apps",
		map[string]any{"password": rewriteSecret, "user": "svc"}, nil)
	status, response := system.Call(t, owner, http.MethodPut, path+"/vaulted"+namespaceParam, rewrite,
		map[string]string{"If-Match": "1"})
	if status != http.StatusConflict {
		t.Fatalf("stale update = %d %v, want 409", status, response)
	}
	if bytes.Contains([]byte(harness.String(response, "error", "object", "spec", "config", "password")), []byte(storedSecret)) {
		t.Fatalf("the conflict answer carried the credential value: %v", response)
	}

	// The catalogue metadata is the surface this case owns: the catalogue sync
	// writes the application definition (C-03, C-24), so the parameter and its
	// sensitive marker must be readable there while the value stays out (C-14).
	status, raw, err = system.Identity.HTTP.Raw(http.MethodGet,
		"/api/v1/resources/ApplicationDefinition/vault", nil, harness.Bearer(owner))
	if err != nil || status != http.StatusOK {
		t.Fatalf("read the definition = %d %s (%v)", status, raw, err)
	}
	for _, secret := range []string{storedSecret, rewriteSecret} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("the catalogue metadata carried a credential value: %s", raw)
		}
	}
	metadata := map[string]any{}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("decode the definition %q: %v", raw, err)
	}
	parameter, found := configField(metadata, "password")
	if !found {
		t.Fatalf("the catalogue metadata lost the parameter: %s", raw)
	}
	if parameter["sensitive"] != true {
		t.Fatalf("the parameter lost its sensitive marker: %v", parameter)
	}

	// A refused write does not leak the value into the log either; the refusal
	// is what makes the log non-empty, so the absence below is a fact about the
	// lines that were written, not about a silent log.
	refused := harness.ApplicationManifest("refused", harness.Namespace, "vault", "apps",
		map[string]any{"password": rewriteSecret, "user": "svc"}, nil)
	if status, response := system.Call(t, guest, http.MethodPost, path+namespaceParam, refused, nil); status != http.StatusForbidden {
		t.Fatalf("guest write = %d %v, want 403", status, response)
	}
	if system.Logs.Len() == 0 {
		t.Fatal("the control plane logged nothing, so the credential absence below proves nothing")
	}
	for _, secret := range []string{storedSecret, rewriteSecret} {
		if system.Contains(secret) {
			t.Fatalf("the control-plane log carried a credential value: %s", system.Logs.String())
		}
	}

	// The control-plane store keeps no credential value: the export a backup or
	// a diagnostics bundle is built from cannot carry one.
	exporter := &identity.Exporter{
		Store:  system.Identity.Store,
		Target: filepath.Join(t.TempDir(), "backup"),
		Now:    system.Identity.Clock.Now,
	}
	result, err := exporter.Export(context.Background())
	if err != nil {
		t.Fatalf("export the database: %v", err)
	}
	exported, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}
	// The absence below is a fact about a non-empty export that carries the
	// accounts and the audit stream, not about a file holding nothing (AC-18).
	entries, err := system.Identity.Store.AuditEntries(context.Background(), 10)
	if err != nil {
		t.Fatalf("read the audit stream: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the control plane recorded no audit entry, so the export has none to carry")
	}
	if len(exported) == 0 {
		t.Fatal("the exported database is empty, so the credential absence below proves nothing")
	}
	for _, carried := range []string{"owner", "guest", entries[len(entries)-1].Object} {
		if !bytes.Contains(exported, []byte(carried)) {
			t.Fatalf("the export does not carry %q, so the credential absence below proves nothing", carried)
		}
	}
	for _, secret := range []string{storedSecret, rewriteSecret} {
		if bytes.Contains(exported, []byte(secret)) {
			t.Fatal("the control-plane database export carried a credential value")
		}
	}
}
