// Package appstore is the kos-appstore first-party function package: its
// declaration contributes the distribution's catalogue source and the
// app-center page with the install and upgrade entries (C-24, C-31). The
// package ships with the distribution and hosts no repository of its own
// (NG3); the artifact reference below only locates the bytes the installer
// resolves through its ArtifactSource (C-17).
package appstore

import (
	"fmt"
	"net/http"

	"github.com/tsic404/kos/pkg/app/builtin"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/write"
)

const (
	// Identifier is the package identity across versions (C-17).
	Identifier = "kos-appstore"
	// Version is the package version.
	Version = "1.0.0"
	// FrameworkVersion is the framework range the package targets (C-16).
	FrameworkVersion = ">=1.1 <2.0"
	// Reference locates the shipped artifact for the artifact source serving
	// it; the kernel never interprets it (C-17).
	Reference = "embedded://kos-appstore"
	// SourceID names the catalogue source the package contributes (C-24).
	SourceID = "kos-appstore"
	// SyncPull tells how the source delivers its entries (C-24).
	SyncPull = "pull"
	// PageID names the app-center page (C-31).
	PageID = "app-center"
	// InstallAction and UpgradeAction are the page's write entries (C-22).
	InstallAction = "install"
	UpgradeAction = "upgrade"
	// EntriesItem and UpgradesItem are the two catalogue reads the page shows.
	EntriesItem     = "entries"
	UpgradesItem    = "upgrades"
	EntriesRef      = "catalogue/entries"
	UpgradesRef     = "catalogue/upgrades"
	ApplicationPath = "/api/v1/resources/Application"
)

// display is what the app center shows for one shipped definition (C-24).
type display struct {
	// name is the display name; a shipped definition without one shows its own
	// name instead of disappearing.
	name string
	// description explains what the application does.
	description string
	// icon is the display icon key.
	icon string
}

// displays names the applications the distribution ships. It is product data,
// not a kernel table: an application the table does not name is still offered
// under its definition name.
var displays = map[string]display{
	"memcached":     {name: "Memcached", description: "分布式内存缓存", icon: "memcached"},
	"nginx":         {name: "Nginx", description: "静态站点与反向代理", icon: "nginx"},
	"node-exporter": {name: "Node Exporter", description: "节点指标采集", icon: "node-exporter"},
	"redis":         {name: "Redis", description: "内存键值存储", icon: "redis"},
	"whoami":        {name: "Whoami", description: "请求回显服务", icon: "whoami"},
}

// Declaration returns the package declaration: one catalogue source offering
// the shipped application definitions and the app-center page (C-24, C-31).
// Each entry carries the definition spec as the package pins it, so the
// catalogue sync writes exactly the content the digest covers.
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
	}, nil
}

// Package returns the installable artifact of the appstore and the digest that
// names it (C-17).
func Package() (extension.Package, error) {
	declaration, err := Declaration()
	if err != nil {
		return extension.Package{}, err
	}
	content, err := extension.Marshal(declaration)
	if err != nil {
		return extension.Package{}, fmt.Errorf("marshal the appstore declaration: %w", err)
	}
	return extension.Package{Reference: Reference, Digest: extension.DigestOf(content), Content: content}, nil
}

// catalogueSource offers every definition the distribution ships. The entry
// identity is the definition name, so a sync writes one definition per shipped
// application.
func catalogueSource() (extension.CatalogueSource, error) {
	definitions, err := builtin.Definitions()
	if err != nil {
		return extension.CatalogueSource{}, fmt.Errorf("load the shipped definitions: %w", err)
	}
	entries := make([]extension.CatalogueEntry, 0, len(definitions))
	for _, definition := range definitions {
		entries = append(entries, extension.CatalogueEntry{
			ID:         definition.Name,
			Display:    displayOf(definition.Name),
			Definition: definition.Spec,
		})
	}
	return extension.CatalogueSource{ID: SourceID, Sync: SyncPull, Entries: entries}, nil
}

// Page returns the app-center page (C-31): the catalogue entries with the
// install entry and the upgrade paths with the upgrade entry. The install
// parameters are not a fixed item: the config structure travels inside each
// entry, and the page read lays the form fields out from it (C-32).
func Page() extension.Page {
	return extension.Page{
		ID:           PageID,
		Title:        "应用中心",
		Location:     "panel",
		RequiredRole: string(write.RoleMember),
		Items: []extension.PageItem{
			{ID: EntriesItem, Label: "可安装应用", Source: extension.PageSourceComputed, Reference: EntriesRef, Action: InstallAction},
			{ID: UpgradesItem, Label: "可升级应用", Source: extension.PageSourceComputed, Reference: UpgradesRef, Action: UpgradeAction},
		},
		Actions: []extension.Action{
			{ID: InstallAction, Method: http.MethodPost, Path: ApplicationPath},
			{ID: UpgradeAction, Method: http.MethodPut, Path: ApplicationPath},
		},
	}
}

// displayOf renders one shipped definition's display information. A definition
// the table does not name is offered under its own name, so shipping a
// definition without display data cannot drop it from the app center.
func displayOf(name string) extension.CatalogueDisplay {
	shown, found := displays[name]
	if !found {
		return extension.CatalogueDisplay{Name: name}
	}
	return extension.CatalogueDisplay{Name: shown.name, Description: shown.description, Icon: shown.icon}
}
