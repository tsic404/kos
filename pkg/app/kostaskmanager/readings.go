package kostaskmanager

import (
	"context"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/pages/cluster"
)

// eventLimit bounds the overview's recent events; older events are not readable
// outside the window anyway (C-64).
const eventLimit = 50

// UsageSource reports the sampled CPU and memory consumption of the
// applications a session may see (C-49). A distribution with no metric source
// leaves it nil and the item states that instead of inventing numbers.
type UsageSource interface {
	// Usage returns one sample per application.
	Usage(ctx context.Context, session identity.Session) ([]ApplicationUsage, error)
}

// ApplicationUsage is one application's sampled consumption.
type ApplicationUsage struct {
	// Namespace is the application's namespace.
	Namespace string `json:"namespace"`
	// Name is the application name.
	Name string `json:"name"`
	// CPU is the sampled CPU usage as a quantity.
	CPU string `json:"cpu"`
	// Memory is the sampled memory usage as a quantity.
	Memory string `json:"memory"`
}

// Readings resolves the items the product's pages declare (C-53): every read
// goes through the control plane's cluster client and is trimmed to the
// session's visibility (C-59, C-64).
type Readings struct {
	// Dynamic reads the cluster.
	Dynamic dynamic.Interface
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
	// Usage reports resource samples; nil leaves the item stating no source is
	// configured.
	Usage UsageSource
}

// Read implements pages.Readings.
func (r *Readings) Read(ctx context.Context, session identity.Session, item extension.PageItem) (pages.Reading, *controlplane.Error) {
	value, failure := r.value(ctx, session, item)
	if failure != nil {
		return pages.Reading{}, failure
	}
	return pages.Reading{Value: value, ObservedAt: r.reader().Clock()}, nil
}

// value resolves one item from the source it declares.
func (r *Readings) value(ctx context.Context, session identity.Session, item extension.PageItem) (any, *controlplane.Error) {
	switch item.ID {
	case ItemNodesReady:
		return r.nodesReady(ctx)
	case ItemAbnormalObjects:
		return r.abnormalObjects(ctx, session)
	case ItemPendingUpgrades:
		return r.pendingUpgrades(ctx, session)
	case ItemRecentEvents:
		return r.recentEvents(ctx, session)
	case ItemMaintenanceNodes:
		return r.maintenanceNodes(ctx)
	case ItemMaintenanceAffected:
		return r.maintenanceAffected(ctx, session)
	case ItemInstalled:
		return r.installed(ctx, session)
	case ItemInstallable:
		return r.installable(ctx)
	case ItemResourceUsage:
		return r.resourceUsage(ctx, session)
	case ItemStop, ItemStart, ItemRestart, ItemScale, ItemResourceSpec, ItemUpgrade, ItemUninstall:
		return r.entryTargets(ctx, session, item.ID)
	default:
		return nil, unknownItem(item.ID)
	}
}

// reader is the cluster read seam for this call.
func (r *Readings) reader() *cluster.Reader {
	return &cluster.Reader{Dynamic: r.Dynamic, Now: r.Now}
}

// nodesReady reads the nodes and their Ready condition (C-49).
func (r *Readings) nodesReady(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.reader().Nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	ready := 0
	views := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		isReady := nodeReady(node)
		if isReady {
			ready++
		}
		views = append(views, map[string]any{"name": node.Name, "ready": isReady})
	}
	sort.Slice(views, func(left, right int) bool {
		return views[left]["name"].(string) < views[right]["name"].(string)
	})
	return map[string]any{"ready": ready, "total": len(nodes), "nodes": views}, nil
}

