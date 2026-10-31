// Package kossettings is the kos-settings product: the cluster and node
// settings the panel shows (C-36), the modifiable items the product contributes
// (C-23), and the sources its settings and user pages read (C-50, C-53). Every
// item below is this package's own declaration; the kernel hardcodes none of
// them, so the items appear and disappear with the component (C-23, F16).
package kossettings

import (
	"net/http"

	"github.com/tsic404/kos/pkg/extension"
)

// Identifier is the product's extension identity across versions (C-17).
const Identifier = "kos-settings"

// Item identities of the cluster settings C-36 covers.
const (
	ClusterKubernetesVersion = "cluster-kubernetes-version"
	ClusterNodesAndRoles     = "cluster-nodes-and-roles"
	ClusterResourceUsage     = "cluster-resource-usage"
	ClusterSystemComponents  = "cluster-system-components"
	ClusterNetworkAndStorage = "cluster-network-and-storage"
	ClusterHealth            = "cluster-health"
	ClusterMaintenanceWindow = "cluster-maintenance-window"
)

// Item identities of the node settings C-36 covers.
const (
	NodeIdentity              = "node-identity"
	NodeStatus                = "node-status"
	NodeResources             = "node-resources"
	NodeMaintenanceState      = "node-maintenance-state"
	NodeMaintenanceOrigin     = "node-maintenance-origin"
	NodeMaintenanceAffected   = "node-maintenance-affected-apps"
	NodeDevices               = "node-devices"
	NodeProviderStatus        = "node-provider-status"
	NodeSchedulingConstraints = "node-scheduling-constraints"
	NodeReservedResources     = "node-reserved-resources"
	NodeHostConfig            = "node-host-config"
)

// Page identities the product declares (C-22).
const (
	PageSettings = "settings"
	PageUsers    = "users"
)

// Page item identities of the user page, one per source C-50 names.
const (
	UserMembers                = "members"
	ApplicationVisibility      = "application-visibility"
	ApplicationAndVolumeOwners = "owners"
)

// Action identities of the settings page. The browser appends the selected node
// to the path for a node-scoped item.
const (
	ActionSetMaintenanceWindow = "set-cluster-maintenance-window"
	ActionSetReservedResources = "set-node-reserved-resources"
)

// ObservedReason is the reason a fact item carries: the product reads it from
// the cluster or the control-plane database and the user does not write it
// (C-36, C-53).
const ObservedReason = "该项由 kos-settings 按集群事实读取，用户不能修改"

// HostConfigReason explains the one node setting the runtime kernel exposes
// neither for reading nor for writing (C-36).
const HostConfigReason = "该项是节点主机上的 kubelet 配置，Kubernetes API 既不可读也不可写"

// Declaration returns the contribution kos-settings ships (C-18): the two
// settings items the user owns and the two pages C-36 and C-50 put in the
// panel.
func Declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: Identifier,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Settings: []extension.Setting{
				// A cluster-level policy the user owns (C-36).
				{ID: ClusterMaintenanceWindow, Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser, Editable: true},
				// The reservation per node; the node is the write target (C-36).
				{ID: NodeReservedResources, Scope: extension.SettingScopeNode, Writer: extension.SettingWriterUser, Editable: true},
			},
			Pages: []extension.Page{settingsPage(), usersPage()},
		},
	}
}

