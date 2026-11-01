package kostaskmanager

import (
	"net/http"
	"testing"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
)

// TestDeclarationLoads checks that the product's contribution is accepted as
// declared (C-22, C-53).
func TestDeclarationLoads(t *testing.T) {
	loaded, err := Declaration().Load()
	if err != nil {
		t.Fatalf("load declaration: %v", err)
	}
	if len(loaded.Problems) != 0 {
		t.Fatalf("problems = %+v, want none", loaded.Problems)
	}
	if len(loaded.Contributions.Pages) != 3 {
		t.Fatalf("pages = %d, want the overview, application and detail pages", len(loaded.Contributions.Pages))
	}
	ids := map[string]bool{}
	for _, page := range loaded.Contributions.Pages {
		ids[page.ID] = true
	}
	for _, id := range []string{PageOverview, PageApplications, PageApplicationDetail} {
		if !ids[id] {
			t.Fatalf("pages = %v, want %q", ids, id)
		}
	}
}

// TestOverviewCoversTheContractSources checks the overview items and source
// types against C-49.
func TestOverviewCoversTheContractSources(t *testing.T) {
	want := map[string]string{
		ItemNodesReady:          extension.PageSourceObject,
		ItemAbnormalObjects:     extension.PageSourceComputed,
		ItemPendingUpgrades:     extension.PageSourceComputed,
		ItemRecentEvents:        extension.PageSourceObject,
		ItemMaintenanceNodes:    extension.PageSourceObject,
		ItemMaintenanceAffected: extension.PageSourceComputed,
	}
	assertSources(t, overviewPage(), want)
}

// TestApplicationPageCoversTheContractSources checks the application items and
// their modification entries against C-49 and C-66.
func TestApplicationPageCoversTheContractSources(t *testing.T) {
	page := applicationsPage()
	want := map[string]string{
		ItemInstalled:     extension.PageSourceObject,
		ItemInstallable:   extension.PageSourceComputed,
		ItemResourceUsage: extension.PageSourceMetric,
		ItemStop:          extension.PageSourceObject,
		ItemStart:         extension.PageSourceObject,
		ItemRestart:       extension.PageSourceObject,
		ItemScale:         extension.PageSourceObject,
		ItemResourceSpec:  extension.PageSourceObject,
		ItemUpgrade:       extension.PageSourceObject,
		ItemUninstall:     extension.PageSourceObject,
		ItemReinstall:     extension.PageSourceObject,
	}
	assertSources(t, page, want)
	actions := map[string]bool{}
	for _, action := range page.Actions {
		actions[action.ID] = true
	}
	for _, id := range []string{ActionStop, ActionStart, ActionRestart, ActionScale, ActionResourceSpec, ActionUpgrade, ActionUninstall, ActionReinstall} {
		if !actions[id] {
			t.Errorf("action %q is not declared", id)
		}
	}
	for _, item := range page.Items {
		if item.Action == "" && item.Reason == "" {
			t.Errorf("item %q carries neither an entry nor a reason", item.ID)
		}
	}
}

// TestApplicationDetailCoversTheContractSources checks the detail page's items
// and source types against C-52: six object sources and the log read source.
func TestApplicationDetailCoversTheContractSources(t *testing.T) {
	page := applicationDetailPage()
	if !page.ReadOnly || page.ReadOnlyReason == "" || len(page.Actions) != 0 {
		t.Fatalf("detail page = %+v, want a stated read-only page with no action", page)
	}
	assertSources(t, page, map[string]string{
		ItemDetailHealth:    extension.PageSourceObject,
		ItemDetailVolumes:   extension.PageSourceObject,
		ItemDetailEndpoints: extension.PageSourceObject,
		ItemDetailBackup:    extension.PageSourceObject,
		ItemDetailDrift:     extension.PageSourceObject,
		ItemDetailEvents:    extension.PageSourceObject,
		ItemDetailLogs:      extension.PageSourceRead,
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

// TestControlPlaneActionsAreAccepted covers C-22 and C-56: every lifecycle entry
// that posts to the action endpoint resolves to a value the API accepts, so an
// entry pointing at an action the control plane does not know cannot ship.
func TestControlPlaneActionsAreAccepted(t *testing.T) {
	posted := map[string]bool{}
	for _, action := range applicationsPage().Actions {
		if action.Method != http.MethodPost || action.Path != pathActionRequests {
			continue
		}
		value, known := ControlPlaneAction(action.ID)
		if !known {
			t.Errorf("page action %q has no control-plane action", action.ID)
			continue
		}
		if !controlplane.AcceptsAction(value) {
			t.Errorf("page action %q submits %q, which the API rejects", action.ID, value)
		}
		posted[action.ID] = true
	}
	for _, id := range []string{ActionStop, ActionStart, ActionRestart, ActionScale, ActionReinstall} {
		if !posted[id] {
			t.Errorf("the page does not offer %q through the action endpoint", id)
		}
	}
}