// nodeReady reports whether a node carries the Ready condition (C-49).
func nodeReady(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// statused is the shared status shape every kernel kind carries.
type statused struct {
	Status v1beta1.ObjectStatus `json:"status"`
}

// abnormal is C-49's computed rule: an object is abnormal when its health is not
// Healthy or when one of its conditions is not met.
func abnormal(status v1beta1.ObjectStatus) bool {
	if status.Health.State != "" && status.Health.State != v1beta1.HealthStateHealthy {
		return true
	}
	for _, condition := range status.Conditions {
		if condition.Status != metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// abnormalObjects lists the objects C-49 counts as abnormal, trimmed to what the
// session may see.
func (r *Readings) abnormalObjects(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	objects := make([]map[string]any, 0)
	for _, kind := range cluster.StatusKinds() {
		stored, failure := r.reader().List(ctx, kind, "")
		if failure != nil {
			return nil, failure
		}
		for _, object := range stored {
			var shape statused
			if failure := cluster.Decode(object, &shape); failure != nil {
				return nil, failure
			}
			if !abnormal(shape.Status) {
				continue
			}
			visible, failure := r.reader().Visible(ctx, session, kind, object)
			if failure != nil {
				return nil, failure
			}
			if !visible {
				continue
			}
			objects = append(objects, map[string]any{
				"kind":       kind,
				"namespace":  cluster.Text(object, "metadata", "namespace"),
				"name":       cluster.Text(object, "metadata", "name"),
				"phase":      shape.Status.Phase,
				"health":     string(shape.Status.Health.State),
				"conditions": conditionsOf(shape.Status.Conditions),
			})
		}
	}
	sort.Slice(objects, func(left, right int) bool {
		leftKey := objectKey(objects[left])
		rightKey := objectKey(objects[right])
		return leftKey < rightKey
	})
	return map[string]any{
		"rule":    "健康状态不是 Healthy，或存在未满足的条件（C-49、C-53）",
		"objects": objects,
	}, nil
}

// objectKey orders abnormal objects by kind, namespace and name.
func objectKey(object map[string]any) string {
	return object["kind"].(string) + "/" + object["namespace"].(string) + "/" + object["name"].(string)
}

// conditionsOf renders the conditions a page shows.
func conditionsOf(conditions []metav1.Condition) []map[string]any {
	views := make([]map[string]any, 0, len(conditions))
	for _, condition := range conditions {
		views = append(views, map[string]any{
			"type":    condition.Type,
			"status":  string(condition.Status),
			"reason":  condition.Reason,
			"message": condition.Message,
		})
	}
	return views
}

// pendingUpgrade is one application the version comparison finds behind its
// definition, or one the reconciler is already upgrading.
type pendingUpgrade struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Installed string `json:"installed,omitempty"`
	Target    string `json:"target,omitempty"`
	Version   string `json:"version,omitempty"`
}

// pendingUpgrades compares the package a definition offers with the baseline the
// application applied, which is the version discovery of C-49.
func (r *Readings) pendingUpgrades(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	applications, failure := r.applications(ctx, session)
	if failure != nil {
		return nil, failure
	}
	definitions, failure := r.definitions(ctx)
	if failure != nil {
		return nil, failure
	}
	pending := make([]pendingUpgrade, 0)
	for _, application := range applications {
		entry := pendingUpgrade{Namespace: application.Namespace, Name: application.Name}
		definition, known := definitions[application.Spec.DefinitionRef]
		switch {
		case application.Status.Phase == app.PhaseUpgrading:
			entry.State = "in-progress"
			if application.Status.Upgrade != nil {
				entry.Target = application.Status.Upgrade.Target
			}
		case known && applicationBehindDefinition(application, definition):
			entry.State = "definition-changed"
			entry.Installed = application.Status.Applied.DefinitionDigest
			entry.Target = definition.Spec.Package.Digest
			entry.Version = definition.Spec.Package.Version
		case application.Status.Upgrade != nil && application.Status.Upgrade.Target != "" &&
			!application.Status.Upgrade.RolledBack && applicationBehind(application):
			entry.State = "requested"
			entry.Target = application.Status.Upgrade.Target
		default:
			continue
		}
		pending = append(pending, entry)
	}
	return map[string]any{
		"rule":    "定义的包摘要与已应用基线不同，或应用正在升级（C-49、C-53）",
		"pending": pending,
	}, nil
}

// applicationBehindDefinition compares the offered package digest with the
// baseline the application applied (C-34).
func applicationBehindDefinition(application v1beta1.Application, definition v1beta1.ApplicationDefinition) bool {
	if definition.Spec.Package.Digest == "" || application.Status.Applied == nil {
		return false
	}
	applied := application.Status.Applied.DefinitionDigest
	return applied != "" && applied != definition.Spec.Package.Digest
}

// applicationBehind reports a preflight target the applied baseline has not
// reached, so a settled upgrade is not reported as pending.
func applicationBehind(application v1beta1.Application) bool {
	if application.Status.Applied == nil {
		return true
	}
	applied := application.Status.Applied.DefinitionDigest
	return applied == "" || applied != application.Status.Upgrade.Target
}

// definitions indexes the application definitions by name.
func (r *Readings) definitions(ctx context.Context) (map[string]v1beta1.ApplicationDefinition, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "ApplicationDefinition", "")
	if failure != nil {
		return nil, failure
	}
	definitions := make(map[string]v1beta1.ApplicationDefinition, len(objects))
	for _, object := range objects {
		var definition v1beta1.ApplicationDefinition
		if failure := cluster.Decode(object, &definition); failure != nil {
			return nil, failure
		}
		definitions[definition.Name] = definition
	}
	return definitions, nil
}

// recentEvents reads C-64's window, newest first, trimmed to what the session
// may see.
func (r *Readings) recentEvents(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	events, failure := r.reader().Events(ctx, session, "", eventLimit)
	if failure != nil {
		return nil, failure
	}
	return map[string]any{"window": controlplane.EventWindow.String(), "events": events}, nil
}

// maintenanceNodes lists the nodes KOS has taken out of scheduling (C-65).
func (r *Readings) maintenanceNodes(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.reader().Nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	views := make([]map[string]any, 0)
	for _, node := range nodes {
		if !node.Spec.Unschedulable {
			continue
		}
		views = append(views, map[string]any{"name": node.Name, "ready": nodeReady(node)})
	}
	return map[string]any{"nodes": views}, nil
}

// maintenanceAffected lists, per unschedulable node, the applications whose
// node-local volumes are bound to it (C-65).
func (r *Readings) maintenanceAffected(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	nodes, failure := r.reader().Nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	unschedulable := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		if node.Spec.Unschedulable {
			unschedulable[node.Name] = true
		}
	}
	if len(unschedulable) == 0 {
		return map[string]any{"nodes": []any{}}, nil
	}
	bound, failure := r.nodeLocalVolumes(ctx)
	if failure != nil {
		return nil, failure
	}
	applications, failure := r.applications(ctx, session)
	if failure != nil {
		return nil, failure
	}
	affected := make(map[string][]string, len(unschedulable))
	for _, application := range applications {
		namespace := application.Namespace
		if application.Spec.Namespace != "" {
			namespace = application.Spec.Namespace
		}
		for _, volume := range application.Spec.Volumes {
			node, known := bound[namespace+"/"+volume]
			if !known || !unschedulable[node] {
				continue
			}
			affected[node] = append(affected[node], application.Namespace+"/"+application.Name)
		}
	}
	views := make([]map[string]any, 0, len(affected))
	names := make([]string, 0, len(affected))
	for name := range affected {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sort.Strings(affected[name])
		views = append(views, map[string]any{"node": name, "applications": affected[name]})
	}
	return map[string]any{"nodes": views}, nil
}