// settingsPage declares every cluster and node setting C-36 covers. A fact is
// read from its source and carries a reason instead of an entry; the two
// modifiable items reference the control-plane writes that change them.
func settingsPage() extension.Page {
	return extension.Page{
		ID:       PageSettings,
		Title:    "系统设置",
		Location: "panel",
		Items: []extension.PageItem{
			// Cluster settings (C-36).
			{ID: ClusterKubernetesVersion, Label: "Kubernetes 版本与能力", Source: extension.PageSourceObject, Reference: ClusterKubernetesVersion, Reason: ObservedReason},
			{ID: ClusterNodesAndRoles, Label: "节点与角色", Source: extension.PageSourceObject, Reference: ClusterNodesAndRoles, Reason: ObservedReason},
			{ID: ClusterResourceUsage, Label: "资源占用", Source: extension.PageSourceComputed, Reference: ClusterResourceUsage, Reason: ObservedReason},
			{ID: ClusterSystemComponents, Label: "系统组件", Source: extension.PageSourceObject, Reference: ClusterSystemComponents, Reason: ObservedReason},
			{ID: ClusterNetworkAndStorage, Label: "网络与存储能力", Source: extension.PageSourceObject, Reference: ClusterNetworkAndStorage, Reason: ObservedReason},
			{ID: ClusterHealth, Label: "集群健康", Source: extension.PageSourceComputed, Reference: ClusterHealth, Reason: ObservedReason},
			{ID: ClusterMaintenanceWindow, Label: "集群维护窗口", Source: extension.PageSourceDatabase, Reference: ClusterMaintenanceWindow, Action: ActionSetMaintenanceWindow},
			// Node settings (C-36). Each value is keyed by node, because the
			// panel reads one node at a time from the same projection.
			{ID: NodeIdentity, Label: "身份与标签", Source: extension.PageSourceObject, Reference: NodeIdentity, Reason: ObservedReason},
			{ID: NodeStatus, Label: "节点状态", Source: extension.PageSourceObject, Reference: NodeStatus, Reason: ObservedReason},
			{ID: NodeResources, Label: "节点资源", Source: extension.PageSourceObject, Reference: NodeResources, Reason: ObservedReason},
			{ID: NodeMaintenanceState, Label: "维护状态", Source: extension.PageSourceObject, Reference: NodeMaintenanceState, Reason: ObservedReason},
			{ID: NodeMaintenanceOrigin, Label: "维护归属", Source: extension.PageSourceDatabase, Reference: NodeMaintenanceOrigin, Reason: ObservedReason},
			{ID: NodeMaintenanceAffected, Label: "维护受影响的 node-local 应用", Source: extension.PageSourceComputed, Reference: NodeMaintenanceAffected, Reason: ObservedReason},
			{ID: NodeDevices, Label: "设备清单", Source: extension.PageSourceObject, Reference: NodeDevices, Reason: ObservedReason},
			{ID: NodeProviderStatus, Label: "节点级 Provider 状态", Source: extension.PageSourceObject, Reference: NodeProviderStatus, Reason: ObservedReason},
			{ID: NodeSchedulingConstraints, Label: "调度约束", Source: extension.PageSourceObject, Reference: NodeSchedulingConstraints, Reason: ObservedReason},
			{ID: NodeReservedResources, Label: "资源预留", Source: extension.PageSourceDatabase, Reference: NodeReservedResources, Action: ActionSetReservedResources},
			{ID: NodeHostConfig, Label: "节点主机配置", Source: extension.PageSourceComputed, Reference: NodeHostConfig, Reason: HostConfigReason},
		},
		Actions: []extension.Action{
			{ID: ActionSetMaintenanceWindow, Method: http.MethodPut, Path: "/api/v1/settings/" + ClusterMaintenanceWindow},
			{ID: ActionSetReservedResources, Method: http.MethodPut, Path: "/api/v1/settings/" + NodeReservedResources},
		},
	}
}

// usersPage declares the three sources C-50 names for the user page: members
// and roles from the control-plane database, visibility and ownership from the
// objects. Member management belongs to the account surface, so the page is
// read-only and says why.
func usersPage() extension.Page {
	return extension.Page{
		ID:             PageUsers,
		Title:          "用户",
		Location:       "panel",
		ReadOnly:       true,
		ReadOnlyReason: "本页只读：成员与角色由账号接口管理，应用可见性与所有者由应用与卷对象持有",
		Items: []extension.PageItem{
			{ID: UserMembers, Label: "成员与角色", Source: extension.PageSourceDatabase, Reference: UserMembers},
			{ID: ApplicationVisibility, Label: "应用可见性", Source: extension.PageSourceObject, Reference: ApplicationVisibility},
			{ID: ApplicationAndVolumeOwners, Label: "应用与卷所有者", Source: extension.PageSourceObject, Reference: ApplicationAndVolumeOwners},
		},
	}
}
