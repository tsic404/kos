// Package tc3 verifies that a first-party and a third-party package with the
// same declaration load, judge permissions and withdraw identically (AC-1,
// TC-3).
package tc3

import (
	"reflect"
	"testing"

	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// contributions is the declaration both packages carry.
func contributions() extension.Contributions {
	return extension.Contributions{
		CatalogueSources: []extension.CatalogueSource{{ID: "catalogue", Sync: "pull"}},
		Settings: []extension.Setting{{
			ID: "catalogue-endpoint", Scope: extension.SettingScopeCluster,
			Writer: extension.SettingWriterController, Editable: true,
		}},
		Pages: []extension.Page{{
			ID: "catalogue", Title: "Catalogue", Location: "panel", RequiredRole: string(write.RoleMember),
			Items: []extension.PageItem{{
				ID: "entries", Label: "Entries", Source: extension.PageSourceLocal, Store: "catalogue", Action: "install",
			}},
			Actions: []extension.Action{{ID: "install", Method: "POST", Path: "/api/v1/action-requests"}},
		}},
	}
}

// TestTC3SameDeclarationLoadsTheSame installs both packages and compares their
// loaded contributions, then removes them.
func TestTC3SameDeclarationLoadsTheSame(t *testing.T) {
	cluster := harness.Start(t)
	artifacts := extension.MemoryArtifacts{}

	firstParty := extension.Declaration{Identifier: "kos.catalogue", Version: "1.0.0", Contributions: contributions()}
	thirdParty := extension.Declaration{Identifier: "thirdparty.catalogue", Version: "1.0.0", Contributions: contributions()}
	cluster.InstallPackage(t, "first-party", firstParty, artifacts)
	installation := cluster.InstallPackage(t, "third-party", thirdParty, artifacts)

	first, found := installation.Component("first-party")
	if !found || first.Phase != extension.PhaseReady {
		t.Fatalf("first-party = %+v (found %v)", first, found)
	}
	third, found := installation.Component("third-party")
	if !found || third.Phase != extension.PhaseReady {
		t.Fatalf("third-party = %+v (found %v)", third, found)
	}
	if first.Reason != third.Reason || len(first.Problems) != len(third.Problems) {
		t.Fatalf("load results differ: first=%+v third=%+v", first, third)
	}
	if !reflect.DeepEqual(first.Contributions, third.Contributions) {
		t.Fatalf("contributions differ:\nfirst=%+v\nthird=%+v", first.Contributions, third.Contributions)
	}

	// The page judgement is the same for every session role.
	for _, role := range []write.Role{write.RoleOwner, write.RoleMember, write.RoleGuest} {
		firstPage := first.Contributions.Pages[0].Allows(role)
		thirdPage := third.Contributions.Pages[0].Allows(role)
		if firstPage != thirdPage {
			t.Fatalf("role %q sees the page as first=%v third=%v", role, firstPage, thirdPage)
		}
	}
	registry := installation.Registry()
	if len(registry.CatalogueSources()) != 2 || len(registry.Settings()) != 2 || len(registry.Pages()) != 2 {
		t.Fatalf("registry = sources %d, settings %d, pages %d; want two of each",
			len(registry.CatalogueSources()), len(registry.Settings()), len(registry.Pages()))
	}
	if _, found := registry.PageFor("catalogue", write.RoleGuest); found {
		t.Fatal("a guest sees a page that requires a member")
	}
	if _, found := registry.PageFor("catalogue", write.RoleMember); !found {
		t.Fatal("a member does not see the page")
	}

	// Uninstall both: the same removal semantics for either origin.
	cluster.DeleteComponent(t, "first-party")
	cluster.DeleteComponent(t, "third-party")
	removed := cluster.ExtensionState(t, artifacts)
	if len(removed.Registry().CatalogueSources()) != 0 || len(removed.Registry().Settings()) != 0 ||
		len(removed.Registry().Pages()) != 0 {
		t.Fatal("a contribution survived the uninstall")
	}
}
