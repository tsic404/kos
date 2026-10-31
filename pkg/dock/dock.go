// Package dock is the kos-dock first-party extension package: the desktop entry
// object class (C-30), the desktop page (C-52) and the dock preference domain
// (C-27). An entry is a user-written object, so this package contributes no
// controller: removing an entry reaches the entry alone and never the
// application, its data or its downstream objects.
package dock

import (
	"net/http"

	"github.com/tsic404/kos/pkg/extension"
)

// The package identity and the contribution names it declares.
const (
	// Identifier is the extension identity across versions (C-17).
	Identifier = "kos-dock"
	// Version is the package version this declaration belongs to.
	Version = "1.0.0"
	// APIGroup is the extension's own API group; it is never the kernel group
	// (C-19).
	APIGroup = "desktop.kos.tsic.top"
	// EntryKind is the desktop entry object class (C-30).
	EntryKind = "DesktopEntry"
	// EntryResource is the served resource name of the class.
	EntryResource = "desktopentries"
	// EntryVersion is the served version of the class.
	EntryVersion = "v1"
	// PageID is the desktop page identity (C-52).
	PageID = "desktop"
	// PreferenceLayout is the dock layout preference key (C-27).
	PreferenceLayout = "layout"
)

// Visibilities an entry may carry (C-30). The empty value reads as Everyone.
const (
	// VisibilityEveryone shows the entry to every signed-in account.
	VisibilityEveryone = "everyone"
	// VisibilityOwner shows the entry to the account that owns it.
	VisibilityOwner = "owner"
)

// The page actions the desktop page offers: control-plane writes on the entry
// class (C-22). Opening an entry is not among them, because a window is browser
// state and writes nothing.
const (
	// ActionAddEntry creates one desktop entry.
	ActionAddEntry = "add-entry"
	// ActionMoveEntry rewrites the visibility and order of one entry.
	ActionMoveEntry = "move-entry"
	// ActionRemoveEntry deletes one entry without touching its application.
	ActionRemoveEntry = "remove-entry"
)

// Declaration is the kos-dock package (C-18): the entry class, the desktop
// page and the dock preference domain.
func Declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: Identifier,
		Version:    Version,
		APIGroup:   APIGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: APIGroup, Kind: EntryKind, Resource: EntryResource,
				Versions: []string{EntryVersion}, Namespaced: true,
			}},
			Pages: []extension.Page{DesktopPage()},
			Preferences: []extension.PreferenceSchema{{
				ID: "dock", Keys: []string{PreferenceLayout},
			}},
		},
	}
}

// DesktopPage is the desktop C-52 describes: the entries, the installed
// applications, the recent list the browser holds and the notification centre's
// recent events. Only the entries carry a modification entry.
func DesktopPage() extension.Page {
	return extension.Page{
		ID:       PageID,
		Title:    "桌面",
		Location: "desktop",
		Items: []extension.PageItem{
			{
				ID: "entries", Label: "桌面条目",
				Source: extension.PageSourceObject, Reference: EntryKind,
				Action: ActionRemoveEntry,
			},
			{
				ID: "applications", Label: "已安装应用",
				Source: extension.PageSourceObject, Reference: "Application",
				Reason: "桌面只打开应用，不修改应用",
			},
			{
				ID: "recent", Label: "最近使用",
				Source: extension.PageSourceLocal, Store: "desktop.recent",
				Reason: "最近使用只存在于浏览器",
			},
			{
				ID: "notifications", Label: "通知中心",
				Source: extension.PageSourceObject, Reference: "Event",
				Reason: "只读展示最近事件",
			},
		},
		Actions: []extension.Action{
			{ID: ActionAddEntry, Method: http.MethodPost, Path: "/api/v1/resources/" + EntryKind},
			{ID: ActionMoveEntry, Method: http.MethodPut, Path: "/api/v1/resources/" + EntryKind},
			{ID: ActionRemoveEntry, Method: http.MethodDelete, Path: "/api/v1/resources/" + EntryKind},
		},
	}
}
