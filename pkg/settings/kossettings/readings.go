package kossettings

import (
	"context"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/settings"
)

// Audit verbs the maintenance flow records (C-65): a start writes the node's
// unschedulable field and a restore clears it. The origin of an unschedulable
// node is judged from the newest of the two records.
const (
	AuditVerbMaintain = "maintain"
	AuditVerbRestore  = "restore"
)

// Maintenance origin values C-36 reports.
const (
	MaintenanceNone      = "none"
	MaintenancePlanned   = "planned"
	MaintenanceUnplanned = "unplanned"
	MaintenanceUnknown   = "unknown"
)

// auditLimit bounds one read of the maintenance trail.
const auditLimit = 500

// SettingReader projects the setting items the product declared (C-23).
type SettingReader interface {
	Items(ctx context.Context, session identity.Session, node string) ([]settings.Item, *controlplane.Error)
}

// AccountReader lists the local accounts and their roles (C-50, C-63).
type AccountReader interface {
	ListUsers(ctx context.Context) ([]identity.User, error)
}

// AuditReader reads the control-plane audit trail the maintenance origin comes
// from (C-14, C-65).
type AuditReader interface {
	AuditEntries(ctx context.Context, limit int) ([]identity.AuditEntry, error)
}

// Readings resolves every item the product's pages declare (C-53): the cluster
// and node facts of C-36, the setting values it contributes, and the user page
// sources of C-50. Each read stamps the moment it observed the value, so the
// page shell applies the one freshness bound of C-13.
type Readings struct {
	// Dynamic reads the cluster objects the object and computed items judge.
	Dynamic dynamic.Interface
	// Settings projects the setting items this product contributes.
	Settings SettingReader
	// Accounts lists the local accounts of the user page; nil leaves the page
	// unreadable rather than showing an invented membership.
	Accounts AccountReader
	// Audit reads the maintenance trail; nil leaves the origin unknown.
	Audit AuditReader
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
}

// Read implements pages.Readings.
func (r *Readings) Read(ctx context.Context, session identity.Session, item extension.PageItem) (pages.Reading, *controlplane.Error) {
	value, failure := r.value(ctx, session, item)
	if failure != nil {
		return pages.Reading{}, failure
	}
	return pages.Reading{Value: value, ObservedAt: r.now()}, nil
}

// value resolves one item from the source it declares.
func (r *Readings) value(ctx context.Context, session identity.Session, item extension.PageItem) (any, *controlplane.Error) {
	switch item.ID {
	case ClusterKubernetesVersion:
		return r.kubernetesVersions(ctx)
	case ClusterNodesAndRoles:
		return r.clusterNodes(ctx)
	case ClusterResourceUsage:
		return r.clusterResources(ctx)
	case ClusterSystemComponents:
		return r.systemComponents(ctx)
	case ClusterNetworkAndStorage:
		return r.providers(ctx)
	case ClusterHealth:
		return r.clusterHealth(ctx)
	case ClusterMaintenanceWindow:
		return r.settingValue(ctx, session, "", ClusterMaintenanceWindow)
	case NodeIdentity:
		return r.nodeIdentities(ctx)
	case NodeStatus:
		return r.nodeStatuses(ctx)
	case NodeResources:
		return r.nodeResources(ctx)
	case NodeMaintenanceState:
		return r.nodeMaintenanceStates(ctx)
	case NodeMaintenanceOrigin:
		return r.nodeMaintenanceOrigins(ctx)
	case NodeMaintenanceAffected:
		return r.nodeMaintenanceAffected(ctx)
	case NodeDevices:
		return r.nodeDevices(ctx)
	case NodeProviderStatus:
		return r.nodeProviders(ctx)
	case NodeSchedulingConstraints:
		return r.nodeConstraints(ctx)
	case NodeReservedResources:
		return r.reservedResources(ctx, session)
	case NodeHostConfig:
		// The host-side configuration is not reachable through the runtime
		// kernel; the item states that rather than showing a made-up value
		// (C-36).
		return map[string]any{"readable": false}, nil
	case UserMembers:
		return r.members(ctx)
	case ApplicationVisibility:
		return r.applicationVisibility(ctx)
	case ApplicationAndVolumeOwners:
		return r.owners(ctx)
	default:
		return nil, unreadable("页面项 " + item.ID + " 没有来源")
	}
}

