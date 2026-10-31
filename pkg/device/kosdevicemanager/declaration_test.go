package kosdevicemanager

import (
	"testing"

	"github.com/tsic404/kos/pkg/extension"
)

// TestDeclarationLoads checks that the device page is accepted as declared, is
// read-only and states why (C-22, C-50).
func TestDeclarationLoads(t *testing.T) {
	loaded, err := Declaration().Load()
	if err != nil {
		t.Fatalf("load declaration: %v", err)
	}
	if len(loaded.Problems) != 0 {
		t.Fatalf("problems = %+v, want none", loaded.Problems)
	}
	if len(loaded.Contributions.Pages) != 1 {
		t.Fatalf("pages = %d, want the device page", len(loaded.Contributions.Pages))
	}
	page := loaded.Contributions.Pages[0]
	if page.ID != PageDevices || !page.ReadOnly || page.ReadOnlyReason == "" || len(page.Actions) != 0 {
		t.Fatalf("page = %+v, want a read-only page with its reason and no action", page)
	}
	for _, item := range page.Items {
		if item.Action != "" {
			t.Errorf("read-only item %q declares a modification entry", item.ID)
		}
	}
}

// TestDevicePageCoversTheContractSources checks the device items and source
// types against C-50.
func TestDevicePageCoversTheContractSources(t *testing.T) {
	want := map[string]string{
		ItemHardware: extension.PageSourceObject,
		ItemHealth:   extension.PageSourceComputed,
		ItemMarkers:  extension.PageSourceObject,
	}
	page := devicesPage()
	if len(page.Items) != len(want) {
		t.Fatalf("%s declares %d items, want %d", page.ID, len(page.Items), len(want))
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
	}
}
