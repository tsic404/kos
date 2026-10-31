// Package filemanager is the kos-filemanager first-party extension package: it
// contributes the exposed-volume setting and the browser view preferences, and
// runs its own data-plane service for the files inside the volumes a session may
// open (C-23, C-27, C-38, C-60).
//
// The kernel offers no generic file interface (NG10): kos-shell keeps the
// volume's lifecycle and access ownership, while this package serves browsing,
// upload, download, rename and delete from its own handler. The interface opens
// as an application window, so the package declares no panel page.
package filemanager

import (
	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/extension"
)

// The package identity and the surface its declaration names (C-17, C-18).
const (
	// Identifier is the extension identity across versions.
	Identifier = "kos-filemanager"
	// Version is the package version this declaration belongs to.
	Version = "1.0.0"
)

// The contribution surface: one user setting and one preference structure.
const (
	// SettingExposedVolumes names the volumes the account exposes to the file
	// browser. Its value is a JSON list of "namespace/name" references.
	SettingExposedVolumes = "filemanager.exposedVolumes"
	// PreferenceSchema names the preference domain of the file browser.
	PreferenceSchema = "filemanager"
	// PreferenceView is the browser view the window opens with.
	PreferenceView = "view"
	// PreferenceInitialPath is the path the window opens at.
	PreferenceInitialPath = "initialPath"
)

// The views a window may open with (C-27). An unknown stored value falls back
// to the list view instead of failing the window.
const (
	// ViewList shows one entry per row.
	ViewList = "list"
	// ViewGrid shows entries as a grid.
	ViewGrid = "grid"
	// DefaultView is the view used when the account stored none.
	DefaultView = ViewList
)

// Declaration returns the package declaration (C-18): one user setting for the
// exposed volumes and the view/initial-path preference structure.
func Declaration() extension.Declaration {
	return extension.Declaration{
		Identifier:       Identifier,
		Version:          Version,
		FrameworkVersion: ">=1.0 <2.0",
		Contributions: extension.Contributions{
			Settings: []extension.Setting{{
				ID:       SettingExposedVolumes,
				Scope:    extension.SettingScopeUser,
				Writer:   extension.SettingWriterUser,
				Editable: true,
			}},
			Preferences: []extension.PreferenceSchema{{
				ID:   PreferenceSchema,
				Keys: []string{PreferenceView, PreferenceInitialPath},
			}},
		},
		Permissions: []extension.Permission{{
			Contribution: SettingExposedVolumes,
			ReadScope:    extension.ReadScope{Kinds: []extension.GroupKind{{Group: api.Group, Kind: "Volume"}}},
		}},
		Configuration: extension.Configuration{Schema: exposedVolumesSchema()},
	}
}

// exposedVolumesSchema describes the setting value so a reader can render the
// form without knowing the package internals (C-18).
func exposedVolumesSchema() map[string]any {
	return map[string]any{
		"exposedVolumes": map[string]any{
			"type":        "array",
			"description": "本账号在文件浏览器中暴露的卷，元素为 namespace/name",
			"items":       map[string]any{"type": "string"},
		},
	}
}
