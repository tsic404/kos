package kosupdater

import (
	"context"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/abi"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/pages/cluster"
)

// Readings resolves the items the component and update pages declare (C-53):
// providers, their verification results, system components and the application
// package declarations.
type Readings struct {
	// Dynamic reads the cluster.
	Dynamic dynamic.Interface
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
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
	case ItemCapabilities:
		return r.capabilities(ctx)
	case ItemGaps:
		return r.gaps(ctx, session)
	case ItemInstallable:
		return r.installable(ctx)
	case ItemBlockedProviders:
		return r.blockedProviders(ctx)
	case ItemExtensionState:
		return r.extensionState(ctx)
	case ItemVersions:
		return r.versions(ctx)
	case ItemApplicationUpgrades:
		return r.applicationUpgrades(ctx, session)
	case ItemIrreversible:
		return r.irreversible(ctx, session)
	case ItemCompatibility:
		return r.compatibility(ctx)
	default:
		return nil, unknownItem(item.ID)
	}
}

// reader is the cluster read seam for this call.
func (r *Readings) reader() *cluster.Reader {
	return &cluster.Reader{Dynamic: r.Dynamic, Now: r.Now}
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
	sort.Slice(providers, func(left, right int) bool {
		return providers[left].Name < providers[right].Name
	})
	return providers, nil
}

// definitions lists the application definitions.
func (r *Readings) definitions(ctx context.Context) ([]v1beta1.ApplicationDefinition, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "ApplicationDefinition", "")
	if failure != nil {
		return nil, failure
	}
	definitions := make([]v1beta1.ApplicationDefinition, 0, len(objects))
	for _, object := range objects {
		var definition v1beta1.ApplicationDefinition
		if failure := cluster.Decode(object, &definition); failure != nil {
			return nil, failure
		}
		definitions = append(definitions, definition)
	}
	sort.Slice(definitions, func(left, right int) bool {
		return definitions[left].Name < definitions[right].Name
	})
	return definitions, nil
}

// capabilityResult is one verified capability as the page shows it.
type capabilityResult struct {
	Provider   string   `json:"provider"`
	Capability string   `json:"capability"`
	Version    string   `json:"version"`
	Source     string   `json:"source,omitempty"`
	State      string   `json:"state"`
	Reason     string   `json:"reason,omitempty"`
	Failed     []string `json:"failedCases,omitempty"`
}

// capabilities reads the registered and verified capability set (C-09, C-10,
// C-51).
func (r *Readings) capabilities(ctx context.Context) (any, *controlplane.Error) {
	providers, failure := r.providers(ctx)
	if failure != nil {
		return nil, failure
	}
	results := make([]capabilityResult, 0)
	for _, provider := range providers {
		for _, capability := range provider.Status.Capabilities {
			results = append(results, capabilityResult{
				Provider: provider.Name, Capability: capability.Name, Version: capability.Version,
				Source: capability.Source, State: capability.State, Reason: capability.Reason,
				Failed: capability.FailedCases,
			})
		}
	}
	return map[string]any{"capabilities": results}, nil
}

// gapEntry names one object with the capability requirements it does not meet.
type gapEntry struct {
	Namespace string        `json:"namespace,omitempty"`
	Name      string        `json:"name"`
	Gaps      []cluster.Gap `json:"gaps,omitempty"`
	Invalid   string        `json:"invalid,omitempty"`
}

// gaps compares the declared requirements against the verified capability set
// (C-11, C-51).
func (r *Readings) gaps(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	definitions, failure := r.definitions(ctx)
	if failure != nil {
		return nil, failure
	}
	providers, failure := r.providers(ctx)
	if failure != nil {
		return nil, failure
	}
	definitionGaps := make([]gapEntry, 0, len(definitions))
	requirements := make(map[string][]v1beta1.CapabilityRequirement, len(definitions))
	for _, definition := range definitions {
		requirements[definition.Name] = definition.Spec.Requires
		entry, unmet := gapOf("", definition.Name, definition.Spec.Requires, providers)
		if unmet {
			definitionGaps = append(definitionGaps, entry)
		}
	}
	applicationGaps, failure := r.applicationGaps(ctx, session, requirements, providers)
	if failure != nil {
		return nil, failure
	}
	return map[string]any{
		"rule":         "按定义声明的能力需求与 Provider 有效能力集合比较，缺失与保证不足分开（C-11、C-53）",
		"definitions":  definitionGaps,
		"applications": applicationGaps,
	}, nil
}

