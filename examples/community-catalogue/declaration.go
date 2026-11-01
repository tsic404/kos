// Package communitycatalogue is a reference third-party extension package. It
// contributes the same kinds of entries a first-party product contributes — one
// catalogue source and one page over it — so a third party serves the same need
// as the distribution's app centre without a kernel change (C-17, C-18, C-24,
// C-31). Nothing here is installed by the distribution: the package exists to be
// installed beside the first-party products, and to keep serving its entries when
// the first-party product that serves the same need is removed.
package communitycatalogue

import (
	"net/http"

	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/write"
)

// The package identity and the contributions it declares.
const (
	// Identifier is the package identity across versions (C-17).
	Identifier = "community.catalogue"
	// Version is the package version.
	Version = "1.0.0"
	// FrameworkVersion is the framework range the package targets (C-16).
	FrameworkVersion = ">=1.1 <2.0"
	// SourceID names the catalogue source the package contributes (C-24).
	SourceID = "community"
	// SyncPull tells how the source delivers its entries (C-24).
	SyncPull = "pull"
	// PageID names the page the package declares (C-22).
	PageID = "community-app-center"
	// InstallAction and UpgradeAction are the page's write entries (C-22).
	InstallAction = "install"
	UpgradeAction = "upgrade"
	// ItemEntries and ItemUpgrades are the two items the page shows.
	ItemEntries  = "entries"
	ItemUpgrades = "upgrades"
	// EntriesRef and UpgradesRef are the two catalogue reads the page shows.
	EntriesRef  = Identifier + "/entries"
	UpgradesRef = Identifier + "/upgrades"
	// ApplicationPath is the control-plane write both actions go through.
	ApplicationPath = "/api/v1/resources/Application"
)

// Declaration returns the package declaration: one catalogue source offering the
// community definitions and one page over it (C-18, C-24). The page declares the
// same contribution shape the first-party app centre declares, because the kernel
// loads declarations and not names (C-17).
func Declaration() (extension.Declaration, error) {
	source, err := catalogueSource()
	if err != nil {
		return extension.Declaration{}, err
	}
	return extension.Declaration{
		Identifier:       Identifier,
		Version:          Version,
		FrameworkVersion: FrameworkVersion,
		Contributions: extension.Contributions{
			CatalogueSources: []extension.CatalogueSource{source},
			Pages:            []extension.Page{Page()},
		},
		Permissions: []extension.Permission{{
			// C-59's default for a page: a member reads it; a guest does not
			// install applications.
			Contribution: PageID,
			RequiredRole: string(write.RoleMember),
		}},
	}, nil
}

// Page returns the community app centre (C-22, C-31): the entries this source
// offers with the install entry, and the upgrade paths of the applications
// installed from it with the upgrade entry. The install parameters travel inside
// each entry's config structure and are laid out as form fields at read time
// (C-32), exactly as the first-party app centre lays them out.
func Page() extension.Page {
	return extension.Page{
		ID:           PageID,
		Title:        "社区应用中心",
		Location:     "panel",
		RequiredRole: string(write.RoleMember),
		Items: []extension.PageItem{
			{ID: ItemEntries, Label: "社区可安装应用", Source: extension.PageSourceComputed, Reference: EntriesRef, Action: InstallAction},
			{ID: ItemUpgrades, Label: "社区可升级应用", Source: extension.PageSourceComputed, Reference: UpgradesRef, Action: UpgradeAction},
		},
		Actions: []extension.Action{
			{ID: InstallAction, Method: http.MethodPost, Path: ApplicationPath},
			{ID: UpgradeAction, Method: http.MethodPut, Path: ApplicationPath},
		},
	}
}
