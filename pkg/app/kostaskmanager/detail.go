package kostaskmanager

import (
	"github.com/tsic404/kos/pkg/extension"
)

// PageApplicationDetail is the application detail page (C-52): one
// application's health, volumes, endpoints, backup, drift, events and logs.
// The page shows a single application, so it is read with a target; the
// modification entries stay on the applications page, which is why this page is
// read-only and says so (C-53).
const PageApplicationDetail = "application-detail"

// DetailReason explains the detail page's read-only state (C-53).
const DetailReason = "本页只读：详情页展示单个应用的事实，停止、启动、重启、扩缩容与卸载经应用页发起"

// Item identities of the application detail page, one per source C-52 names.
// The log item is a read source; the others read an object.
const (
	ItemDetailHealth    = "detail-health"
	ItemDetailVolumes   = "detail-volumes"
	ItemDetailEndpoints = "detail-endpoints"
	ItemDetailBackup    = "detail-backup"
	ItemDetailDrift     = "detail-drift"
	ItemDetailEvents    = "detail-events"
	ItemDetailLogs      = "detail-logs"
)

// applicationDetailPage declares the items C-52 puts on the detail page.
func applicationDetailPage() extension.Page {
	return extension.Page{
		ID:             PageApplicationDetail,
		Title:          "应用详情",
		Location:       "panel",
		ReadOnly:       true,
		ReadOnlyReason: DetailReason,
		Items: []extension.PageItem{
			{ID: ItemDetailHealth, Label: "健康", Source: extension.PageSourceObject, Reference: reference(ItemDetailHealth)},
			{ID: ItemDetailVolumes, Label: "卷", Source: extension.PageSourceObject, Reference: reference(ItemDetailVolumes)},
			{ID: ItemDetailEndpoints, Label: "入口", Source: extension.PageSourceObject, Reference: reference(ItemDetailEndpoints)},
			{ID: ItemDetailBackup, Label: "备份", Source: extension.PageSourceObject, Reference: reference(ItemDetailBackup)},
			{ID: ItemDetailDrift, Label: "漂移", Source: extension.PageSourceObject, Reference: reference(ItemDetailDrift)},
			{ID: ItemDetailEvents, Label: "事件", Source: extension.PageSourceObject, Reference: reference(ItemDetailEvents)},
			{ID: ItemDetailLogs, Label: "日志", Source: extension.PageSourceRead, Reference: reference(ItemDetailLogs)},
		},
	}
}