// applicationGaps reports the installed applications whose definition requires
// a capability no provider serves.
func (r *Readings) applicationGaps(ctx context.Context, session identity.Session, requirements map[string][]v1beta1.CapabilityRequirement, providers []v1beta1.Provider) ([]gapEntry, *controlplane.Error) {
	applications, failure := r.applications(ctx, session)
	if failure != nil {
		return nil, failure
	}
	gaps := make([]gapEntry, 0)
	for _, application := range applications {
		required, known := requirements[application.Spec.DefinitionRef]
		if !known {
			continue
		}
		entry, unmet := gapOf(application.Namespace, application.Name, required, providers)
		if unmet {
			gaps = append(gaps, entry)
		}
	}
	sort.Slice(gaps, func(left, right int) bool {
		if gaps[left].Namespace != gaps[right].Namespace {
			return gaps[left].Namespace < gaps[right].Namespace
		}
		return gaps[left].Name < gaps[right].Name
	})
	return gaps, nil
}

// gapOf compares one object's requirements and reports whether it has a gap.
func gapOf(namespace, name string, requirements []v1beta1.CapabilityRequirement, providers []v1beta1.Provider) (gapEntry, bool) {
	result, err := cluster.Compare(requirements, providers)
	if err != nil {
		return gapEntry{Namespace: namespace, Name: name, Invalid: err.Error()}, true
	}
	gaps := cluster.Gaps(result)
	if len(gaps) == 0 {
		return gapEntry{}, false
	}
	return gapEntry{Namespace: namespace, Name: name, Gaps: gaps}, true
}

// installableEntry is one definition and whether its required capabilities are
// met (C-51).
type installableEntry struct {
	Name        string        `json:"name"`
	Version     string        `json:"version,omitempty"`
	Installable bool          `json:"installable"`
	Gaps        []cluster.Gap `json:"gaps,omitempty"`
	Invalid     string        `json:"invalid,omitempty"`
}

// installable lists the definitions whose required capabilities are served
// (C-51).
func (r *Readings) installable(ctx context.Context) (any, *controlplane.Error) {
	definitions, failure := r.definitions(ctx)
	if failure != nil {
		return nil, failure
	}
	providers, failure := r.providers(ctx)
	if failure != nil {
		return nil, failure
	}
	entries := make([]installableEntry, 0, len(definitions))
	for _, definition := range definitions {
		entry := installableEntry{Name: definition.Name, Version: definition.Spec.Package.Version}
		result, err := cluster.Compare(definition.Spec.Requires, providers)
		if err != nil {
			entry.Invalid = err.Error()
			entries = append(entries, entry)
			continue
		}
		entry.Installable = len(result.Missing) == 0
		entry.Gaps = cluster.Gaps(result)
		entries = append(entries, entry)
	}
	return map[string]any{"definitions": entries}, nil
}

// blockedProvider is one provider whose upgrade is blocked (C-48, C-51).
type blockedProvider struct {
	Name       string              `json:"name"`
	Version    string              `json:"version,omitempty"`
	Reason     string              `json:"reason,omitempty"`
	Message    string              `json:"message,omitempty"`
	Dependents []v1beta1.ObjectRef `json:"dependents,omitempty"`
	References []v1beta1.ObjectRef `json:"references,omitempty"`
}

// blockedProviders reads the providers whose UpgradeBlocked condition is true
// and the objects that keep them from advancing (C-48).
func (r *Readings) blockedProviders(ctx context.Context) (any, *controlplane.Error) {
	providers, failure := r.providers(ctx)
	if failure != nil {
		return nil, failure
	}
	blocked := make([]blockedProvider, 0)
	for _, provider := range providers {
		condition, found := conditionOf(provider.Status.Conditions, "UpgradeBlocked")
		if !found || condition.Status != metav1.ConditionTrue {
			continue
		}
		blocked = append(blocked, blockedProvider{
			Name: provider.Name, Version: provider.Spec.Version,
			Reason: condition.Reason, Message: condition.Message,
			Dependents: provider.Status.Dependents, References: provider.Status.References,
		})
	}
	return map[string]any{"providers": blocked}, nil
}

