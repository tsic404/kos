package kosdiagnostics

import (
	"testing"

	"github.com/tsic404/kos/pkg/extension"
)

// TestDeclarationLoads checks that the diagnostics page is accepted as declared
// and carries its export entry (C-22, C-51).
func TestDeclarationLoads(t *testing.T) {
	loaded, err := Declaration().Load()
	if err != nil {
		t.Fatalf("load declaration: %v", err)
	}
	if len(loaded.Problems) != 0 {
		t.Fatalf("problems = %+v, want none", loaded.Problems)
	}
	if len(loaded.Contributions.Pages) != 1 {
		t.Fatalf("pages = %d, want the diagnostics page", len(loaded.Contributions.Pages))
	}
	page := loaded.Contributions.Pages[0]
	if page.ID != PageDiagnostics || page.ReadOnly {
		t.Fatalf("page = %+v, want the writable diagnostics page", page)
	}
	if len(page.Actions) != 1 || page.Actions[0].ID != ActionExport {
		t.Fatalf("actions = %+v, want the bundle export", page.Actions)
	}
}

// TestDiagnosticsPageCoversTheContractSources checks the diagnostics items and
// source types against C-51.
func TestDiagnosticsPageCoversTheContractSources(t *testing.T) {
	want := map[string]string{
		ItemDegradation: extension.PageSourceComputed,
		ItemBlocked:     extension.PageSourceObject,
		ItemEvidence:    extension.PageSourceObject,
		ItemExport:      extension.PageSourceComputed,
	}
	page := diagnosticsPage()
	if len(page.Items) != len(want) {
		t.Fatalf("%s declares %d items, want %d", page.ID, len(page.Items), len(want))
	}
	actions := map[string]bool{}
	for _, action := range page.Actions {
		actions[action.ID] = true
	}
	for _, item := range page.Items {
		source, known := want[item.ID]
		if !known {
			t.Errorf("unexpected item %q", item.ID)
			continue
		}
		if item.Source != source {
			t.Errorf("item %q source = %q, want %q", item.ID, item.Source, source)
		}
		if item.Action != "" && !actions[item.Action] {
			t.Errorf("item %q names undeclared action %q", item.ID, item.Action)
		}
		if item.Action == "" && item.Reason == "" {
			t.Errorf("item %q carries neither an entry nor a reason", item.ID)
		}
	}
}
