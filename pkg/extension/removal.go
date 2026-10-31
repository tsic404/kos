package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// Residual manifest carriers and dispositions (C-29). The manifest is one
// immutable ConfigMap in the KOS namespace, named after the component it
// records.
const (
	// ResidualRetained marks data a declaration kept on uninstall.
	ResidualRetained = "retained"
	// ResidualReleased marks an external resource whose reference was released
	// without deleting it.
	ResidualReleased = "released"
	// ResidualLabel binds the manifest to the component UID.
	ResidualLabel = "kos.tsic.top/extension-residual"
	// ResidualKey is the manifest field carrying the entries.
	ResidualKey    = "residual"
	residualPrefix = "kos-extension-residual-"
)

// Residual is one entry of an extension uninstall manifest (C-29).
type Residual struct {
	// Object is the retained or released object.
	Object v1beta1.ObjectRef `json:"object"`
	// Disposition is retained or released.
	Disposition string `json:"disposition"`
}

// ResidualName is the manifest name of one uninstalled component.
func ResidualName(uid types.UID) string {
	return residualPrefix + string(uid)
}

// EncodeResiduals renders a manifest into its ConfigMap value.
func EncodeResiduals(entries []Residual) (string, error) {
	if entries == nil {
		entries = []Residual{}
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("encode the residual manifest: %w", err)
	}
	return string(encoded), nil
}

// DecodeResiduals reads a manifest from its ConfigMap value.
func DecodeResiduals(text string) ([]Residual, error) {
	if text == "" {
		return nil, nil
	}
	var entries []Residual
	if err := json.Unmarshal([]byte(text), &entries); err != nil {
		return nil, fmt.Errorf("decode the residual manifest: %w", err)
	}
	return entries, nil
}

// retentionFor returns the declared fate of one object class. An entry matches
// by class key, kind or resource; the class-wide entry covers every class. An
// undeclared class is deleted, which is C-29's default.
func retentionFor(retention []Retention, class ObjectClass) (keep bool, external string, declared bool) {
	key := class.GroupKind().String()
	for _, entry := range retention {
		if entry.Resource != ClassObjectClasses && entry.Resource != key &&
			entry.Resource != class.Kind && entry.Resource != class.Resource {
			continue
		}
		return entry.Keep, entry.ExternalReference, true
	}
	return false, "", false
}