// conditionOf finds one condition by type.
func conditionOf(conditions []metav1.Condition, conditionType string) (metav1.Condition, bool) {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}

// componentState is one system component as the page shows it (C-28, C-51).
type componentState struct {
	Name       string               `json:"name"`
	Phase      string               `json:"phase,omitempty"`
	Health     string               `json:"health,omitempty"`
	Disabled   bool                 `json:"disabled"`
	Conditions []componentCondition `json:"conditions"`
}

// componentCondition is one extension condition with the dependency facts it
// carries.
type componentCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// extensionState reads the enablement state and the missing dependencies of
// every system component (C-28, C-51).
func (r *Readings) extensionState(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "SystemComponent", "")
	if failure != nil {
		return nil, failure
	}
	components := make([]componentState, 0, len(objects))
	for _, object := range objects {
		var component v1beta1.SystemComponent
		if failure := cluster.Decode(object, &component); failure != nil {
			return nil, failure
		}
		state := componentState{
			Name: component.Name, Phase: component.Status.Phase,
			Health: string(component.Status.Health.State), Disabled: component.Spec.Disable,
			Conditions: make([]componentCondition, 0, len(component.Status.Conditions)),
		}
		for _, condition := range component.Status.Conditions {
			if condition.Type != extension.ConditionFrameworkCompatible &&
				condition.Type != extension.ConditionDependenciesMet &&
				condition.Type != extension.ConditionContributionsLoaded &&
				condition.Type != extension.ConditionPackageVerified &&
				condition.Type != extension.ConditionDisableBlocked {
				continue
			}
			state.Conditions = append(state.Conditions, componentCondition{
				Type: condition.Type, Status: string(condition.Status),
				Reason: condition.Reason, Message: condition.Message,
			})
		}
		components = append(components, state)
	}
	sort.Slice(components, func(left, right int) bool {
		return components[left].Name < components[right].Name
	})
	return map[string]any{"components": components}, nil
}

// nodeVersion is one node's reported version.
type nodeVersion struct {
	Name           string `json:"name"`
	KubeletVersion string `json:"kubeletVersion,omitempty"`
	OSImage        string `json:"osImage,omitempty"`
}

// versions reports the KOS versions this control plane serves and the versions
// the cluster nodes run: the version discovery of C-51.
func (r *Readings) versions(ctx context.Context) (any, *controlplane.Error) {
	nodes, failure := r.reader().Nodes(ctx)
	if failure != nil {
		return nil, failure
	}
	clusterNodes := make([]nodeVersion, 0, len(nodes))
	for _, node := range nodes {
		clusterNodes = append(clusterNodes, nodeVersion{
			Name: node.Name, KubeletVersion: node.Status.NodeInfo.KubeletVersion,
			OSImage: node.Status.NodeInfo.OSImage,
		})
	}
	sort.Slice(clusterNodes, func(left, right int) bool {
		return clusterNodes[left].Name < clusterNodes[right].Name
	})
	return map[string]any{
		"kos": map[string]any{
			"apiVersions":       abi.Versions,
			"storageVersion":    abi.StorageVersion,
			"frameworkVersions": extension.SupportedFrameworks(),
		},
		"cluster": map[string]any{"nodes": clusterNodes},
	}, nil
}

// upgradeView is one application's upgrade preflight (C-34, C-51).
type upgradeView struct {
	Namespace    string                     `json:"namespace"`
	Name         string                     `json:"name"`
	Phase        string                     `json:"phase,omitempty"`
	Target       string                     `json:"target,omitempty"`
	RolledBack   bool                       `json:"rolledBack"`
	Irreversible []v1beta1.IrreversibleItem `json:"irreversible,omitempty"`
}

// applicationUpgrades reads the applications that carry an upgrade preflight
// (C-51).
func (r *Readings) applicationUpgrades(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	applications, failure := r.applications(ctx, session)
	if failure != nil {
		return nil, failure
	}
	upgrades := make([]upgradeView, 0)
	for _, application := range applications {
		view := upgradeView{
			Namespace: application.Namespace, Name: application.Name,
			Phase: application.Status.Phase,
		}
		if upgrade := application.Status.Upgrade; upgrade != nil {
			view.Target = upgrade.Target
			view.RolledBack = upgrade.RolledBack
			view.Irreversible = upgrade.Irreversible
		} else if application.Status.Phase != app.PhaseUpgrading {
			continue
		}
		upgrades = append(upgrades, view)
	}
	sort.Slice(upgrades, func(left, right int) bool {
		if upgrades[left].Namespace != upgrades[right].Namespace {
			return upgrades[left].Namespace < upgrades[right].Namespace
		}
		return upgrades[left].Name < upgrades[right].Name
	})
	return map[string]any{"applications": upgrades}, nil
}

