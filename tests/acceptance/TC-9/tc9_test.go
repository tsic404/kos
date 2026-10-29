// Package tc9 verifies that a catalogue source offers entries with display
// information, version, dependencies, install parameters and a digest, that
// installing one produces an application and its definition, and that a package
// whose content no longer matches the digest cannot be installed (AC-4, TC-9).
package tc9

import (
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	extensionID = "demo.appstore"
	sourceID    = "demo-catalogue"
	entryID     = "demo-blog"
	ownerName   = "owner"
	ownerPass   = "owner-password"
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

// definition builds the definition one entry installs, with its digest computed
// from the content as C-32 requires.
func definition(t *testing.T, body string) v1beta1.ApplicationDefinitionSpec {
	t.Helper()
	spec := v1beta1.ApplicationDefinitionSpec{
		Package:  v1beta1.PackageIdentity{Version: "1.0.0"},
		Config:   []v1beta1.ConfigField{{Name: "replicas", Type: "integer"}},
		Template: body,
		Requires: []v1beta1.CapabilityRequirement{
			{Name: "storage.volume", Version: "v1"},
			{Name: "demo.gpu/accel", Optional: true},
		},
	}
	digest, err := app.DefinitionDigest(spec)
	if err != nil {
		t.Fatalf("digest definition: %v", err)
	}
	spec.Package.Digest = digest
	return spec
}

// declaration declares one catalogue source with one entry carrying spec.
func declaration(spec v1beta1.ApplicationDefinitionSpec) extension.Declaration {
	return extension.Declaration{
		Identifier: extensionID,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			CatalogueSources: []extension.CatalogueSource{{
				ID:   sourceID,
				Sync: "pull",
				Entries: []extension.CatalogueEntry{{
					ID: entryID,
					Display: extension.CatalogueDisplay{
						Name: "Demo Blog", Description: "A demo application", Icon: "blog",
					},
					Definition: spec,
				}},
			}},
		},
	}
}

// TestTC9CatalogueEntrySyncAndDigest reads an entry, installs it, then changes
// the package content so the digest no longer matches.
func TestTC9CatalogueEntrySyncAndDigest(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	token := shell.SignInOwner(t, ownerName, ownerPass)
	artifacts := extension.MemoryArtifacts{}
	installation := cluster.InstallPackageWith(t, "appstore", declaration(definition(t, template)), artifacts, shell.Retention())
	shell.Use(installation)
	shell.SyncCatalogue(t)

	status, body := shell.Call(t, http.MethodGet, "/api/v1/catalogue/entries", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list entries = %d %v", status, body)
	}
	entries := harness.Items(t, body)
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want the one declared entry", entries)
	}
	entry := entries[0]
	if name := harness.String(entry, "name"); name != "Demo Blog" {
		t.Fatalf("display name = %q", name)
	}
	if version := harness.String(entry, "version"); version != "1.0.0" {
		t.Fatalf("version = %q", version)
	}
	if digest := harness.String(entry, "digest"); digest == "" {
		t.Fatalf("entry = %v, want a package digest", entry)
	}
	if dependencies, _ := entry["dependencies"].([]any); len(dependencies) != 2 {
		t.Fatalf("dependencies = %v, want the two declared requirements", entry["dependencies"])
	}
	if config, _ := entry["config"].([]any); len(config) != 1 || harness.String(config[0].(map[string]any), "name") != "replicas" {
		t.Fatalf("config = %v, want the install parameter structure", entry["config"])
	}
	if installable, _ := entry["installable"].(bool); !installable {
		t.Fatalf("entry = %v, want installable", entry)
	}

	// The sync wrote the definition, so installing the entry creates an
	// application that the definition renders.
	definitionRef := harness.String(entry, "definition")
	cluster.Get(t, "v1beta1", "ApplicationDefinition", definitionRef)
	manifest := harness.ApplicationManifest("blog", harness.Namespace, definitionRef, harness.Namespace, nil, nil)
	status, body = shell.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, token)
	if status != http.StatusAccepted {
		t.Fatalf("install the entry = %d %v", status, body)
	}
	cluster.Get(t, "v1beta1", "Application", "blog")

	// A package whose content changed without its digest cannot be installed,
	// and the definition keeps the content it was installed from.
	changedSpec := definition(t, template)
	changedSpec.Template = template + "data:\n  extra: \"changed\"\n"
	changed := declaration(changedSpec)
	content, err := extension.Marshal(changed)
	if err != nil {
		t.Fatalf("marshal changed declaration: %v", err)
	}
	artifacts["test://appstore-changed"] = content
	cluster.RepointComponent(t, "appstore", "test://appstore-changed", extension.DigestOf(content))
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))
	shell.SyncCatalogue(t)

	status, body = shell.Call(t, http.MethodGet, "/api/v1/catalogue/entries", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list entries after the change = %d %v", status, body)
	}
	entries = harness.Items(t, body)
	if len(entries) != 1 {
		t.Fatalf("entries after the change = %v, want the entry still visible", entries)
	}
	if installable, _ := entries[0]["installable"].(bool); installable {
		t.Fatalf("entry = %v, want not installable when the content does not match the digest", entries[0])
	}
	if reason := harness.String(entries[0], "reason"); reason == "" {
		t.Fatalf("entry = %v, want the reason it cannot be installed", entries[0])
	}
	stored := cluster.Get(t, "v1beta1", "ApplicationDefinition", definitionRef)
	storedSpec, _, err := unstructured.NestedMap(stored.Object, "spec")
	if err != nil {
		t.Fatalf("read stored definition spec: %v", err)
	}
	if body := harness.String(storedSpec, "template"); body != template {
		t.Fatalf("stored definition template = %q, want the installed content", body)
	}
}

