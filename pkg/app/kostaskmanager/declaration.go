// Package kostaskmanager is the kos-taskmanager product: the run view of the
// panel. It declares the overview and application pages of C-49 and reads their
// items from the cluster: node readiness, abnormal objects, pending upgrades,
// recent events, maintenance, installed applications, the installable set, the
// resource sample and the lifecycle entries C-66 and C-67 carry.
package kostaskmanager

import (
	"net/http"

	"github.com/tsic404/kos/pkg/extension"
)

// Identifier is the product's extension identity across versions (C-17).
const Identifier = "kos-taskmanager"

// Page identities the product declares (C-22).
const (
	// PageOverview is the panel overview (C-49).
	PageOverview = "overview"
	// PageApplications is the installed-application view (C-49).
	PageApplications = "applications"
)

// Item identities of the overview page, one per source C-49 names.
const (
	ItemNodesReady          = "nodes-ready"
	ItemAbnormalObjects     = "abnormal-objects"
	ItemPendingUpgrades     = "pending-upgrades"
	ItemRecentEvents        = "recent-events"
	ItemMaintenanceNodes    = "maintenance-nodes"
	ItemMaintenanceAffected = "maintenance-affected-applications"
)

// Item identities of the application page. The lifecycle items carry the
// control-plane entry that performs them (C-66, C-67).
const (
	ItemInstalled     = "installed-applications"
	ItemInstallable   = "installable-applications"
	ItemResourceUsage = "application-usage"
	ItemStop          = "stop-applications"
	ItemStart         = "start-applications"
	ItemRestart       = "restart-applications"
	ItemScale         = "scale-applications"
	ItemResourceSpec  = "application-resources"
	ItemUpgrade       = "application-upgrade"
	ItemUninstall     = "application-uninstall"
)

// OverviewReason explains the one read-only page of the pair (C-53).
const OverviewReason = "本页只读：节点、事件与升级状态由运行内核与版本发现持有，修改经应用页与系统设置发起"

// Action identities of the application page (C-22). The browser supplies the
// target application in the request body.
const (
	ActionStop         = "stop-application"
	ActionStart        = "start-application"
	ActionRestart      = "restart-application"
	ActionScale        = "scale-application"
	ActionResourceSpec = "set-application-resources"
	ActionUpgrade      = "upgrade-application"
	ActionUninstall    = "uninstall-application"
)

// Control-plane paths the entries write through (C-12, C-22).
const (
	pathActionRequests = "/api/v1/action-requests"
	pathApplications   = "/api/v1/resources/Application"
)

// Display reasons of the items on the writable application page that are not
// themselves modification entries (C-53).
const (
	ReasonInstalled   = "应用事实由应用调和写入，修改经同一页的操作入口"
	ReasonInstallable = "可安装列表按定义与能力需求计算，安装经应用中心发起"
	ReasonUsage       = "资源占用按指标采样读取，页面不写入"
)

// Declaration returns the contribution kos-taskmanager ships (C-18): the two
// pages C-49 puts in the panel and the lifecycle entries the application page
// offers.
func Declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: Identifier,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Pages: []extension.Page{overviewPage(), applicationsPage()},
		},
	}
}

// overviewPage declares the overview items C-49 names.
func overviewPage() extension.Page {
	return extension.Page{
		ID:             PageOverview,
		Title:          "概览",
		Location:       "panel",
		ReadOnly:       true,
		ReadOnlyReason: OverviewReason,
		Items: []extension.PageItem{
			{ID: ItemNodesReady, Label: "节点就绪", Source: extension.PageSourceObject, Reference: reference(ItemNodesReady)},
			{ID: ItemAbnormalObjects, Label: "异常对象", Source: extension.PageSourceComputed, Reference: reference(ItemAbnormalObjects)},
			{ID: ItemPendingUpgrades, Label: "待处理升级", Source: extension.PageSourceComputed, Reference: reference(ItemPendingUpgrades)},
			{ID: ItemRecentEvents, Label: "最近事件", Source: extension.PageSourceObject, Reference: reference(ItemRecentEvents)},
			{ID: ItemMaintenanceNodes, Label: "维护中的节点", Source: extension.PageSourceObject, Reference: reference(ItemMaintenanceNodes)},
			{ID: ItemMaintenanceAffected, Label: "维护受影响的 node-local 应用", Source: extension.PageSourceComputed, Reference: reference(ItemMaintenanceAffected)},
		},
	}
}

// applicationsPage declares the application items C-49 names: the installed and
// installable sets, the resource sample, and the stop, start, restart, scale,
// resource, upgrade and uninstall entries.
func applicationsPage() extension.Page {
	return extension.Page{
		ID:       PageApplications,
		Title:    "应用",
		Location: "panel",
		Items: []extension.PageItem{
			{ID: ItemInstalled, Label: "已安装应用与健康", Source: extension.PageSourceObject, Reference: reference(ItemInstalled), Reason: ReasonInstalled},
			{ID: ItemInstallable, Label: "可安装列表", Source: extension.PageSourceComputed, Reference: reference(ItemInstallable), Reason: ReasonInstallable},
			{ID: ItemResourceUsage, Label: "资源占用", Source: extension.PageSourceMetric, Reference: reference(ItemResourceUsage), Reason: ReasonUsage},
			{ID: ItemStop, Label: "停止应用", Source: extension.PageSourceObject, Reference: reference(ItemStop), Action: ActionStop},
			{ID: ItemStart, Label: "启动应用", Source: extension.PageSourceObject, Reference: reference(ItemStart), Action: ActionStart},
			{ID: ItemRestart, Label: "重启应用", Source: extension.PageSourceObject, Reference: reference(ItemRestart), Action: ActionRestart},
			{ID: ItemScale, Label: "扩缩容", Source: extension.PageSourceObject, Reference: reference(ItemScale), Action: ActionScale},
			{ID: ItemResourceSpec, Label: "资源规格", Source: extension.PageSourceObject, Reference: reference(ItemResourceSpec), Action: ActionResourceSpec},
			{ID: ItemUpgrade, Label: "升级应用", Source: extension.PageSourceObject, Reference: reference(ItemUpgrade), Action: ActionUpgrade},
			{ID: ItemUninstall, Label: "卸载应用", Source: extension.PageSourceObject, Reference: reference(ItemUninstall), Action: ActionUninstall},
		},
		Actions: []extension.Action{
			{ID: ActionStop, Method: http.MethodPost, Path: pathActionRequests},
			{ID: ActionStart, Method: http.MethodPost, Path: pathActionRequests},
			{ID: ActionRestart, Method: http.MethodPost, Path: pathActionRequests},
			{ID: ActionScale, Method: http.MethodPost, Path: pathActionRequests},
			{ID: ActionResourceSpec, Method: http.MethodPut, Path: pathApplications},
			{ID: ActionUpgrade, Method: http.MethodPut, Path: pathApplications},
			{ID: ActionUninstall, Method: http.MethodDelete, Path: pathApplications},
		},
	}
}

// reference is the item reference the product owns; the router dispatches by
// the identifier in front of it (C-22).
func reference(item string) string { return Identifier + "/" + item }