// applyRetention settles the data of one uninstalling component: kept classes
// stay and are listed, external references are only released, and undeclared
// classes are deleted (C-29). The returned entries are what the residual
// manifest records.
func applyRetention(ctx context.Context, client dynamic.Interface, writer *write.Writer, classes []ObjectClass, retention []Retention) ([]Residual, error) {
	residuals := make([]Residual, 0)
	for _, class := range classes {
		objects, err := listClassObjects(ctx, client, class)
		if err != nil {
			return nil, err
		}
		keep, external, declared := retentionFor(retention, class)
		switch {
		case external != "":
			// An external resource is never deleted; its reference is released
			// and the release is recorded.
			for _, object := range objects {
				residuals = append(residuals, Residual{Object: objectRefOf(object, class), Disposition: ResidualReleased})
			}
		case declared && keep:
			for _, object := range objects {
				residuals = append(residuals, Residual{Object: objectRefOf(object, class), Disposition: ResidualRetained})
			}
		default:
			for _, object := range objects {
				// C-02: an object without an attribution marker is not KOS's
				// to delete. It is left in place rather than failing the
				// uninstall.
				if attribution, err := markers.AttributionOf(object.GetLabels()); err != nil || attribution == markers.AttributionUnmanaged {
					continue
				}
				if err := deleteClassName(ctx, writer, class, object); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, entry := range retention {
		if entry.ExternalReference == "" || coversClass(entry, classes) {
			continue
		}
		residuals = append(residuals, Residual{
			Object:      v1beta1.ObjectRef{Kind: entry.Resource, Name: entry.ExternalReference},
			Disposition: ResidualReleased,
		})
	}
	sort.SliceStable(residuals, func(left, right int) bool {
		leftKey := objectRefKey(residuals[left].Object) + "/" + residuals[left].Disposition
		rightKey := objectRefKey(residuals[right].Object) + "/" + residuals[right].Disposition
		return leftKey < rightKey
	})
	return residuals, nil
}

// coversClass reports whether a retention entry names one of the classes.
func coversClass(entry Retention, classes []ObjectClass) bool {
	for _, class := range classes {
		if entry.Resource == ClassObjectClasses || entry.Resource == class.GroupKind().String() ||
			entry.Resource == class.Kind || entry.Resource == class.Resource {
			return true
		}
	}
	return false
}

// deleteClassName deletes one instance as the extension's own controller,
// scoped to the class's API group (C-20, C-29). The controller is the writer
// C-03 allocates for extension objects.
func deleteClassName(ctx context.Context, writer *write.Writer, class ObjectClass, object *unstructured.Unstructured) error {
	scoped := *writer
	scoped.Scope = write.Scope{Groups: map[string]bool{class.Group: true}}
	resources := make(map[schema.GroupKind]string, len(writer.Resources)+1)
	for key, plural := range writer.Resources {
		resources[key] = plural
	}
	resources[class.GroupKind().Schema()] = class.Resource
	scoped.Resources = resources
	err := scoped.Delete(ctx, write.SubjectExtensionReconciler, write.Session{}, write.RefOf(object))
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s %s/%s: %w", class.Kind, object.GetNamespace(), object.GetName(), err)
	}
	return nil
}

// writeResiduals records the uninstall manifest once. A second write is a
// no-op: the manifest is immutable.
func writeResiduals(ctx context.Context, writer *write.Writer, uid types.UID, entries []Residual) error {
	if len(entries) == 0 {
		return nil
	}
	encoded, err := EncodeResiduals(entries)
	if err != nil {
		return err
	}
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"immutable":  true,
		"metadata": map[string]any{
			"name":      ResidualName(uid),
			"namespace": api.SystemNamespace,
			"labels": map[string]any{
				markers.ManagedBy: write.ManagedByValue,
				ResidualLabel:     string(uid),
			},
		},
		"data": map[string]any{ResidualKey: encoded},
	}}
	if _, err := writer.Create(ctx, write.SubjectInstaller, write.Session{}, document); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("write residual manifest: %w", err)
	}
	return nil
}

// clearRegistration removes the capability registration a component no longer
// serves (C-28, C-29). The Provider object stays; only the spec fields the
// component-registry manager wrote are cleared. A Provider this component does
// not own is left alone (C-02).
func clearRegistration(ctx context.Context, client dynamic.Interface, resources map[schema.GroupKind]string, writer *write.Writer, name string, uid types.UID) error {
	plural, ok := resources[schema.GroupKind{Group: api.Group, Kind: "Provider"}]
	if !ok || client == nil {
		return nil
	}
	resource := client.Resource(schema.GroupVersionResource{
		Group: api.Group, Version: v1beta1.GroupVersion.Version, Resource: plural,
	})
	current, err := resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read provider %s: %w", name, err)
	}
	owner := current.GetLabels()[markers.OwnerUID]
	componentRef, _, err := unstructured.NestedString(current.Object, "spec", "componentRef")
	if err != nil {
		return fmt.Errorf("provider %s: read spec.componentRef: %w", name, err)
	}
	if owner != string(uid) && componentRef != name {
		return nil
	}
	spec, _, err := unstructured.NestedMap(current.Object, "spec")
	if err != nil {
		return fmt.Errorf("provider %s: read spec: %w", name, err)
	}
	if len(spec) == 0 {
		return nil
	}
	labels := map[string]any{}
	for _, key := range []string{markers.ManagedBy, markers.OwnerUID} {
		if value := current.GetLabels()[key]; value != "" {
			labels[key] = value
		}
	}
	metadata := map[string]any{"name": name}
	if len(labels) > 0 {
		metadata["labels"] = labels
	}
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "Provider",
		"metadata":   metadata,
		// The empty componentRef keeps the spec an object while the apply
		// removes the capability fields this manager owned.
		"spec": map[string]any{"componentRef": ""},
	}}
	if _, err := writer.Apply(ctx, write.SubjectComponentRegistry, write.Session{}, write.PartSpec, document); err != nil {
		return fmt.Errorf("clear provider %s registration: %w", name, err)
	}
	return nil
}