// nodeLocalVolumes maps "namespace/name" of a node-local volume to its binding
// node (C-38).
func (r *Readings) nodeLocalVolumes(ctx context.Context) (map[string]string, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Volume", "")
	if failure != nil {
		return nil, failure
	}
	bound := make(map[string]string, len(objects))
	for _, object := range objects {
		var volume v1beta1.Volume
		if failure := cluster.Decode(object, &volume); failure != nil {
			return nil, failure
		}
		if volume.Spec.Mobility != v1beta1.VolumeMobilityNodeLocal || volume.Status.Node == "" {
			continue
		}
		bound[volume.Namespace+"/"+volume.Name] = volume.Status.Node
	}
	return bound, nil
}

// applications lists the applications the session may see, ordered by
// namespace and name.
func (r *Readings) applications(ctx context.Context, session identity.Session) ([]v1beta1.Application, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Application", "")
	if failure != nil {
		return nil, failure
	}
	applications := make([]v1beta1.Application, 0, len(objects))
	for _, object := range objects {
		var application v1beta1.Application
		if failure := cluster.Decode(object, &application); failure != nil {
			return nil, failure
		}
		visible, failure := r.reader().Visible(ctx, session, "Application", object)
		if failure != nil {
			return nil, failure
		}
		if !visible {
			continue
		}
		applications = append(applications, application)
	}
	sort.Slice(applications, func(left, right int) bool {
		if applications[left].Namespace != applications[right].Namespace {
			return applications[left].Namespace < applications[right].Namespace
		}
		return applications[left].Name < applications[right].Name
	})
	return applications, nil
}

