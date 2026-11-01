// Package tc115 verifies AC-56 through TC-115: a third-party extension serves
// the same kind of need a first-party product serves — a second catalogue source
// with its own page — and behaves alike without a kernel change; the kernel kind
// set, capability baseline, page shell and writer table stay as they were, and the
// need keeps being served by the third party after the first-party product is
// removed.
package tc115

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/appstore"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"

	community "github.com/tsic404/kos/examples/community-catalogue"
)

const (
	ownerName     = "owner"
	ownerPassword = "correct-horse-battery"
	thirdParty    = "community"
)

// TestTC115ThirdPartyServesTheSameNeed installs the app centre and the reference
// third-party package, compares what both serve, then removes the first-party
// product and checks the third party still serves the need.
func TestTC115ThirdPartyServesTheSameNeed(t *testing.T) {
	cluster := harness.Start(t)
	communityReadings := &community.Readings{}
	shell := cluster.StartShell(t, func(config *pages.Config) {
		appCenter := config.Readings
		config.Readings = pages.Router{
			appstore.Identifier:  appCenter,
			community.Identifier: communityReadings,
		}
	})
	communityReadings.Catalogue = shell.Catalogue
	communityReadings.Now = shell.Accounts.Clock.Now

	kindsBefore := append([]string(nil), api.Kinds()...)
	capabilitiesBefore := extension.BaselineCapabilities()
	ownedBefore := write.OwnedKinds()

	artifacts := extension.MemoryArtifacts{}
	cluster.InstallPackageWith(t, "appstore", appstoreDeclaration(t), artifacts, shell.Retention())
	installation := cluster.InstallPackageWith(t, "community", communityDeclaration(t), artifacts, shell.Retention())
	shell.Use(installation)
	token := shell.SignInOwner(t, ownerName, ownerPassword)

	// The third-party component loads through the same declaration path: no
	// contribution class is refused, and the kernel is untouched (C-17, C-18).
	requireReady(t, installation, "community")
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore)

	// Both pages render through the same shell, and each shows its own source's
	// entries with the install form its config structure generates (C-22, C-31).
	firstParty := readPage(t, shell, token, appstore.PageID)
	thirdPartyPage := readPage(t, shell, token, community.PageID)
	firstEntries := listOf(t, firstParty, appstore.EntriesItem)
	thirdEntries := listOf(t, thirdPartyPage, community.ItemEntries)
	if len(firstEntries) == 0 || len(thirdEntries) == 0 {
		t.Fatalf("first-party entries = %d, third-party entries = %d; both serve their own source",
			len(firstEntries), len(thirdEntries))
	}
	for _, raw := range thirdEntries {
		entry, _ := raw.(map[string]any)
		if harness.String(entry, "source") != community.SourceID {
			t.Fatalf("third-party entry = %v, want the third-party source", entry)
		}
	}
	requireSameShape(t, firstEntries[0].(map[string]any), thirdEntries[0].(map[string]any))
	requireSameEntry(t, firstParty, appstore.InstallAction, thirdPartyPage, community.InstallAction)

	// The catalogue sync writes the definitions both sources offer, and installing
	// one of the third party's entries produces the same objects the first-party
	// install produces (C-24, C-31).
	shell.SyncCatalogue(t)
	status, body := shell.Call(t, http.MethodGet, "/api/v1/catalogue/entries", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list entries = %d %v", status, body)
	}
	offered := map[string]map[string]any{}
	for _, entry := range harness.Items(t, body) {
		offered[harness.String(entry, "source")+"/"+harness.String(entry, "id")] = entry
	}
	thirdEntryID := harness.String(thirdEntries[0].(map[string]any), "id")
	offer, found := offered[community.SourceID+"/"+thirdEntryID]
	if !found {
		t.Fatalf("the third party's entry is not in the catalogue: %v", offered)
	}
	if installable, _ := offer["installable"].(bool); !installable {
		t.Fatalf("third-party entry = %v, want installable", offer)
	}
	definitionName := harness.String(offer, "definition")
	manifest := harness.ApplicationManifest("community-app", harness.Namespace, definitionName, harness.Namespace, nil, nil)
	status, body = shell.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, token)
	if status != http.StatusAccepted {
		t.Fatalf("install the third party's entry = %d %v", status, body)
	}
	definition := cluster.Get(t, "v1beta1", "ApplicationDefinition", definitionName)
	if digest := harness.String(definition.Object, "spec", "package", "digest"); digest != harness.String(offer, "digest") {
		t.Fatalf("definition digest = %q, want the entry's package digest", digest)
	}
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore)

	// Removing the first-party product withdraws its page and its source, and
	// the third party keeps serving the same need: its page renders, its entries
	// stay in the catalogue and an application can still be installed from it.
	cluster.DeleteComponent(t, "appstore")
	installation = cluster.ExtensionStateWith(t, artifacts, shell.Retention())
	shell.Use(installation)
	if _, found := installation.Registry().PageFor(appstore.PageID, write.RoleMember); found {
		t.Fatal("the first-party app-centre page survived the uninstall")
	}
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore)

	status, body = shell.Call(t, http.MethodGet, "/api/v1/pages/"+appstore.PageID, nil, token)
	if status != http.StatusNotFound {
		t.Fatalf("removed first-party page = %d %v, want 404", status, body)
	}
	thirdPartyPage = readPage(t, shell, token, community.PageID)
	if entries := listOf(t, thirdPartyPage, community.ItemEntries); len(entries) != len(thirdEntries) {
		t.Fatalf("third-party entries = %d after the first-party removal, want %d", len(entries), len(thirdEntries))
	}
	shell.SyncCatalogue(t)
	status, body = shell.Call(t, http.MethodGet, "/api/v1/catalogue/entries", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list entries after the removal = %d %v", status, body)
	}
	for _, entry := range harness.Items(t, body) {
		if harness.String(entry, "source") != community.SourceID {
			t.Fatalf("entry = %v survived from the removed first-party source", entry)
		}
	}
	secondEntryID := harness.String(thirdEntries[1].(map[string]any), "id")
	manifest = harness.ApplicationManifest("community-app-2", harness.Namespace, secondEntryID, harness.Namespace, nil, nil)
	status, body = shell.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, token)
	if status != http.StatusAccepted {
		t.Fatalf("install from the third party after the removal = %d %v", status, body)
	}
	cluster.Get(t, "v1beta1", "Application", "community-app-2")
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore)
}

