// Package kosupdater is the kos-updater product: the component and update views
// of the panel. It declares the two pages of C-51 and reads installed and
// verified capabilities, capability gaps, the installable set, blocked
// providers, extension enablement with missing dependencies, the version matrix
// and the irreversible-upgrade warnings.
package kosupdater

import "github.com/tsic404/kos/pkg/extension"

// Identifier is the product's extension identity across versions (C-17).
const Identifier = "kos-updater"

// Page identities the product declares (C-22).
const (
	// PageComponents is the component view (C-51).
	PageComponents = "components"
	// PageUpdates is the update view (C-51).
	PageUpdates = "updates"
)

// Item identities of the component page, one per source C-51 names.
const (
	ItemCapabilities     = "installed-capabilities"
	ItemGaps             = "capability-gaps"
	ItemInstallable      = "installable-components"
	ItemBlockedProviders = "blocked-providers"
	ItemExtensionState   = "extension-state"
)

// Item identities of the update page, one per source C-51 names.
const (
	ItemVersions            = "versions"
	ItemApplicationUpgrades = "application-upgrades"
	ItemIrreversible        = "irreversible-upgrades"
	ItemCompatibility       = "extension-compatibility"
)

// Read-only reasons: neither page carries a modification entry in C-51.
const (
	ComponentsReadOnlyReason = "本页只读：组件启停经系统组件资源与安装流程发起，能力来自 Provider 注册与验证结果"
	UpdatesReadOnlyReason    = "本页只读：应用升级经应用资源接口发起，平台与扩展升级由安装流程执行"
)

// Declaration returns the contribution kos-updater ships (C-18).
func Declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: Identifier,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Pages: []extension.Page{componentsPage(), updatesPage()},
		},
	}
}

// componentsPage declares the component items C-51 names.
func componentsPage() extension.Page {
	return extension.Page{
		ID:             PageComponents,
		Title:          "组件",
		Location:       "panel",
		ReadOnly:       true,
		ReadOnlyReason: ComponentsReadOnlyReason,
		Items: []extension.PageItem{
			{ID: ItemCapabilities, Label: "已装能力", Source: extension.PageSourceObject, Reference: reference(ItemCapabilities)},
			{ID: ItemGaps, Label: "能力缺口", Source: extension.PageSourceComputed, Reference: reference(ItemGaps)},
			{ID: ItemInstallable, Label: "可安装组件", Source: extension.PageSourceComputed, Reference: reference(ItemInstallable)},
			{ID: ItemBlockedProviders, Label: "升级被阻塞的 Provider 与依赖者", Source: extension.PageSourceObject, Reference: reference(ItemBlockedProviders)},
			{ID: ItemExtensionState, Label: "扩展的启用状态与缺失依赖", Source: extension.PageSourceObject, Reference: reference(ItemExtensionState)},
		},
	}
}

// updatesPage declares the update items C-51 names.
func updatesPage() extension.Page {
	return extension.Page{
		ID:             PageUpdates,
		Title:          "更新",
		Location:       "panel",
		ReadOnly:       true,
		ReadOnlyReason: UpdatesReadOnlyReason,
		Items: []extension.PageItem{
			{ID: ItemVersions, Label: "KOS 版本与集群版本", Source: extension.PageSourceComputed, Reference: reference(ItemVersions)},
			{ID: ItemApplicationUpgrades, Label: "应用升级", Source: extension.PageSourceObject, Reference: reference(ItemApplicationUpgrades)},
			{ID: ItemIrreversible, Label: "不可逆升级提示", Source: extension.PageSourceComputed, Reference: reference(ItemIrreversible)},
			{ID: ItemCompatibility, Label: "扩展与框架版本兼容性", Source: extension.PageSourceComputed, Reference: reference(ItemCompatibility)},
		},
	}
}

// reference is the item reference the product owns; the router dispatches by
// the identifier in front of it (C-22).
func reference(item string) string { return Identifier + "/" + item }