// irreversibleWarning is one upgrade the package declaration makes irreversible
// (C-34, C-51).
type irreversibleWarning struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Object    string `json:"object,omitempty"`
	Detail    string `json:"detail"`
}

// irreversible reads the irreversible-change warnings from the package
// declarations and the recorded preflights (C-34, C-51).
func (r *Readings) irreversible(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	applications, failure := r.applications(ctx, session)
	if failure != nil {
		return nil, failure
	}
	definitions, failure := r.definitions(ctx)
	if failure != nil {
		return nil, failure
	}
	offered := make(map[string]v1beta1.ApplicationDefinition, len(definitions))
	for _, definition := range definitions {
		offered[definition.Name] = definition
	}
	warnings := make([]irreversibleWarning, 0)
	for _, application := range applications {
		definition, known := offered[application.Spec.DefinitionRef]
		if known && application.Status.Applied != nil {
			applied := application.Status.Applied.DataFormat
			target := definition.Spec.Package.DataFormat
			if applied != "" && target != "" && applied != target {
				warnings = append(warnings, irreversibleWarning{
					Namespace: application.Namespace, Name: application.Name,
					Kind:   "dataFormat",
					Detail: "包声明的数据格式由 " + applied + " 变为 " + target + "，升级不可逆",
				})
			}
		}
		if application.Status.Upgrade == nil {
			continue
		}
		for _, item := range application.Status.Upgrade.Irreversible {
			warnings = append(warnings, irreversibleWarning{
				Namespace: application.Namespace, Name: application.Name,
				Kind: string(item.Kind), Object: refText(item.Object), Detail: item.Detail,
			})
		}
	}
	return map[string]any{"warnings": warnings}, nil
}

// refText renders the object an irreversible item names.
func refText(ref *v1beta1.ObjectRef) string {
	if ref == nil {
		return ""
	}
	if ref.Namespace == "" {
		return ref.Kind + "/" + ref.Name
	}
	return ref.Kind + "/" + ref.Namespace + "/" + ref.Name
}

// compatibilityView is one component's framework compatibility (C-28, C-51).
type compatibilityView struct {
	Name                string `json:"name"`
	FrameworkCompatible string `json:"frameworkCompatible"`
	PackageVerified     string `json:"packageVerified"`
	Reason              string `json:"reason,omitempty"`
	Message             string `json:"message,omitempty"`
}

// compatibility reports the framework version set this control plane serves and
// the compatibility each installed component reports (C-28, C-51).
func (r *Readings) compatibility(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "SystemComponent", "")
	if failure != nil {
		return nil, failure
	}
	components := make([]compatibilityView, 0, len(objects))
	for _, object := range objects {
		var component v1beta1.SystemComponent
		if failure := cluster.Decode(object, &component); failure != nil {
			return nil, failure
		}
		view := compatibilityView{Name: component.Name}
		if condition, found := conditionOf(component.Status.Conditions, extension.ConditionFrameworkCompatible); found {
			view.FrameworkCompatible = string(condition.Status)
			view.Reason = condition.Reason
			view.Message = condition.Message
		}
		if condition, found := conditionOf(component.Status.Conditions, extension.ConditionPackageVerified); found {
			view.PackageVerified = string(condition.Status)
		}
		components = append(components, view)
	}
	sort.Slice(components, func(left, right int) bool {
		return components[left].Name < components[right].Name
	})
	return map[string]any{
		"supportedFrameworks": extension.SupportedFrameworks(),
		"components":          components,
	}, nil
}

// applications lists the applications the session may see, ordered by namespace
// and name (C-59).
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

// unknownItem marks an item this product does not declare.
func unknownItem(id string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeCapabilityMissing,
		Message:     "kos-updater 没有声明页面条目 " + id,
		Stage:       controlplane.StageCapability,
		SideEffects: controlplane.SideEffectsNone,
	}
}
