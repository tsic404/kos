// Package tc57 verifies the install and upgrade write paths of AC-28: a
// catalogue entry whose definition declares a capability nothing provides is
// refused and the missing capability is listed, a package that no longer matches
// its digest is refused as a rejected value rather than a missing extension, a
// config that misses a required field or leaves an enum is refused with the
// field and its constraint named, and no application object is produced. Only a
// write that creates the object is an install: an application whose entry became
// uninstallable stays updatable (AC-28, TC-57).
package tc57

import (
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/appstore"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	extensionID = "acceptance.appstore"
	sourceID    = "acceptance-catalogue"
	absentEntry = "needs-absent"
	offerEntry  = "wiki"
	// tamperedEntry offers a package whose content no longer matches its digest.
	tamperedEntry = "tampered"
	ownerName     = "owner"
	ownerPass     = "owner-password"
	// absentCapability names a capability no declaration or baseline provides.
	absentCapability = "acceptance.absent"
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

// definition builds one entry's definition with its digest computed from the
// content, as C-32 requires.
func definition(t *testing.T, fields []v1beta1.ConfigField, requires []v1beta1.CapabilityRequirement) v1beta1.ApplicationDefinitionSpec {
	t.Helper()
	spec := v1beta1.ApplicationDefinitionSpec{
		Package:  v1beta1.PackageIdentity{Version: "1.0.0"},
		Config:   fields,
		Template: template,
		Requires: requires,
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

// declaration offers both entries from one source.
func declaration(offers ...extension.CatalogueEntry) extension.Declaration {
	return extension.Declaration{
		Identifier: extensionID,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			CatalogueSources: []extension.CatalogueSource{{ID: sourceID, Sync: "pull", Entries: offers}},
		},
	}
}

// entryNamed returns one catalogue entry by identity.
func entryNamed(t *testing.T, entries []map[string]any, id string) map[string]any {
	t.Helper()
	for _, entry := range entries {
		if harness.String(entry, "id") == id {
			return entry
		}
	}
	t.Fatalf("the catalogue offers no entry %q: %v", id, entries)
	return nil
}

// pageNamed returns one page by identity.
func pageNamed(t *testing.T, pages []map[string]any, id string) (map[string]any, bool) {
	t.Helper()
	for _, page := range pages {
		if harness.String(page, "id") == id {
			return page, true
		}
	}
	return nil, false
}

// itemNamed returns one rendered page item by identity.
func itemNamed(t *testing.T, items []any, id string) (map[string]any, bool) {
	t.Helper()
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if harness.String(item, "id") == id {
			return item, true
		}
	}
	return nil, false
}

// valueEntry returns one catalogue entry inside a rendered page value.
func valueEntry(t *testing.T, values []any, id string) (map[string]any, bool) {
	t.Helper()
	for _, raw := range values {
		entry, _ := raw.(map[string]any)
		if harness.String(entry, "id") == id {
			return entry, true
		}
	}
	return nil, false
}

// contains reports whether the decoded list carries value.
func contains(values []any, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// TestTC57MissingDependencyAndOutOfRangeConfig drives both refusals C-31 and
// C-32 name, then checks the upgrade write applies the same structure.
func TestTC57MissingDependencyAndOutOfRangeConfig(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	token := shell.SignInOwner(t, ownerName, ownerPass)
	artifacts := extension.MemoryArtifacts{}

	// The shipped kos-appstore package installs as a component and contributes
	// the page this case installs from (C-31).
	shipped, err := appstore.Package()
	if err != nil {
		t.Fatalf("build the shipped appstore package: %v", err)
	}
	artifacts[shipped.Reference] = shipped.Content
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component("appstore", shipped.Reference, shipped.Digest)})

	missing := definition(t, nil, []v1beta1.CapabilityRequirement{{Name: absentCapability, Version: "v1"}})
	installable := definition(t, []v1beta1.ConfigField{
		{Name: "mode", Type: "enum", Enum: []string{"dev", "prod"}, Required: true},
		{Name: "replicas", Type: "integer", Required: true, Minimum: new("2"), Maximum: new("4")},
	}, nil)
	// One entry's packaged content no longer matches its digest: C-24 refuses it,
	// and the refusal must not be reported as a missing extension (C-12).
	tampered := definition(t, nil, nil)
	tampered.Template = template + "data:\n  extra: \"x\"\n"
	installation := cluster.InstallPackageWith(t, "acceptance-source", declaration(
		extension.CatalogueEntry{ID: absentEntry, Display: extension.CatalogueDisplay{Name: "Needs Absent"}, Definition: missing},
		extension.CatalogueEntry{ID: offerEntry, Display: extension.CatalogueDisplay{Name: "Wiki"}, Definition: installable},
		extension.CatalogueEntry{ID: tamperedEntry, Display: extension.CatalogueDisplay{Name: "Tampered"}, Definition: tampered},
	), artifacts, shell.Retention())
	shell.Use(installation)
	shell.SyncCatalogue(t)

	// The app center page is the install entry C-31 describes: its two items
	// name the install and upgrade writes.
	status, body := shell.Call(t, http.MethodGet, "/api/v1/pages", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list pages = %d %v", status, body)
	}
	page, found := pageNamed(t, harness.Items(t, body), appstore.PageID)
	if !found {
		t.Fatalf("pages = %v, want the app center page", body)
	}
	if title := harness.String(page, "title"); title == "" {
		t.Fatalf("page = %v, want a title", page)
	}
	if actions, _ := page["actions"].([]any); len(actions) != 2 {
		t.Fatalf("page = %v, want the install and upgrade entries", page)
	}

	// The page read resolves the catalogue entries, and each entry carries the
	// install parameter structure the form fields are generated from (C-32).
	status, body = shell.Call(t, http.MethodGet, "/api/v1/pages/"+appstore.PageID, nil, token)
	if status != http.StatusOK {
		t.Fatalf("read the app center page = %d %v", status, body)
	}
	rendered, _ := harness.Nested(body, "items").([]any)
	entriesItem, found := itemNamed(t, rendered, appstore.EntriesItem)
	if !found {
		t.Fatalf("page items = %v, want the entries item", rendered)
	}
	offers, _ := entriesItem["value"].([]any)
	shown, found := valueEntry(t, offers, offerEntry)
	if !found {
		t.Fatalf("entries item = %v, want the offered entry", entriesItem["value"])
	}
	fields, _ := shown["config"].([]any)
	names := map[string]map[string]any{}
	for _, raw := range fields {
		field, _ := raw.(map[string]any)
		names[harness.String(field, "name")] = field
	}
	mode := names["mode"]
	if mode == nil || mode["required"] != true {
		t.Fatalf("the form structure lost the required mode field: %v", fields)
	}
	if accepted, _ := mode["enum"].([]any); len(accepted) != 2 {
		t.Fatalf("the form structure lost the mode values: %v", mode)
	}
	if replicas := names["replicas"]; replicas == nil || harness.String(replicas, "maximum") != "4" {
		t.Fatalf("the form structure lost the replicas range: %v", fields)
	}

	// The entry read model already reports the missing capability and the
	// reason it cannot be installed.
	status, body = shell.Call(t, http.MethodGet, "/api/v1/catalogue/entries", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list entries = %d %v", status, body)
	}
	entries := harness.Items(t, body)
	refused := entryNamed(t, entries, absentEntry)
	if installable, _ := refused["installable"].(bool); installable {
		t.Fatalf("entry = %v, want not installable while its capability is absent", refused)
	}
	if reason := harness.String(refused, "reason"); !strings.Contains(reason, absentCapability) {
		t.Fatalf("reason = %q, want the missing capability named", reason)
	}
	listed, _ := refused["missing"].([]any)
	if !contains(listed, absentCapability) {
		t.Fatalf("missing = %v, want %q listed", refused["missing"], absentCapability)
	}
	if other := entryNamed(t, entries, offerEntry); !other["installable"].(bool) {
		t.Fatalf("entry = %v, want the installable offer", other)
	}

	namespaceParam := "?namespace=" + harness.Namespace
	installPath := "/api/v1/resources/Application" + namespaceParam

	// Installing the entry is refused before anything is written, and the
	// sync never wrote a definition for an entry it judged uninstallable.
	absent := harness.ApplicationManifest("absent-app", harness.Namespace, absentEntry, harness.Namespace, nil, nil)
	status, response := shell.Call(t, http.MethodPost, installPath, absent, token)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("install the entry with a missing capability = %d %v, want 422", status, response)
	}
	if code := harness.String(response, "error", "code"); code != "capability_missing" {
		t.Fatalf("code = %q, want capability_missing", code)
	}
	if message := harness.String(response, "error", "message"); !strings.Contains(message, absentCapability) {
		t.Fatalf("message = %q, want the missing capability named", message)
	}
	if status, response := shell.Call(t, http.MethodGet, "/api/v1/resources/ApplicationDefinition/"+absentEntry, nil, token); status != http.StatusNotFound {
		t.Fatalf("read the definition of the refused entry = %d %v, want 404", status, response)
	}
	assertNoApplications(t, shell, token)

	// A package whose content no longer matches its digest is not a missing
	// extension: the refusal names the rejected value and keeps the two codes
	// apart (C-12, C-24).
	tamperedApp := harness.ApplicationManifest("tampered-app", harness.Namespace, tamperedEntry, harness.Namespace, nil, nil)
	status, response = shell.Call(t, http.MethodPost, installPath, tamperedApp, token)
	if status != http.StatusBadRequest {
		t.Fatalf("install from a digest-mismatched entry = %d %v, want 400", status, response)
	}
	if code := harness.String(response, "error", "code"); code != "invalid_field" {
		t.Fatalf("code = %q, want invalid_field: a broken package is not a missing capability", code)
	}
	if stage := harness.String(response, "error", "stage"); stage != "validate" {
		t.Fatalf("stage = %q, want validate", stage)
	}
	if message := harness.String(response, "error", "message"); !strings.Contains(message, "摘要") {
		t.Fatalf("message = %q, want the package refusal named", message)
	}
	assertNoApplications(t, shell, token)

	// A submission that misses a required field and leaves the declared range is
	// refused with both fields and their constraints named.
	outOfRange := harness.ApplicationManifest("wiki-bad", harness.Namespace, offerEntry, harness.Namespace,
		map[string]any{"mode": "staging", "replicas": 9}, nil)
	status, response = shell.Call(t, http.MethodPost, installPath, outOfRange, token)
	if status != http.StatusBadRequest {
		t.Fatalf("install with an out-of-range config = %d %v, want 400", status, response)
	}
	if code := harness.String(response, "error", "code"); code != "invalid_field" {
		t.Fatalf("code = %q, want invalid_field", code)
	}
	message := harness.String(response, "error", "message")
	for _, expected := range []string{"mode", "dev", "prod", "replicas", "maximum 4"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("message = %q, want %q named", message, expected)
		}
	}
	missingRequired := harness.ApplicationManifest("wiki-empty", harness.Namespace, offerEntry, harness.Namespace,
		map[string]any{"replicas": 3}, nil)
	status, response = shell.Call(t, http.MethodPost, installPath, missingRequired, token)
	if status != http.StatusBadRequest {
		t.Fatalf("install with a missing required field = %d %v, want 400", status, response)
	}
	if message := harness.String(response, "error", "message"); !strings.Contains(message, "mode") || !strings.Contains(message, "required") {
		t.Fatalf("message = %q, want the required field named", message)
	}
	assertNoApplications(t, shell, token)

	// The upgrade write applies the same structure: a valid install is accepted,
	// and rewriting the config outside the structure is refused with the object
	// unchanged (C-32, C-34).
	valid := harness.ApplicationManifest("wiki", harness.Namespace, offerEntry, harness.Namespace,
		map[string]any{"mode": "dev", "replicas": 3}, nil)
	if status, response := shell.Call(t, http.MethodPost, installPath, valid, token); status != http.StatusAccepted {
		t.Fatalf("install a valid config = %d %v", status, response)
	}
	stored := cluster.Get(t, "v1beta1", "Application", "wiki")
	upgrade := harness.ApplicationManifest("wiki", harness.Namespace, offerEntry, harness.Namespace,
		map[string]any{"mode": "staging", "replicas": 9}, nil)
	status, response = shell.Accounts.HTTP.Call(t, http.MethodPut, "/api/v1/resources/Application/wiki"+namespaceParam, upgrade,
		map[string]string{"Authorization": "Bearer " + token, "If-Match": stored.GetResourceVersion()})
	if status != http.StatusBadRequest {
		t.Fatalf("upgrade with an out-of-range config = %d %v, want 400", status, response)
	}
	message = harness.String(response, "error", "message")
	for _, expected := range []string{"mode", "dev", "prod", "replicas", "maximum 4"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("message = %q, want %q named", message, expected)
		}
	}
	after := cluster.Get(t, "v1beta1", "Application", "wiki")
	if got, _ := harness.Nested(after.Object, "spec", "config", "mode").(string); got != "dev" {
		t.Fatalf("stored mode = %q, want the refused upgrade to leave it at dev", got)
	}

	// Withdrawing the capability makes the entry uninstallable while the
	// application stays (C-33). An update is not an install: it is accepted,
	// while a new install from the same entry is still refused.
	withdrawn := cluster.UpgradePackage(t, "acceptance-source", declaration(
		extension.CatalogueEntry{ID: absentEntry, Display: extension.CatalogueDisplay{Name: "Needs Absent"}, Definition: missing},
		extension.CatalogueEntry{ID: offerEntry, Display: extension.CatalogueDisplay{Name: "Wiki"},
			Definition: definition(t, installable.Config, []v1beta1.CapabilityRequirement{{Name: absentCapability, Version: "v1"}})},
		extension.CatalogueEntry{ID: tamperedEntry, Display: extension.CatalogueDisplay{Name: "Tampered"}, Definition: tampered},
	), artifacts)
	shell.Use(withdrawn)

	stored = cluster.Get(t, "v1beta1", "Application", "wiki")
	reconfigured := harness.ApplicationManifest("wiki", harness.Namespace, offerEntry, harness.Namespace,
		map[string]any{"mode": "prod", "replicas": 2}, nil)
	status, response = shell.Accounts.HTTP.Call(t, http.MethodPut, "/api/v1/resources/Application/wiki"+namespaceParam, reconfigured,
		map[string]string{"Authorization": "Bearer " + token, "If-Match": stored.GetResourceVersion()})
	if status != http.StatusAccepted {
		t.Fatalf("update while the entry is uninstallable = %d %v, want 202: an existing application is not an install", status, response)
	}
	updated := cluster.Get(t, "v1beta1", "Application", "wiki")
	if got, _ := harness.Nested(updated.Object, "spec", "config", "mode").(string); got != "prod" {
		t.Fatalf("stored mode = %q, want the accepted update to apply", got)
	}

	another := harness.ApplicationManifest("wiki-two", harness.Namespace, offerEntry, harness.Namespace,
		map[string]any{"mode": "dev", "replicas": 2}, nil)
	status, response = shell.Call(t, http.MethodPost, installPath, another, token)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("install after the capability was withdrawn = %d %v, want 422", status, response)
	}
	if code := harness.String(response, "error", "code"); code != "capability_missing" {
		t.Fatalf("code = %q, want capability_missing on the install path", code)
	}
	status, body = shell.Call(t, http.MethodGet, "/api/v1/resources/Application?namespace="+harness.Namespace, nil, token)
	if status != http.StatusOK {
		t.Fatalf("list applications = %d %v", status, body)
	}
	if items := harness.Items(t, body); len(items) != 1 {
		t.Fatalf("applications = %v, want only the installed application", items)
	}
}

// assertNoApplications checks that no application object exists: the refused
// installs never produced one (C-31).
func assertNoApplications(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	status, body := shell.Call(t, http.MethodGet, "/api/v1/resources/Application?namespace="+harness.Namespace, nil, token)
	if status != http.StatusOK {
		t.Fatalf("list applications = %d %v", status, body)
	}
	if items := harness.Items(t, body); len(items) != 0 {
		t.Fatalf("applications = %v, want none: a refused install writes no object", items)
	}
}
