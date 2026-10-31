package kosupdater

import (
	"testing"

	"github.com/tsic404/kos/pkg/extension"
)

// TestDeclarationLoads checks that both pages are accepted as declared and state
// why they are read-only (C-22, C-51).
func TestDeclarationLoads(t *testing.T) {
	loaded, err := Declaration().Load()
	if err != nil {
		t.Fatalf("load declaration: %v", err)
	}
	if len(loaded.Problems) != 0 {
		t.Fatalf("problems = %+v, want none", loaded.Problems)
	}
	if len(loaded.Contributions.Pages) != 2 {
		t.Fatalf("pages = %d, want the component and update pages", len(loaded.Contributions.Pages))
	}
	ids := map[string]bool{}
	for _, page := range loaded.Contributions.Pages {
		ids[page.ID] = true
		if !page.ReadOnly || page.ReadOnlyReason == "" || len(page.Actions) != 0 {
			t.Fatalf("page %q = %+v, want a read-only page with its reason", page.ID, page)
		}
	}
	if !ids[PageComponents] || !ids[PageUpdates] {
		t.Fatalf("pages = %v, want %q and %q", ids, PageComponents, PageUpdates)
	}
}

// TestPagesCoverTheContractSources checks the component and update items and
// source types against C-51.
func TestPagesCoverTheContractSources(t *testing.T) {
	assertSources(t, componentsPage(), map[string]string{
		ItemCapabilities:     extension.PageSourceObject,
		ItemGaps:             extension.PageSourceComputed,
		ItemInstallable:      extension.PageSourceComputed,
		ItemBlockedProviders: extension.PageSourceObject,
		ItemExtensionState:   extension.PageSourceObject,
	})
	assertSources(t, updatesPage(), map[string]string{
		ItemVersions:            extension.PageSourceComputed,
		ItemApplicationUpgrades: extension.PageSourceObject,
		ItemIrreversible:        extension.PageSourceComputed,
		ItemCompatibility:       extension.PageSourceComputed,
	})
}

// assertSources checks one page's item set and source types exactly.
func assertSources(t *testing.T, page extension.Page, want map[string]string) {
	t.Helper()
	if len(page.Items) != len(want) {
		t.Fatalf("%s declares %d items, want %d", page.ID, len(page.Items), len(want))
	}
	for _, item := range page.Items {
		source, known := want[item.ID]
		if !known {
			t.Errorf("page %s declares unexpected item %q", page.ID, item.ID)
			continue
		}
		if item.Source != source {
			t.Errorf("item %q source = %q, want %q", item.ID, item.Source, source)
		}
	}
}
