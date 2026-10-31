package kosstorage

import (
	"testing"

	"github.com/tsic404/kos/pkg/extension"
)

// TestDeclarationLoads checks that the storage page is accepted as declared
// (C-22, C-53).
func TestDeclarationLoads(t *testing.T) {
	loaded, err := Declaration().Load()
	if err != nil {
		t.Fatalf("load declaration: %v", err)
	}
	if len(loaded.Problems) != 0 {
		t.Fatalf("problems = %+v, want none", loaded.Problems)
	}
	if len(loaded.Contributions.Pages) != 1 || loaded.Contributions.Pages[0].ID != PageStorage {
		t.Fatalf("pages = %+v, want the storage page", loaded.Contributions.Pages)
	}
}

// TestStoragePageCoversTheContractSources checks the storage items and source
// types against C-49, and the migration entry against C-40.
func TestStoragePageCoversTheContractSources(t *testing.T) {
	want := map[string]string{
		ItemDevicesAndPolicies: extension.PageSourceObject,
		ItemVolumes:            extension.PageSourceObject,
		ItemUsage:              extension.PageSourceMetric,
		ItemBackups:            extension.PageSourceObject,
		ItemMigration:          extension.PageSourceObject,
	}
	page := storagePage()
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
	if !actions[ActionMigration] {
		t.Fatalf("the migration entry is not declared")
	}
}