// nodeRoles is one node and the roles its labels declare.
type nodeRoles struct {
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

// kubernetesVersions reports the kubelet version every node runs; the set is
// the cluster's versions, and a mixed set stays visible (C-36).
func (r *Readings) kubernetesVersions(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	seen := map[string]bool{}
	versions := make([]string, 0, len(nodes))
	for _, node := range nodes {
		version := node.Status.NodeInfo.KubeletVersion
		if version == "" || seen[version] {
			continue
		}
		seen[version] = true
		versions = append(versions, version)
	}
	sort.Strings(versions)
	return map[string]any{"versions": versions}, nil
}

// clusterNodes reports every node with the roles its labels name (C-36).
func (r *Readings) clusterNodes(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	entries := make([]nodeRoles, 0, len(nodes))
	for _, node := range nodes {
		entries = append(entries, nodeRoles{Name: node.Name, Roles: rolesOf(node)})
	}
	return entries, nil
}

// clusterResources reports the allocatable CPU and memory the nodes offer
// together; it is the pool C-36's resource item judges (C-36).
func (r *Readings) clusterResources(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	cpu := apiresource.Quantity{}
	memory := apiresource.Quantity{}
	for _, node := range nodes {
		cpu.Add(allocatable(node, corev1.ResourceCPU))
		memory.Add(allocatable(node, corev1.ResourceMemory))
	}
	return map[string]any{"cpu": cpu.String(), "memory": memory.String()}, nil
}

// componentSummary is one system component as the panel reads it.
type componentSummary struct {
	Name   string `json:"name"`
	Phase  string `json:"phase,omitempty"`
	Health string `json:"health,omitempty"`
}

// systemComponents reports the installed system components and their health
// (C-36).
func (r *Readings) systemComponents(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.kernel(ctx, "SystemComponent")
	if failure != nil {
		return nil, failure
	}
	entries := make([]componentSummary, 0, len(objects))
	for _, object := range objects {
		var component v1beta1.SystemComponent
		if failure := decode(object, &component); failure != nil {
			return nil, failure
		}
		entries = append(entries, componentSummary{
			Name:   component.Name,
			Phase:  component.Status.Phase,
			Health: string(component.Status.Health.State),
		})
	}
	return entries, nil
}

// providerSummary is one registered Provider as the panel reads it.
type providerSummary struct {
	Name           string   `json:"name"`
	Version        string   `json:"version,omitempty"`
	Health         string   `json:"health,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	StorageClasses []string `json:"storageClasses,omitempty"`
}

// providers reports the registered providers with their capabilities and
// storage classes; it is the network and storage part of C-36.
func (r *Readings) providers(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.kernel(ctx, "Provider")
	if failure != nil {
		return nil, failure
	}
	entries := make([]providerSummary, 0, len(objects))
	for _, object := range objects {
		var provider v1beta1.Provider
		if failure := decode(object, &provider); failure != nil {
			return nil, failure
		}
		entries = append(entries, summaryOf(provider))
	}
	return entries, nil
}

// clusterHealth aggregates the two facts C-36 lists as cluster health: every
// node ready and every system component healthy. A failing fact is named, so a
// degraded cluster says what degraded.
func (r *Readings) clusterHealth(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	reasons := []string{}
	for _, node := range nodes {
		if !isReady(node) {
			reasons = append(reasons, "节点 "+node.Name+" 未就绪")
		}
	}
	objects, failure := r.kernel(ctx, "SystemComponent")
	if failure != nil {
		return nil, failure
	}
	for _, object := range objects {
		var component v1beta1.SystemComponent
		if failure := decode(object, &component); failure != nil {
			return nil, failure
		}
		if component.Status.Health.State != v1beta1.HealthStateHealthy {
			reasons = append(reasons, "系统组件 "+component.Name+" 健康为 "+string(component.Status.Health.State))
		}
	}
	health := string(v1beta1.HealthStateHealthy)
	if len(reasons) > 0 {
		health = string(v1beta1.HealthStateDegraded)
	}
	return map[string]any{"health": health, "reasons": reasons}, nil
}

// nodeIdentity is one node's name and labels (C-36).
type nodeIdentity struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

// nodeIdentities reports every node's identity and labels (C-36).
func (r *Readings) nodeIdentities(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	entries := make([]nodeIdentity, 0, len(nodes))
	for _, node := range nodes {
		entries = append(entries, nodeIdentity{Name: node.Name, Labels: node.Labels})
	}
	return entries, nil
}

// nodeState is one node's readiness and schedulability (C-36).
type nodeState struct {
	Name        string `json:"name"`
	Ready       bool   `json:"ready"`
	Schedulable bool   `json:"schedulable"`
}

// nodeStatuses reports the readiness and schedulability of every node (C-36).
func (r *Readings) nodeStatuses(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	entries := make([]nodeState, 0, len(nodes))
	for _, node := range nodes {
		entries = append(entries, nodeState{Name: node.Name, Ready: isReady(node), Schedulable: !node.Spec.Unschedulable})
	}
	return entries, nil
}

// nodeCapacity is one node's allocatable CPU and memory (C-36).
type nodeCapacity struct {
	Name   string `json:"name"`
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// nodeResources reports every node's allocatable resources (C-36).
func (r *Readings) nodeResources(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	entries := make([]nodeCapacity, 0, len(nodes))
	for _, node := range nodes {
		cpu := allocatable(node, corev1.ResourceCPU)
		memory := allocatable(node, corev1.ResourceMemory)
		entries = append(entries, nodeCapacity{Name: node.Name, CPU: cpu.String(), Memory: memory.String()})
	}
	return entries, nil
}

// nodeSchedulability is one node's maintenance state (C-36, C-65).
type nodeSchedulability struct {
	Name        string `json:"name"`
	Schedulable bool   `json:"schedulable"`
}

// nodeMaintenanceStates reports whether each node is unschedulable, which is
// where C-65 keeps the maintenance state.
func (r *Readings) nodeMaintenanceStates(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	entries := make([]nodeSchedulability, 0, len(nodes))
	for _, node := range nodes {
		entries = append(entries, nodeSchedulability{Name: node.Name, Schedulable: !node.Spec.Unschedulable})
	}
	return entries, nil
}

// nodeOrigin is one node's maintenance origin (C-36).
type nodeOrigin struct {
	Name   string `json:"name"`
	Origin string `json:"origin"`
}

// nodeMaintenanceOrigins judges the maintenance origin of every node from the
// audit trail (C-65): a KOS write whose newest record is a start is planned, an
// unschedulable node without one is unplanned, and an unreadable trail is
// unknown rather than guessed.
func (r *Readings) nodeMaintenanceOrigins(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	var entries []identity.AuditEntry
	readable := true
	if r.Audit == nil {
		readable = false
	} else {
		trail, err := r.Audit.AuditEntries(ctx, auditLimit)
		if err != nil {
			readable = false
		}
		entries = trail
	}
	origins := make([]nodeOrigin, 0, len(nodes))
	for _, node := range nodes {
		origins = append(origins, nodeOrigin{Name: node.Name, Origin: originOf(node, entries, readable)})
	}
	return origins, nil
}

// originOf applies C-65's origin rule to one node.
func originOf(node corev1.Node, entries []identity.AuditEntry, readable bool) string {
	if !node.Spec.Unschedulable {
		return MaintenanceNone
	}
	if !readable {
		return MaintenanceUnknown
	}
	object := api.Group + "/Node/" + node.Name
	for index := len(entries) - 1; index >= 0; index-- {
		if entries[index].Object != object {
			continue
		}
		if entries[index].Verb == AuditVerbMaintain {
			return MaintenancePlanned
		}
		if entries[index].Verb == AuditVerbRestore {
			return MaintenanceUnplanned
		}
	}
	return MaintenanceUnplanned
}

// nodeApplications is one node and the node-local applications maintenance
// would stop (C-36, C-65).
type nodeApplications struct {
	Name         string   `json:"name"`
	Applications []string `json:"applications"`
}

// nodeMaintenanceAffected computes the node-local applications bound to each
// node: an application is affected when it references a node-local volume whose
// data sits on that node (C-38, C-65).
func (r *Readings) nodeMaintenanceAffected(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	affected, failure := r.affectedByNode(ctx)
	if failure != nil {
		return nil, failure
	}
	entries := make([]nodeApplications, 0, len(nodes))
	for _, node := range nodes {
		names := affected[node.Name]
		if names == nil {
			names = []string{}
		}
		entries = append(entries, nodeApplications{Name: node.Name, Applications: names})
	}
	return entries, nil
}

// affectedByNode maps a node to the names of the applications bound to it
// through a node-local volume.
func (r *Readings) affectedByNode(ctx context.Context) (map[string][]string, *controlplane.Error) {
	volumes, failure := r.kernel(ctx, "Volume")
	if failure != nil {
		return nil, failure
	}
	bound := map[string]string{}
	for _, object := range volumes {
		var volume v1beta1.Volume
		if failure := decode(object, &volume); failure != nil {
			return nil, failure
		}
		if volume.Spec.Mobility != v1beta1.VolumeMobilityNodeLocal || volume.Status.Node == "" {
			continue
		}
		bound[volume.Name] = volume.Status.Node
	}
	applications, failure := r.kernel(ctx, "Application")
	if failure != nil {
		return nil, failure
	}
	affected := map[string][]string{}
	for _, object := range applications {
		var application v1beta1.Application
		if failure := decode(object, &application); failure != nil {
			return nil, failure
		}
		nodes := map[string]bool{}
		for _, name := range application.Spec.Volumes {
			if node, ok := bound[name]; ok {
				nodes[node] = true
			}
		}
		for node := range nodes {
			affected[node] = append(affected[node], application.Name)
		}
	}
	for node := range affected {
		sort.Strings(affected[node])
	}
	return affected, nil
}

// nodeDevices is one node's device facts (C-36).
type nodeDevices struct {
	Name     string `json:"name"`
	Devices  int    `json:"devices"`
	Occupied int    `json:"occupied"`
}

// nodeDevices reports the device facts each node reported, with how many are
// occupied (C-37, C-36).
func (r *Readings) nodeDevices(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.kernel(ctx, "NodeDevice")
	if failure != nil {
		return nil, failure
	}
	entries := make([]nodeDevices, 0, len(objects))
	for _, object := range objects {
		var device v1beta1.NodeDevice
		if failure := decode(object, &device); failure != nil {
			return nil, failure
		}
		entry := nodeDevices{Name: device.Name, Devices: len(device.Spec.Devices)}
		for _, fact := range device.Spec.Devices {
			if fact.Occupancy != nil {
				entry.Occupied++
			}
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name < entries[right].Name })
	return entries, nil
}

// nodeProviders reports the node-level provider status per node: the
// registration is cluster-scoped, so a node reports the providers that declare a
// node-scoped capability or that own a volume bound to it (C-36, C-47).
func (r *Readings) nodeProviders(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	objects, failure := r.kernel(ctx, "Provider")
	if failure != nil {
		return nil, failure
	}
	var nodeLevel []providerSummary
	known := map[string]providerSummary{}
	for _, object := range objects {
		var provider v1beta1.Provider
		if failure := decode(object, &provider); failure != nil {
			return nil, failure
		}
		summary := summaryOf(provider)
		known[provider.Name] = summary
		if len(nodeScoped(provider)) > 0 {
			nodeLevel = append(nodeLevel, summary)
		}
	}
	bound, failure := r.providersByNode(ctx)
	if failure != nil {
		return nil, failure
	}
	type nodeProviders struct {
		Name      string            `json:"name"`
		Providers []providerSummary `json:"providers"`
	}
	entries := make([]nodeProviders, 0, len(nodes))
	for _, node := range nodes {
		providers := append([]providerSummary(nil), nodeLevel...)
		seen := map[string]bool{}
		for _, summary := range providers {
			seen[summary.Name] = true
		}
		for _, name := range bound[node.Name] {
			if seen[name] {
				continue
			}
			seen[name] = true
			if summary, ok := known[name]; ok {
				providers = append(providers, summary)
			}
		}
		if providers == nil {
			providers = []providerSummary{}
		}
		entries = append(entries, nodeProviders{Name: node.Name, Providers: providers})
	}
	return entries, nil
}

// providersByNode maps a node to the provider names of the node-local volumes
// bound to it.
func (r *Readings) providersByNode(ctx context.Context) (map[string][]string, *controlplane.Error) {
	volumes, failure := r.kernel(ctx, "Volume")
	if failure != nil {
		return nil, failure
	}
	providers := map[string][]string{}
	for _, object := range volumes {
		var volume v1beta1.Volume
		if failure := decode(object, &volume); failure != nil {
			return nil, failure
		}
		if volume.Status.Node == "" || volume.Status.Provider.Name == "" {
			continue
		}
		providers[volume.Status.Node] = append(providers[volume.Status.Node], volume.Status.Provider.Name)
	}
	return providers, nil
}

// nodeConstraints is one node's scheduling constraints (C-36).
type nodeConstraints struct {
	Name        string   `json:"name"`
	Taints      []string `json:"taints,omitempty"`
	Schedulable bool     `json:"schedulable"`
}

// nodeConstraints reports each node's taints and schedulability (C-36).
func (r *Readings) nodeConstraints(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	entries := make([]nodeConstraints, 0, len(nodes))
	for _, node := range nodes {
		taints := make([]string, 0, len(node.Spec.Taints))
		for _, taint := range node.Spec.Taints {
			taints = append(taints, string(taint.Key)+"="+taint.Value+":"+string(taint.Effect))
		}
		entries = append(entries, nodeConstraints{Name: node.Name, Taints: taints, Schedulable: !node.Spec.Unschedulable})
	}
	return entries, nil
}

// settingValue reads one declared setting's current value through the settings
// domain (C-23).
func (r *Readings) settingValue(ctx context.Context, session identity.Session, node, id string) (any, *controlplane.Error) {
	items, failure := r.settingsItems(ctx, session, node)
	if failure != nil {
		return nil, failure
	}
	for _, item := range items {
		if item.ID == id {
			return item.Value, nil
		}
	}
	return "", nil
}

// reservedResources reads the node reservation setting per node: the panel
// reads one node at a time, so the item answers one value per node (C-36).
func (r *Readings) reservedResources(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	nodes, failure := r.nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	values := make(map[string]string, len(nodes))
	for _, node := range nodes {
		// Every node appears, so a node without a written reservation reads as
		// empty instead of disappearing from the panel.
		values[node.Name] = ""
		items, failure := r.settingsItems(ctx, session, node.Name)
		if failure != nil {
			return nil, failure
		}
		for _, item := range items {
			if item.ID == NodeReservedResources {
				values[node.Name] = item.Value
			}
		}
	}
	return values, nil
}

// settingsItems projects the settings for one node target.
func (r *Readings) settingsItems(ctx context.Context, session identity.Session, node string) ([]settings.Item, *controlplane.Error) {
	if r.Settings == nil {
		return nil, unreadable("设置域不可用")
	}
	return r.Settings.Items(ctx, session, node)
}

// account is one local account and its role (C-50).
type account struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// members reports the members and roles from the control-plane database (C-50).
func (r *Readings) members(ctx context.Context) (any, *controlplane.Error) {
	if r.Accounts == nil {
		return nil, unreadable("账号存储不可用")
	}
	users, err := r.Accounts.ListUsers(ctx)
	if err != nil {
		return nil, &controlplane.Error{
			Code:        controlplane.CodeDatabaseUnavailable,
			Message:     "控制面数据库暂不可用，成员与角色暂不可读",
			Stage:       controlplane.StageRead,
			Retryable:   true,
			SideEffects: controlplane.SideEffectsNone,
		}
	}
	members := make([]account, 0, len(users))
	for _, user := range users {
		members = append(members, account{Username: user.Username, Role: string(user.Role)})
	}
	return members, nil
}

// applicationAccess is one application's visibility as the user page reads it
// (C-50, C-59).
type applicationAccess struct {
	Name        string   `json:"name"`
	Namespace   string   `json:"namespace,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	Members     []string `json:"members,omitempty"`
	AllowGuests bool     `json:"allowGuests"`
}

// applicationVisibility reports every application's visibility from its spec
// (C-50, C-59).
func (r *Readings) applicationVisibility(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.kernel(ctx, "Application")
	if failure != nil {
		return nil, failure
	}
	entries := make([]applicationAccess, 0, len(objects))
	for _, object := range objects {
		var application v1beta1.Application
		if failure := decode(object, &application); failure != nil {
			return nil, failure
		}
		entries = append(entries, applicationAccess{
			Name:        application.Name,
			Namespace:   application.Namespace,
			Owner:       application.Spec.Owner,
			Members:     application.Spec.Visibility.Members,
			AllowGuests: application.Spec.Visibility.AllowGuests,
		})
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name < entries[right].Name })
	return entries, nil
}