// otherTemplate renders the same definition shape with different content, so a
// definition written by the second source is distinguishable from the first's.
const otherTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  mode: "second"
`

// sourceDeclaration declares one catalogue source offering the shared entry
// identity with body as its definition template.
func sourceDeclaration(t *testing.T, identifier, source, body string) extension.Declaration {
	t.Helper()
	spec := v1beta1.ApplicationDefinitionSpec{
		Package:  v1beta1.PackageIdentity{Version: "1.0.0"},
		Template: body,
	}
	digest, err := app.DefinitionDigest(spec)
	if err != nil {
		t.Fatalf("digest definition: %v", err)
	}
	spec.Package.Digest = digest
	return extension.Declaration{
		Identifier: identifier,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			CatalogueSources: []extension.CatalogueSource{{
				ID:   source,
				Sync: "pull",
				Entries: []extension.CatalogueEntry{{
					ID:         entryID,
					Display:    extension.CatalogueDisplay{Name: source},
					Definition: spec,
				}},
			}},
		},
	}
}

// hasProblemClass reports whether one component failed the named contribution
// class.
func hasProblemClass(problems []extension.ClassProblem, class string) bool {
	for _, problem := range problems {
		if problem.Class == class {
			return true
		}
	}
	return false
}

// TestTC9CrossSourceEntryConflictKeepsOneDefinition installs two sources that
// offer the same entry identity: the first declaration keeps the entry and the
// definition it names, the later component reports the conflict, and repeated
// syncs never hand the cluster-scoped definition back and forth (C-24).
func TestTC9CrossSourceEntryConflictKeepsOneDefinition(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	token := shell.SignInOwner(t, ownerName, ownerPass)
	artifacts := extension.MemoryArtifacts{}

	installation := cluster.InstallPackageWith(t, "first-store",
		sourceDeclaration(t, "demo.store.first", "first-catalogue", template), artifacts, shell.Retention())
	shell.Use(installation)
	installation = cluster.InstallPackageWith(t, "second-store",
		sourceDeclaration(t, "demo.store.second", "second-catalogue", otherTemplate), artifacts, shell.Retention())
	shell.Use(installation)

	// Two syncs: the round after a write is where a flip between two writers
	// shows up.
	shell.SyncCatalogue(t)
	shell.SyncCatalogue(t)

	status, body := shell.Call(t, http.MethodGet, "/api/v1/catalogue/entries", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list entries = %d %v", status, body)
	}
	entries := harness.Items(t, body)
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want the one identity both sources claim", entries)
	}
	if source := harness.String(entries[0], "source"); source != "first-catalogue" {
		t.Fatalf("entry source = %q, want the first declaration", source)
	}

	stored := cluster.Get(t, "v1beta1", "ApplicationDefinition", entryID)
	storedSpec, _, err := unstructured.NestedMap(stored.Object, "spec")
	if err != nil {
		t.Fatalf("read stored definition spec: %v", err)
	}
	if got := harness.String(storedSpec, "template"); got != template {
		t.Fatalf("stored definition template = %q, want the first source's content", got)
	}

	result, found := installation.Component("second-store")
	if !found || result.Phase != extension.PhaseDegraded {
		t.Fatalf("second component = %+v, want degraded on the entry conflict", result)
	}
	if !hasProblemClass(result.Problems, extension.ClassCatalogueSources) {
		t.Fatalf("problems = %v, want the catalogue conflict named", result.Problems)
	}
}
