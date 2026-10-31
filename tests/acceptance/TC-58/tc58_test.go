// Package tc58 verifies the sensitive install parameter of AC-28: after a value
// is submitted, the application projection, the control-plane log of the
// install and the diagnostic bundle carry no value, the catalogue metadata keeps
// the marker without the value, and the projection only says it is set
// (AC-28, TC-58).
package tc58

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	extensionID = "acceptance.appstore"
	sourceID    = "acceptance-catalogue"
	entryID     = "vaulted"
	ownerName   = "owner"
	ownerPass   = "owner-password"
	// secret is distinctive enough that a substring match is decisive.
	secret = "sensitive-install-value-4d1c"
)

// template renders one ConfigMap in the declared namespace.
const template = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  mode: "acceptance"
`

// definition carries one sensitive config field and one plain field, so the case
// can show the mask next to a value that stays readable (C-32).
func definition(t *testing.T) v1beta1.ApplicationDefinitionSpec {
	t.Helper()
	spec := v1beta1.ApplicationDefinitionSpec{
		Package: v1beta1.PackageIdentity{Version: "1.0.0"},
		Config: []v1beta1.ConfigField{
			{Name: "password", Type: "string", Sensitive: true},
			{Name: "user", Type: "string"},
		},
		Template: template,
	}
	digest, err := app.DefinitionDigest(spec)
	if err != nil {
		t.Fatalf("digest the definition: %v", err)
	}
	spec.Package.Digest = digest
	if err := app.ValidateDefinition(spec); err != nil {
		t.Fatalf("the definition is invalid: %v", err)
	}
	return spec
}

// declaration offers the definition as one catalogue entry.
func declaration(spec v1beta1.ApplicationDefinitionSpec) extension.Declaration {
	return extension.Declaration{
		Identifier: extensionID,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			CatalogueSources: []extension.CatalogueSource{{
				ID:   sourceID,
				Sync: "pull",
				Entries: []extension.CatalogueEntry{{
					ID:         entryID,
					Display:    extension.CatalogueDisplay{Name: "Vaulted"},
					Definition: spec,
				}},
			}},
		},
	}
}

// TestTC58SensitiveParameterIsNotReadBack installs through the catalogue and
// reads every surface C-14 names for a sensitive parameter.
func TestTC58SensitiveParameterIsNotReadBack(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	token := shell.SignInOwner(t, ownerName, ownerPass)
	artifacts := extension.MemoryArtifacts{}
	installation := cluster.InstallPackageWith(t, "appstore", declaration(definition(t)), artifacts, shell.Retention())
	shell.Use(installation)
	shell.SyncCatalogue(t)

	// The catalogue metadata carries the parameter and its marker, never a value.
	status, raw, err := shell.Accounts.HTTP.Raw(http.MethodGet, "/api/v1/catalogue/entries", nil, harness.Bearer(token))
	if err != nil || status != http.StatusOK {
		t.Fatalf("read the catalogue = %d %s (%v)", status, raw, err)
	}
	if !bytes.Contains(raw, []byte(`"sensitive":true`)) {
		t.Fatalf("the catalogue lost the sensitive marker: %s", raw)
	}

	namespaceParam := "?namespace=" + harness.Namespace
	manifest := harness.ApplicationManifest("vaulted", harness.Namespace, entryID, harness.Namespace,
		map[string]any{"password": secret, "user": "svc"}, nil)
	if status, response := shell.Call(t, http.MethodPost, "/api/v1/resources/Application"+namespaceParam, manifest, token); status != http.StatusAccepted {
		t.Fatalf("install the entry = %d %v", status, response)
	}

	// The projection masks the value and still says it is set; the plain field
	// stays readable.
	status, raw, err = shell.Accounts.HTTP.Raw(http.MethodGet, "/api/v1/resources/Application/vaulted"+namespaceParam, nil, harness.Bearer(token))
	if err != nil || status != http.StatusOK {
		t.Fatalf("read the application = %d %s (%v)", status, raw, err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("the projection carried the sensitive value: %s", raw)
	}
	status, body := shell.Call(t, http.MethodGet, "/api/v1/resources/Application"+namespaceParam, nil, token)
	if status != http.StatusOK {
		t.Fatalf("list applications = %d %v", status, body)
	}
	items := harness.Items(t, body)
	if len(items) != 1 {
		t.Fatalf("applications = %v, want the one installed application", items)
	}
	if mask := harness.String(items[0], "object", "spec", "config", "password"); mask != "***" {
		t.Fatalf("password = %q, want the mask that says it is set", mask)
	}
	if user := harness.String(items[0], "object", "spec", "config", "user"); user != "svc" {
		t.Fatalf("user = %q, want the plain value readable", user)
	}

	// A refused write makes the control-plane log non-empty; the install itself
	// and that refusal must not carry the value.
	guest := shell.Account(t, token, "guest", "guest-password", write.RoleGuest)
	if status, response := shell.Accounts.HTTP.Call(t, http.MethodPost, "/api/v1/resources/Application"+namespaceParam,
		harness.ApplicationManifest("refused", harness.Namespace, entryID, harness.Namespace,
			map[string]any{"password": secret, "user": "svc"}, nil),
		map[string]string{"Authorization": "Bearer " + guest}); status != http.StatusForbidden {
		t.Fatalf("guest install = %d %v, want 403", status, response)
	}
	if shell.Logs.Len() == 0 {
		t.Fatal("the control plane logged nothing, so the sensitivity absence below proves nothing")
	}
	if bytes.Contains(shell.Logs.Bytes(), []byte(secret)) {
		t.Fatalf("the control-plane log carried the sensitive value: %s", shell.Logs.String())
	}

	// The diagnostic bundle carries the condition snapshot and the render diff
	// of the application, and no value of the sensitive parameter.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bundle, err := cluster.DiagnosticService(installation).Bundle(ctx, harness.Namespace, "vaulted")
	if err != nil {
		t.Fatalf("assemble the diagnostic bundle: %v", err)
	}
	encoded, err := bundle.Encode()
	if err != nil {
		t.Fatalf("encode the diagnostic bundle: %v", err)
	}
	if bundle.Application.Name != "vaulted" || len(bundle.ConditionSnapshot) == 0 {
		t.Fatalf("the bundle is not about the installed application: %+v", bundle.Application)
	}
	if !bytes.Contains(encoded, []byte("vaulted")) {
		t.Fatal("the bundle carries no trace of the application, so the absence below proves nothing")
	}
	if bytes.Contains(encoded, []byte(secret)) {
		t.Fatalf("the diagnostic bundle carried the sensitive value: %s", encoded)
	}
}