// componentDependents lists the live components whose dependency edges name
// the target component (C-28, C-29).
func componentDependents(targetName string, declarations map[string]Loaded) []write.Ref {
	target, ok := declarations[targetName]
	if !ok {
		return nil
	}
	var dependents []write.Ref
	for name, declaration := range declarations {
		if name == targetName {
			continue
		}
		if !declarationDependsOn(target, declaration, declarations, targetName) {
			continue
		}
		dependents = append(dependents, componentRef(name))
	}
	sortRefs(dependents)
	return dependents
}

// declarationDependsOn reports whether one declaration has an edge the target
// meets. A capability edge is covered when another live component serves it.
func declarationDependsOn(target, other Loaded, declarations map[string]Loaded, targetName string) bool {
	for _, requirement := range other.Dependencies.Components {
		if requirement.Identifier != target.Identifier {
			continue
		}
		constraint, err := ParseConstraint(requirement.Versions)
		if err == nil && constraint.Allows(target.Version) {
			return true
		}
	}
	for _, requirement := range other.Dependencies.Capabilities {
		if !contributesRequirement(target, requirement) {
			continue
		}
		if !capabilityServedElsewhere(requirement, declarations, targetName) {
			return true
		}
	}
	return false
}

// contributesRequirement reports whether a declaration serves a requirement.
func contributesRequirement(declaration Loaded, requirement CapabilityRequirement) bool {
	constraint, err := ParseConstraint(requirement.Versions)
	if err != nil {
		return false
	}
	for _, capability := range declaration.Contributions.Capabilities.Items {
		if capability.Name == requirement.Name && constraint.Allows(capability.Version) {
			return true
		}
	}
	return false
}

// capabilityServedElsewhere reports whether another live component serves one
// capability requirement.
func capabilityServedElsewhere(requirement CapabilityRequirement, declarations map[string]Loaded, excluded string) bool {
	for name, declaration := range declarations {
		if name == excluded {
			continue
		}
		if contributesRequirement(declaration, requirement) {
			return true
		}
	}
	return false
}

// componentRef renders one system component as a dependent reference.
func componentRef(name string) write.Ref {
	return write.Ref{
		Group:   api.Group,
		Version: v1beta1.GroupVersion.Version,
		Kind:    "SystemComponent",
		Name:    name,
	}
}

// objectDependents lists the extension objects of other components that
// reference a class the target declares (C-29). The kernel follows only the
// reference paths each class declares (C-19).
func objectDependents(ctx context.Context, client dynamic.Interface, targetName string, declarations map[string]Loaded) ([]write.Ref, error) {
	target, ok := declarations[targetName]
	if !ok || len(target.Contributions.ObjectClasses) == 0 {
		return nil, nil
	}
	referenced := make(map[schema.GroupKind]bool, len(target.Contributions.ObjectClasses))
	for _, class := range target.Contributions.ObjectClasses {
		referenced[class.GroupKind().Schema()] = true
	}
	var dependents []write.Ref
	for name, declaration := range declarations {
		if name == targetName {
			continue
		}
		for _, class := range declaration.Contributions.ObjectClasses {
			if len(class.References) == 0 {
				continue
			}
			objects, err := listClassObjects(ctx, client, class)
			if err != nil {
				return nil, err
			}
			for _, object := range objects {
				if !referencesAnyClass(object, class.References, referenced) {
					continue
				}
				dependents = append(dependents, write.RefOf(object))
			}
		}
	}
	sortRefs(dependents)
	return dependents, nil
}

// referencesAnyClass reports whether an object references any class of the
// target set at a declared path.
func referencesAnyClass(object *unstructured.Unstructured, paths []string, classes map[schema.GroupKind]bool) bool {
	for _, path := range paths {
		value, found, err := unstructured.NestedFieldNoCopy(object.Object, strings.Split(path, ".")...)
		if err != nil || !found {
			continue
		}
		for _, candidate := range decodeReferences(value) {
			if classes[schema.GroupKind{Group: candidate.Group, Kind: candidate.Kind}] {
				return true
			}
		}
	}
	return false
}

