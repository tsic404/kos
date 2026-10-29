// Package tc10 verifies that removing a catalogue source removes its entries
// while the application installed from one of them stays, and that the
// application's upgrade path is marked as missing its source (AC-4, TC-10).
package tc10

import (
	"net/http"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/catalogue"
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

// declaration declares one catalogue source with one entry.
func declaration(t *testing.T) extension.Declaration {
	t.Helper()
	spec := v1beta1.ApplicationDefinitionSpec{
		Package:  v1beta1.PackageIdentity{Version: "1.0.0"},
		Template: template,
	}
	digest, err := app.DefinitionDigest(spec)
	if err != nil {
		t.Fatalf("digest definition: %v", err)
	}
	spec.Package.Digest = digest
	return extension.Declaration{
		Identifier: extensionID,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			CatalogueSources: []extension.CatalogueSource{{
				ID:   sourceID,
				Sync: "pull",
				Entries: []extension.CatalogueEntry{{
					ID:         entryID,
					Display:    extension.CatalogueDisplay{Name: "Demo Blog"},
					Definition: spec,
				}},
			}},
		},
	}
}

// TestTC10SourceRemovalKeepsInstalledApplication removes the source and checks
// the entry is gone, the application is untouched and its upgrade path names the
// missing source.
func TestTC10SourceRemovalKeepsInstalledApplication(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	token := shell.SignInOwner(t, ownerName, ownerPass)
	artifacts := extension.MemoryArtifacts{}
	installation := cluster.InstallPackageWith(t, "appstore", declaration(t), artifacts, shell.Retention())
	shell.Use(installation)
	shell.SyncCatalogue(t)

	status, body := shell.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.ApplicationManifest("blog", harness.Namespace, entryID, harness.Namespace, nil, nil), token)
	if status != http.StatusAccepted {
		t.Fatalf("install the entry = %d %v", status, body)
	}
	before := cluster.Get(t, "v1beta1", "Application", "blog")

	// Removing the component withdraws the source: the entry disappears and the
	// definition is no longer offered.
	cluster.DeleteComponent(t, "appstore")
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))
	shell.SyncCatalogue(t)

	status, body = shell.Call(t, http.MethodGet, "/api/v1/catalogue/entries", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list entries after the removal = %d %v", status, body)
	}
	if entries := harness.Items(t, body); len(entries) != 0 {
		t.Fatalf("entries after the removal = %v, want none", entries)
	}

	// The application is unchanged: same spec and same stored object.
	after := cluster.Get(t, "v1beta1", "Application", "blog")
	if after.GetResourceVersion() != before.GetResourceVersion() {
		t.Fatalf("application resourceVersion = %q, want %q: the removal must not touch it",
			after.GetResourceVersion(), before.GetResourceVersion())
	}

	status, body = shell.Call(t, http.MethodGet, "/api/v1/catalogue/upgrades", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list upgrade paths = %d %v", status, body)
	}
	upgrades := harness.Items(t, body)
	if len(upgrades) != 1 {
		t.Fatalf("upgrade paths = %v, want the one installed application", upgrades)
	}
	if state := harness.String(upgrades[0], "state"); state != catalogue.UpgradeSourceMissing {
		t.Fatalf("upgrade state = %q, want %q", state, catalogue.UpgradeSourceMissing)
	}
	if definition := harness.String(upgrades[0], "definition"); definition != entryID {
		t.Fatalf("upgrade definition = %q, want %q", definition, entryID)
	}
	if name := harness.String(upgrades[0], "name"); name != "blog" {
		t.Fatalf("upgrade application = %q, want blog", name)
	}
}
