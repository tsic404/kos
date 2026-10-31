package appstore

import (
	"net/http"
	"testing"

	"github.com/tsic404/kos/pkg/app/builtin"
	"github.com/tsic404/kos/pkg/extension"
)

// TestDeclarationLoads checks that the package the distribution ships declares
// only contributions the framework accepts (C-18): a declaration the loader
// rejects would install as a degraded component with the app center missing.
func TestDeclarationLoads(t *testing.T) {
	declaration, err := Declaration()
	if err != nil {
		t.Fatalf("build the appstore declaration: %v", err)
	}
	if declaration.Identifier != Identifier || declaration.Version != Version {
		t.Fatalf("declaration = %s@%s, want %s@%s", declaration.Identifier, declaration.Version, Identifier, Version)
	}
	loaded, err := declaration.Load()
	if err != nil {
		t.Fatalf("load the appstore declaration: %v", err)
	}
	if len(loaded.Problems) > 0 {
		t.Fatalf("the appstore declaration has contribution problems: %+v", loaded.Problems)
	}
	if len(loaded.Contributions.CatalogueSources) != 1 || len(loaded.Contributions.Pages) != 1 {
		t.Fatalf("contributions = %+v, want one catalogue source and one page", loaded.Contributions)
	}
}

// TestCatalogueSourceOffersTheShippedApplications checks that every application
// the distribution ships appears as an entry carrying the definition the package
// pins, so the catalogue sync writes exactly the content the digest covers
// (C-24, C-32).
func TestCatalogueSourceOffersTheShippedApplications(t *testing.T) {
	declaration, err := Declaration()
	if err != nil {
		t.Fatalf("build the appstore declaration: %v", err)
	}
	definitions, err := builtin.Definitions()
	if err != nil {
		t.Fatalf("load the shipped definitions: %v", err)
	}
	source := declaration.Contributions.CatalogueSources[0]
	if source.ID != SourceID || source.Sync != SyncPull {
		t.Fatalf("source = %s/%s, want %s/%s", source.ID, source.Sync, SourceID, SyncPull)
	}
	if len(source.Entries) != len(definitions) {
		t.Fatalf("entries = %d, want one per shipped definition (%d)", len(source.Entries), len(definitions))
	}
	offered := map[string]extension.CatalogueEntry{}
	for _, entry := range source.Entries {
		if entry.Display.Name == "" {
			t.Errorf("entry %s carries no display name", entry.ID)
		}
		offered[entry.ID] = entry
	}
	for _, definition := range definitions {
		entry, found := offered[definition.Name]
		if !found {
			t.Errorf("the source does not offer the shipped definition %s", definition.Name)
			continue
		}
		if entry.Definition.Package.Digest != definition.Spec.Package.Digest {
			t.Errorf("entry %s pins %s, want the shipped digest %s",
				entry.ID, entry.Definition.Package.Digest, definition.Spec.Package.Digest)
		}
	}
}

// TestPageCarriesTheInstallAndUpgradeEntries checks the app-center page: both
// catalogue reads are shown, and each names the control-plane write that carries
// it out (C-22, C-31).
func TestPageCarriesTheInstallAndUpgradeEntries(t *testing.T) {
	page := Page()
	if page.ID != PageID || page.Title == "" || page.Location == "" {
		t.Fatalf("page = %+v, want an identified, titled page in a location", page)
	}
	if page.ReadOnly {
		t.Fatalf("page = %+v, want a page with modification entries", page)
	}
	actions := map[string]extension.Action{}
	for _, action := range page.Actions {
		actions[action.ID] = action
	}
	for id, method := range map[string]string{
		InstallAction: http.MethodPost,
		UpgradeAction: http.MethodPut,
	} {
		action, found := actions[id]
		if !found {
			t.Fatalf("page declares no %s action: %+v", id, page.Actions)
		}
		if action.Method != method || action.Path != ApplicationPath {
			t.Errorf("%s action = %s %s, want %s %s", id, action.Method, action.Path, method, ApplicationPath)
		}
	}
	items := map[string]extension.PageItem{}
	for _, item := range page.Items {
		items[item.ID] = item
	}
	for itemID, actionID := range map[string]string{
		EntriesItem:  InstallAction,
		UpgradesItem: UpgradeAction,
	} {
		item, found := items[itemID]
		if !found {
			t.Fatalf("page declares no %s item: %+v", itemID, page.Items)
		}
		if item.Source != extension.PageSourceComputed || item.Reference == "" {
			t.Errorf("item %s = %s/%s, want a computed read with a reference", itemID, item.Source, item.Reference)
		}
		if item.Action != actionID {
			t.Errorf("item %s action = %q, want %q", itemID, item.Action, actionID)
		}
	}
}

// TestPackageVerifies checks the artifact the installer resolves: its content
// parses back into the same contributions and matches the digest the package
// names (C-17).
func TestPackageVerifies(t *testing.T) {
	pkg, err := Package()
	if err != nil {
		t.Fatalf("build the appstore package: %v", err)
	}
	if pkg.Reference != Reference {
		t.Fatalf("reference = %q, want %q", pkg.Reference, Reference)
	}
	if err := pkg.Verify(); err != nil {
		t.Fatalf("verify the appstore package: %v", err)
	}
	parsed, err := extension.Parse(pkg.Content)
	if err != nil {
		t.Fatalf("parse the appstore package: %v", err)
	}
	if parsed.Identifier != Identifier || len(parsed.Contributions.Pages) != 1 {
		t.Fatalf("parsed declaration = %+v, want the appstore contributions", parsed)
	}
}
