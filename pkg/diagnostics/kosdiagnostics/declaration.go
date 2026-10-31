// Package kosdiagnostics is the kos-diagnostics product: the diagnostics view of
// the panel. It declares the diagnostics page of C-51 and reads the degradation
// reasons and next actions, the blocked uninstall and volume objects, the
// evidence chain of each unhealthy application, and the diagnostic-bundle export
// entry C-26 carries.
package kosdiagnostics

import (
	"net/http"

	"github.com/tsic404/kos/pkg/extension"
)

// Identifier is the product's extension identity across versions (C-17).
const Identifier = "kos-diagnostics"

// PageDiagnostics is the diagnostics page (C-51).
const PageDiagnostics = "diagnostics"

// Item identities of the diagnostics page, one per source C-51 names.
const (
	ItemDegradation = "degradation-reasons"
	ItemBlocked     = "blocked-objects"
	ItemEvidence    = "evidence-segments"
	ItemExport      = "export-bundle"
)

// ActionExport assembles one application's diagnostic bundle; the browser names
// the application in the request body (C-22, C-26).
const ActionExport = "export-diagnostic-bundle"

// pathBundles is the control-plane export class the action writes to (C-12).
const pathBundles = "/api/v1/diagnostics/bundles"

// Display reasons of the items that carry no entry of their own (C-53).
const (
	ReasonDegradation = "降级原因按条件聚合计算，页面不写入"
	ReasonBlocked     = "阻塞对象由应用与卷调和写入，页面不写入"
	ReasonEvidence    = "证据沿引用链读取，页面不写入"
)

// Declaration returns the contribution kos-diagnostics ships (C-18): the
// diagnostics page and its export entry.
func Declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: Identifier,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Pages: []extension.Page{diagnosticsPage()},
		},
	}
}

// diagnosticsPage declares the diagnostics items C-51 names.
func diagnosticsPage() extension.Page {
	return extension.Page{
		ID:       PageDiagnostics,
		Title:    "诊断",
		Location: "panel",
		Items: []extension.PageItem{
			{ID: ItemDegradation, Label: "降级原因与建议动作", Source: extension.PageSourceComputed, Reference: reference(ItemDegradation), Reason: ReasonDegradation},
			{ID: ItemBlocked, Label: "卸载卡住与阻塞对象", Source: extension.PageSourceObject, Reference: reference(ItemBlocked), Reason: ReasonBlocked},
			{ID: ItemEvidence, Label: "各段证据", Source: extension.PageSourceObject, Reference: reference(ItemEvidence), Reason: ReasonEvidence},
			{ID: ItemExport, Label: "诊断包导出", Source: extension.PageSourceComputed, Reference: reference(ItemExport), Action: ActionExport},
		},
		Actions: []extension.Action{
			{ID: ActionExport, Method: http.MethodPost, Path: pathBundles},
		},
	}
}

// reference is the item reference the product owns; the router dispatches by
// the identifier in front of it (C-22).
func reference(item string) string { return Identifier + "/" + item }