// ownership is one object's owner as the user page reads it (C-50).
type ownership struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Owner string `json:"owner,omitempty"`
}

// owners reports application and volume ownership from their specs (C-50).
func (r *Readings) owners(ctx context.Context) (any, *controlplane.Error) {
	entries := []ownership{}
	for _, kind := range []string{"Application", "Volume"} {
		objects, failure := r.kernel(ctx, kind)
		if failure != nil {
			return nil, failure
		}
		for _, object := range objects {
			var entry ownership
			switch kind {
			case "Application":
				var application v1beta1.Application
				if failure := decode(object, &application); failure != nil {
					return nil, failure
				}
				entry = ownership{Name: application.Name, Kind: kind, Owner: application.Spec.Owner}
			case "Volume":
				var volume v1beta1.Volume
				if failure := decode(object, &volume); failure != nil {
					return nil, failure
				}
				entry = ownership{Name: volume.Name, Kind: kind, Owner: volume.Spec.Owner}
			}
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// nodes reads every node ordered by name.
func (r *Readings) nodes(ctx context.Context) ([]corev1.Node, *controlplane.Error) {
	objects, failure := r.list(ctx, schema.GroupVersionResource{Version: "v1", Resource: "nodes"})
	if failure != nil {
		return nil, failure
	}
	nodes := make([]corev1.Node, 0, len(objects))
	for _, object := range objects {
		var node corev1.Node
		if failure := decode(object, &node); failure != nil {
			return nil, failure
		}
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(left, right int) bool { return nodes[left].Name < nodes[right].Name })
	return nodes, nil
}

// kernel reads every object of one kernel kind.
func (r *Readings) kernel(ctx context.Context, kind string) ([]unstructured.Unstructured, *controlplane.Error) {
	resource, ok := api.ResourceName(kind)
	if !ok {
		return nil, unreadable(kind + " 不是内核对象类型")
	}
	return r.list(ctx, schema.GroupVersionResource{
		Group:    api.Group,
		Version:  v1beta1.GroupVersion.Version,
		Resource: resource,
	})
}

// list reads one resource; an empty namespace lists every namespace.
func (r *Readings) list(ctx context.Context, gvr schema.GroupVersionResource) ([]unstructured.Unstructured, *controlplane.Error) {
	if r.Dynamic == nil {
		return nil, clusterUnavailable()
	}
	list, err := r.Dynamic.Resource(gvr).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, clusterUnavailable()
	}
	return list.Items, nil
}

// decode converts one stored object into its typed shape.
func decode(object unstructured.Unstructured, target any) *controlplane.Error {
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, target); err != nil {
		return unreadable("对象 " + object.GetName() + " 无法解析")
	}
	return nil
}

// rolesOf reports the roles a node's labels declare.
func rolesOf(node corev1.Node) []string {
	const prefix = "node-role.kubernetes.io/"
	roles := make([]string, 0, 2)
	for label := range node.Labels {
		if role, ok := strings.CutPrefix(label, prefix); ok && role != "" {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	return roles
}

// isReady reports the node's Ready condition.
func isReady(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// allocatable reads one allocatable resource of a node.
func allocatable(node corev1.Node, name corev1.ResourceName) apiresource.Quantity {
	quantity := node.Status.Allocatable[name]
	return quantity
}

// summaryOf renders one registered provider.
func summaryOf(provider v1beta1.Provider) providerSummary {
	summary := providerSummary{
		Name:           provider.Name,
		Version:        provider.Spec.Version,
		Health:         string(provider.Status.Health.State),
		StorageClasses: provider.Spec.StorageClasses,
	}
	for _, capability := range provider.Spec.Capabilities {
		summary.Capabilities = append(summary.Capabilities, capability.Name+"@"+capability.Version)
	}
	return summary
}

// nodeScoped returns the capabilities of one provider that hold only on the
// nodes providing them (C-09).
func nodeScoped(provider v1beta1.Provider) []v1beta1.ProviderCapability {
	capabilities := make([]v1beta1.ProviderCapability, 0, len(provider.Spec.Capabilities))
	for _, capability := range provider.Spec.Capabilities {
		if capability.Scope == v1beta1.CapabilityScopeNode {
			capabilities = append(capabilities, capability)
		}
	}
	return capabilities
}

// now supplies the read clock.
func (r *Readings) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// unreadable reports an item whose source cannot be judged.
func unreadable(message string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeInternal,
		Message:     message,
		Stage:       controlplane.StageRead,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// clusterUnavailable reports a cluster read that failed (C-56).
func clusterUnavailable() *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeClusterUnavailable,
		Message:     "集群暂不可达，设置读取失败",
		Stage:       controlplane.StageRead,
		Retryable:   true,
		SideEffects: controlplane.SideEffectsNone,
	}
}