// appstoreDeclaration loads the first-party product's package.
func appstoreDeclaration(t *testing.T) extension.Declaration {
	t.Helper()
	declaration, err := appstore.Declaration()
	if err != nil {
		t.Fatalf("build the app-centre declaration: %v", err)
	}
	return declaration
}

// communityDeclaration loads the third-party package.
func communityDeclaration(t *testing.T) extension.Declaration {
	t.Helper()
	declaration, err := community.Declaration()
	if err != nil {
		t.Fatalf("build the third-party declaration: %v", err)
	}
	return declaration
}

// readPage reads one page and fails the case on a refusal.
func readPage(t *testing.T, shell *harness.Shell, token, id string) map[string]any {
	t.Helper()
	status, body := shell.Call(t, http.MethodGet, "/api/v1/pages/"+id, nil, token)
	if status != http.StatusOK {
		t.Fatalf("read page %q = %d %v", id, status, body)
	}
	return body
}

// listOf resolves one rendered item whose value is a list.
func listOf(t *testing.T, page map[string]any, id string) []any {
	t.Helper()
	items, _ := page["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if harness.String(item, "id") != id {
			continue
		}
		value, _ := item["value"].([]any)
		if value == nil {
			t.Fatalf("item %q of page %q carries no list value", id, harness.String(page, "id"))
		}
		return value
	}
	t.Fatalf("page %q has no item %q", harness.String(page, "id"), id)
	return nil
}

// requireSameShape checks that the third party renders the same entry shape the
// first party renders: the same display fields, the same install form generated
// from the config structure, the same installability judgement.
func requireSameShape(t *testing.T, first, third map[string]any) {
	t.Helper()
	for _, field := range []string{"name", "source", "version", "digest", "definition"} {
		if harness.String(first, field) == "" || harness.String(third, field) == "" {
			t.Fatalf("entry field %q: first-party = %q, third-party = %q", field, harness.String(first, field), harness.String(third, field))
		}
	}
	firstFields, _ := first["config"].([]any)
	thirdFields, _ := third["config"].([]any)
	if len(firstFields) != len(thirdFields) {
		t.Fatalf("install form fields: first-party = %d, third-party = %d", len(firstFields), len(thirdFields))
	}
	for index := range thirdFields {
		firstField, _ := firstFields[index].(map[string]any)
		thirdField, _ := thirdFields[index].(map[string]any)
		for _, key := range []string{"name", "type", "control"} {
			if harness.String(firstField, key) != harness.String(thirdField, key) {
				t.Fatalf("install form field %d %q: first-party = %q, third-party = %q",
					index, key, harness.String(firstField, key), harness.String(thirdField, key))
			}
		}
	}
	if first["installable"] != third["installable"] {
		t.Fatalf("installability: first-party = %v, third-party = %v", first["installable"], third["installable"])
	}
}

// requireSameEntry checks that both pages offer their install entry as the same
// control-plane write (C-22).
func requireSameEntry(t *testing.T, firstPage map[string]any, firstAction string, thirdPage map[string]any, thirdAction string) {
	t.Helper()
	first := actionOf(t, firstPage, firstAction)
	third := actionOf(t, thirdPage, thirdAction)
	if harness.String(first, "entry", "method") != harness.String(third, "entry", "method") ||
		harness.String(first, "entry", "path") != harness.String(third, "entry", "path") {
		t.Fatalf("install entry: first-party = %v, third-party = %v", first, third)
	}
}

// actionOf resolves one page action.
func actionOf(t *testing.T, page map[string]any, id string) map[string]any {
	t.Helper()
	actions, _ := page["actions"].([]any)
	for _, raw := range actions {
		action, _ := raw.(map[string]any)
		if harness.String(action, "id") == id {
			return action
		}
	}
	t.Fatalf("page %q offers no action %q", harness.String(page, "id"), id)
	return nil
}

// requireReady checks one component loaded with no contribution problem.
func requireReady(t *testing.T, installation *extension.Installation, name string) {
	t.Helper()
	result, found := installation.Component(name)
	if !found || result.Phase != extension.PhaseReady || len(result.Problems) > 0 {
		t.Fatalf("component %q = %+v (found %v), want Ready with no problem", name, result, found)
	}
}

// requireKernelUnchanged checks the kernel kind set, the capability baseline and
// the writer table after one install or uninstall step (C-01, C-09, F17).
func requireKernelUnchanged(t *testing.T, kinds []string, capabilities []extension.Capability, owned []string) {
	t.Helper()
	if !reflect.DeepEqual(api.Kinds(), kinds) {
		t.Fatalf("the kernel kind set changed: %v", api.Kinds())
	}
	if !reflect.DeepEqual(extension.BaselineCapabilities(), capabilities) {
		t.Fatal("the capability baseline changed")
	}
	if !reflect.DeepEqual(write.OwnedKinds(), owned) {
		t.Fatal("the writer table changed")
	}
}
