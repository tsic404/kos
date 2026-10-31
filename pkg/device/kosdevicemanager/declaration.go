// Package kosdevicemanager is the kos-devicemanager product: the device view of
// the panel. It declares the read-only device page of C-50 and reads node
// hardware, GPU facts, the health alerts derived from them and the stale and
// invalid markers C-37 defines. The page has no modification entry: device
// facts are written by the node collector alone.
package kosdevicemanager

import "github.com/tsic404/kos/pkg/extension"

// Identifier is the product's extension identity across versions (C-17).
const Identifier = "kos-devicemanager"

// PageDevices is the device page (C-50).
const PageDevices = "devices"

// Item identities of the device page, one per source C-50 names.
const (
	ItemHardware = "node-hardware"
	ItemHealth   = "device-health-alerts"
	ItemMarkers  = "stale-and-invalid"
)

// ReadOnlyReason explains why the page offers no write entry (C-37, C-50).
const ReadOnlyReason = "本页只读：设备事实由节点采集器按 C-37 写入，KOS 与面板都不写设备事实，也不写节点容量或调度信息"

// Declaration returns the contribution kos-devicemanager ships (C-18).
func Declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: Identifier,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Pages: []extension.Page{devicesPage()},
		},
	}
}

// devicesPage declares the device items C-50 names.
func devicesPage() extension.Page {
	return extension.Page{
		ID:             PageDevices,
		Title:          "设备",
		Location:       "panel",
		ReadOnly:       true,
		ReadOnlyReason: ReadOnlyReason,
		Items: []extension.PageItem{
			{ID: ItemHardware, Label: "节点硬件与 GPU", Source: extension.PageSourceObject, Reference: reference(ItemHardware)},
			{ID: ItemHealth, Label: "健康告警", Source: extension.PageSourceComputed, Reference: reference(ItemHealth)},
			{ID: ItemMarkers, Label: "陈旧与无效标记", Source: extension.PageSourceObject, Reference: reference(ItemMarkers)},
		},
	}
}

// reference is the item reference the product owns; the router dispatches by
// the identifier in front of it (C-22).
func reference(item string) string { return Identifier + "/" + item }
