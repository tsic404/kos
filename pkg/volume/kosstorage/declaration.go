// Package kosstorage is the kos-storage product: the storage view of the panel.
// It declares the storage page of C-49 and reads device facts, storage classes,
// volumes and their phases, usage samples, backup policies and their newest
// drill, plus the migration entry C-40 carries. It never reads or writes the
// data inside a volume.
package kosstorage

import (
	"net/http"

	"github.com/tsic404/kos/pkg/extension"
)

// Identifier is the product's extension identity across versions (C-17).
const Identifier = "kos-storage"

// PageStorage is the storage page (C-49).
const PageStorage = "storage"

// Item identities of the storage page, one per source C-49 names.
const (
	ItemDevicesAndPolicies = "devices-and-policies"
	ItemVolumes            = "volumes-and-phases"
	ItemUsage              = "volume-usage"
	ItemBackups            = "backup-status"
	ItemMigration          = "migrate-volume"
)

// ActionMigration starts a VolumeMigration; the browser names the source volume
// and target storage class in the request body (C-40).
const ActionMigration = "start-volume-migration"

// pathMigrations is the control-plane class a migration is created under (C-12).
const pathMigrations = "/api/v1/resources/VolumeMigration"

// Display reasons of the items that carry no entry of their own (C-53).
const (
	ReasonDevices = "设备事实由采集适配器写入、存储类由 Provider 登记，本页只读展示"
	ReasonVolumes = "卷与阶段由卷调和写入，修改经卷资源接口发起"
	ReasonUsage   = "使用量按指标采样读取，页面不写入"
	ReasonBackups = "备份状态由备份策略调和写入，修改经备份策略资源接口发起"
)

// Declaration returns the contribution kos-storage ships (C-18): the storage
// page and its migration entry.
func Declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: Identifier,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Pages: []extension.Page{storagePage()},
		},
	}
}

// storagePage declares the storage items C-49 names and the migration entry
// C-40 carries.
func storagePage() extension.Page {
	return extension.Page{
		ID:       PageStorage,
		Title:    "存储",
		Location: "panel",
		Items: []extension.PageItem{
			{ID: ItemDevicesAndPolicies, Label: "设备与策略", Source: extension.PageSourceObject, Reference: reference(ItemDevicesAndPolicies), Reason: ReasonDevices},
			{ID: ItemVolumes, Label: "卷与阶段", Source: extension.PageSourceObject, Reference: reference(ItemVolumes), Reason: ReasonVolumes},
			{ID: ItemUsage, Label: "使用率", Source: extension.PageSourceMetric, Reference: reference(ItemUsage), Reason: ReasonUsage},
			{ID: ItemBackups, Label: "备份状态与最近演练", Source: extension.PageSourceObject, Reference: reference(ItemBackups), Reason: ReasonBackups},
			{ID: ItemMigration, Label: "迁移卷", Source: extension.PageSourceObject, Reference: reference(ItemMigration), Action: ActionMigration},
		},
		Actions: []extension.Action{
			{ID: ActionMigration, Method: http.MethodPost, Path: pathMigrations},
		},
	}
}

// reference is the item reference the product owns; the router dispatches by
// the identifier in front of it (C-22).
func reference(item string) string { return Identifier + "/" + item }