// installed renders the installed applications with their health and resource
// projection (C-49).
func (r *Readings) installed(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	applications, failure := r.applications(ctx, session)
	if failure != nil {
		return nil, failure
	}
	views := make([]map[string]any, 0, len(applications))
	for _, application := range applications {
		views = append(views, map[string]any{
			"namespace": application.Namespace,
			"name":      application.Name,
			"phase":     application.Status.Phase,
			"health":    string(application.Status.Health.State),
			"owner":     application.Spec.Owner,
			"paused":    application.Spec.Paused,
			"replicas":  application.Spec.Replicas,
			"resources": application.Status.Resources,
		})
	}
	return map[string]any{"applications": views}, nil
}

// installable lists the definitions whose required capabilities the installed
// providers serve (C-11, C-49, C-53).
func (r *Readings) installable(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "ApplicationDefinition", "")
	if failure != nil {
		return nil, failure
	}
	providers, failure := r.providers(ctx)
	if failure != nil {
		return nil, failure
	}
	entries := make([]map[string]any, 0, len(objects))
	for _, object := range objects {
		var definition v1beta1.ApplicationDefinition
		if failure := cluster.Decode(object, &definition); failure != nil {
			return nil, failure
		}
		entry := map[string]any{
			"name": definition.Name, "version": definition.Spec.Package.Version,
		}
		result, err := cluster.Compare(definition.Spec.Requires, providers)
		if err != nil {
			entry["installable"] = false
			entry["invalid"] = err.Error()
		} else {
			entry["installable"] = len(result.Missing) == 0
			entry["gaps"] = cluster.Gaps(result)
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left]["name"].(string) < entries[right]["name"].(string)
	})
	return map[string]any{
		"rule":        "定义存在且必需能力需求已被已验证的 Provider 满足（C-11、C-53）",
		"definitions": entries,
	}, nil
}

// providers lists the installed providers.
func (r *Readings) providers(ctx context.Context) ([]v1beta1.Provider, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Provider", "")
	if failure != nil {
		return nil, failure
	}
	providers := make([]v1beta1.Provider, 0, len(objects))
	for _, object := range objects {
		var provider v1beta1.Provider
		if failure := cluster.Decode(object, &provider); failure != nil {
			return nil, failure
		}
		providers = append(providers, provider)
	}
	return providers, nil
}

// resourceUsage reports the sampled consumption of C-49's metric source.
func (r *Readings) resourceUsage(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	if r.Usage == nil {
		return map[string]any{
			"available": false,
			"reason":    "发行版没有接入资源指标来源，KOS 不采集工作负载用量",
		}, nil
	}
	samples, err := r.Usage.Usage(ctx, session)
	if err != nil {
		return map[string]any{"available": false, "reason": "资源指标读取失败：" + err.Error()}, nil
	}
	return map[string]any{"available": true, "applications": samples}, nil
}

// entryTargets names the applications one lifecycle entry applies to, using the
// same gates the control plane applies (C-66).
func (r *Readings) entryTargets(ctx context.Context, session identity.Session, item string) (any, *controlplane.Error) {
	applications, failure := r.applications(ctx, session)
	if failure != nil {
		return nil, failure
	}
	targets := make([]map[string]string, 0, len(applications))
	for _, application := range applications {
		if !applies(item, application) {
			continue
		}
		targets = append(targets, map[string]string{"namespace": application.Namespace, "name": application.Name})
	}
	return map[string]any{"applications": targets}, nil
}

// applies reports whether one lifecycle entry can run against an application
// (C-66). The control plane remains the judge; this only names the entries the
// page shows as applicable.
func applies(item string, application v1beta1.Application) bool {
	switch item {
	case ItemStop:
		return !application.Spec.Paused
	case ItemStart:
		return application.Spec.Paused
	case ItemRestart:
		return application.Status.Phase == app.PhaseReady || application.Status.Phase == app.PhaseUpgrading
	case ItemUpgrade:
		return application.Spec.DefinitionRef != ""
	default:
		return true
	}
}

// unknownItem marks an item this product does not declare.
func unknownItem(id string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeCapabilityMissing,
		Message:     "kos-taskmanager 没有声明页面条目 " + id,
		Stage:       controlplane.StageCapability,
		SideEffects: controlplane.SideEffectsNone,
	}
}