// applicationAndProviderDependents lists the applications and Providers that
// require a capability the target contributes (C-29).
func applicationAndProviderDependents(ctx context.Context, client dynamic.Interface, resources map[schema.GroupKind]string, targetName string, target Loaded) ([]write.Ref, error) {
	capabilities := target.Contributions.Capabilities.Items
	if len(capabilities) == 0 {
		return nil, nil
	}
	definitions, err := listKernelObjects(ctx, client, resources, "ApplicationDefinition")
	if err != nil {
		return nil, err
	}
	var dependents []write.Ref
	referencingDefinitions := make(map[string]bool)
	for _, definition := range definitions {
		requirements, err := requirementList(definition)
		if err != nil {
			return nil, err
		}
		if !requirementsMatch(requirements, capabilities) {
			continue
		}
		referencingDefinitions[definition.GetName()] = true
	}
	if len(referencingDefinitions) > 0 {
		applications, err := listKernelObjects(ctx, client, resources, "Application")
		if err != nil {
			return nil, err
		}
		for _, application := range applications {
			reference, _, err := unstructured.NestedString(application.Object, "spec", "definitionRef")
			if err != nil || !referencingDefinitions[reference] {
				continue
			}
			dependents = append(dependents, write.RefOf(application))
		}
	}
	providers, err := listKernelObjects(ctx, client, resources, "Provider")
	if err != nil {
		return nil, err
	}
	for _, provider := range providers {
		declaredBy, _, err := unstructured.NestedString(provider.Object, "spec", "componentRef")
		if err != nil || declaredBy == targetName {
			continue
		}
		if providerCapabilitiesMatch(provider, capabilities) {
			dependents = append(dependents, write.RefOf(provider))
		}
	}
	sortRefs(dependents)
	return dependents, nil
}

// requirementList decodes the capability requirements of one definition.
func requirementList(definition *unstructured.Unstructured) ([]v1beta1.CapabilityRequirement, error) {
	raw, found, err := unstructured.NestedSlice(definition.Object, "spec", "requires")
	if err != nil || !found {
		return nil, err
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var requirements []v1beta1.CapabilityRequirement
	if err := json.Unmarshal(encoded, &requirements); err != nil {
		return nil, fmt.Errorf("definition %s: decode spec.requires: %w", definition.GetName(), err)
	}
	return requirements, nil
}

// requirementsMatch reports whether any requirement names one of the
// capabilities at a version the requirement admits.
func requirementsMatch(requirements []v1beta1.CapabilityRequirement, capabilities []Capability) bool {
	for _, requirement := range requirements {
		constraint, err := ParseConstraint(requirement.Version)
		if err != nil {
			continue
		}
		for _, capability := range capabilities {
			if requirement.Name == capability.Name && constraint.Allows(capability.Version) {
				return true
			}
		}
	}
	return false
}

// providerCapabilitiesMatch reports whether one Provider registers a
// capability the target contributes.
func providerCapabilitiesMatch(provider *unstructured.Unstructured, capabilities []Capability) bool {
	raw, found, err := unstructured.NestedSlice(provider.Object, "spec", "capabilities")
	if err != nil || !found {
		return false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return false
	}
	var registered []v1beta1.ProviderCapability
	if err := json.Unmarshal(encoded, &registered); err != nil {
		return false
	}
	for _, entry := range registered {
		for _, capability := range capabilities {
			if entry.Name == capability.Name && entry.Version == capability.Version {
				return true
			}
		}
	}
	return false
}

// listKernelObjects lists every object of one kernel kind.
func listKernelObjects(ctx context.Context, client dynamic.Interface, resources map[schema.GroupKind]string, kind string) ([]*unstructured.Unstructured, error) {
	plural, ok := resources[schema.GroupKind{Group: api.Group, Kind: kind}]
	if !ok {
		return nil, fmt.Errorf("component gate: the resource name of %s is unknown", kind)
	}
	resource := client.Resource(schema.GroupVersionResource{
		Group: api.Group, Version: v1beta1.GroupVersion.Version, Resource: plural,
	})
	namespace := ""
	if scope, _ := api.ScopeOf(kind); scope == api.ScopeNamespaced {
		namespace = metav1.NamespaceAll
	}
	list, err := resource.Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	objects := make([]*unstructured.Unstructured, 0, len(list.Items))
	for index := range list.Items {
		objects = append(objects, &list.Items[index])
	}
	return objects, nil
}

// sortRefs orders a dependent list and drops repeats.
func sortRefs(refs []write.Ref) {
	sort.SliceStable(refs, func(left, right int) bool { return refs[left].Key() < refs[right].Key() })
}
