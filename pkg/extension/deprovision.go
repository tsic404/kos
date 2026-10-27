package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
)

// ComponentFinalizer keeps a system component visible while another object
// still depends on it (C-17, C-48). It is written by the component reconciler,
// the subject that also writes the component status (C-03).
const ComponentFinalizer = "kos.tsic.top/system-component"

// DeprovisionBlocked is the condition of a component whose deletion a dependent
// keeps open (C-48).
const (
	// ConditionDeprovisionBlocked reports a blocked deletion.
	ConditionDeprovisionBlocked = "DeprovisionBlocked"
	// ReasonDeprovisionBlocked is its reason.
	ReasonDeprovisionBlocked = "DeprovisionBlocked"
)

// Gate holds a deleting system component open while another object depends on
// it, and releases the component once nothing does (C-17, C-46, C-48). It reads
// the dependency edges from the endpoints: the spec references and the issuer
// an endpoint's status resolved.
type Gate struct {
	// Dynamic reads the components and the endpoints that depend on them.
	Dynamic dynamic.Interface
	// Resources maps a kernel kind to its served resource name.
	Resources map[schema.GroupKind]string
	// Status writes the component condition and finalizers (C-03).
	Status *write.Writer
}

// Blocked is one component a dependent kept from being removed, with every
// dependent that holds it.
type Blocked struct {
	// Name is the component's object name.
	Name string
	// Dependents are the endpoints that reference it, ordered.
	Dependents []write.Ref
}

// Reconcile keeps the gate on every managed component, blocks the deletion of a
// component that still has dependents and releases one that has none. It
// returns the components it held open.
func (g *Gate) Reconcile(ctx context.Context) ([]Blocked, error) {
	if g.Dynamic == nil || g.Status == nil {
		return nil, errors.New("component gate needs a dynamic client and a status writer")
	}
	components, err := g.list(ctx, "SystemComponent")
	if err != nil {
		return nil, err
	}
	endpoints, err := g.list(ctx, "Endpoint")
	if err != nil {
		return nil, err
	}
	dependents := dependentsOf(endpoints)
	blocked := make([]Blocked, 0, len(components))
	for _, component := range components {
		attribution, err := markers.AttributionOf(component.GetLabels())
		if err != nil {
			return nil, fmt.Errorf("component %s: %w", component.GetName(), err)
		}
		if attribution == markers.AttributionUnmanaged {
			continue
		}
		refs := dependents[component.GetName()]
		if component.GetDeletionTimestamp() == nil {
			if err := g.keepFinalizer(ctx, component); err != nil {
				return nil, err
			}
			if err := g.clearBlocked(ctx, component); err != nil {
				return nil, err
			}
			continue
		}
		if !hasFinalizer(component, ComponentFinalizer) {
			// The deletion started before the gate ever observed the component:
			// the API server refuses new finalizers on a deleting object, so the
			// gate cannot hold it and must not claim a block that would not hold
			// (C-48). The component leaves the install set like any uninstall.
			continue
		}
		if len(refs) > 0 {
			if err := g.blockDeletion(ctx, component, refs); err != nil {
				return nil, err
			}
			blocked = append(blocked, Blocked{Name: component.GetName(), Dependents: refs})
			continue
		}
		if err := g.releaseFinalizer(ctx, component); err != nil {
			return nil, err
		}
	}
	return blocked, nil
}

// BlockedNames returns the names of the components a round held open, so the
// install pass does not treat a refused deletion as an uninstall (C-48).
func BlockedNames(blocked []Blocked) map[string]bool {
	names := make(map[string]bool, len(blocked))
	for _, component := range blocked {
		names[component.Name] = true
	}
	return names
}

// dependentsOf indexes the endpoints by the component each of them depends on
// (C-46).
func dependentsOf(endpoints []*unstructured.Unstructured) map[string][]write.Ref {
	dependents := make(map[string][]write.Ref, len(endpoints))
	for _, object := range endpoints {
		ref := write.RefOf(object)
		for _, component := range endpoint.ComponentRefs(object) {
			dependents[component] = append(dependents[component], ref)
		}
	}
	for name, refs := range dependents {
		dependents[name] = orderedRefs(refs)
	}
	return dependents
}

// orderedRefs sorts a dependent list and drops repeats, so the same set always
// renders the same message.
func orderedRefs(refs []write.Ref) []write.Ref {
	kept := make([]write.Ref, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if seen[ref.Key()] {
			continue
		}
		seen[ref.Key()] = true
		kept = append(kept, ref)
	}
	sort.Slice(kept, func(left, right int) bool { return kept[left].Key() < kept[right].Key() })
	return kept
}

// namesOf renders the dependent list the way the condition carries it: every
// dependent named, namespace-qualified.
func namesOf(refs []write.Ref) string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		name := ref.Name
		if ref.Namespace != "" {
			name = ref.Namespace + "/" + name
		}
		names = append(names, name)
	}
	joined := ""
	for index, name := range names {
		if index > 0 {
			joined += ", "
		}
		joined += name
	}
	return joined
}

// keepFinalizer puts the gate on a component the first time its reconciler
// observes it, so a later deletion is observed instead of vanishing (C-48).
func (g *Gate) keepFinalizer(ctx context.Context, component *unstructured.Unstructured) error {
	if hasFinalizer(component, ComponentFinalizer) {
		return nil
	}
	finalizers := append(append([]string{}, component.GetFinalizers()...), ComponentFinalizer)
	_, err := g.Status.SetFinalizers(ctx, write.SubjectBackupReconciler, write.RefOf(component), finalizers)
	return err
}

// releaseFinalizer lets a deletion finish once nothing depends on the
// component. The object is gone when the API server removes it, so a missing
// object is the desired state.
func (g *Gate) releaseFinalizer(ctx context.Context, component *unstructured.Unstructured) error {
	if !hasFinalizer(component, ComponentFinalizer) {
		return nil
	}
	finalizers := make([]string, 0, len(component.GetFinalizers()))
	for _, finalizer := range component.GetFinalizers() {
		if finalizer == ComponentFinalizer {
			continue
		}
		finalizers = append(finalizers, finalizer)
	}
	_, err := g.Status.SetFinalizers(ctx, write.SubjectBackupReconciler, write.RefOf(component), finalizers)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// blockDeletion writes the blocked condition and lists every dependent, so the
// panel can show who holds the component (C-48).
func (g *Gate) blockDeletion(ctx context.Context, component *unstructured.Unstructured, dependents []write.Ref) error {
	status, err := statusOf(component)
	if err != nil {
		return err
	}
	conditions, err := conditionsFrom(status["conditions"])
	if err != nil {
		return fmt.Errorf("component %s: %w", component.GetName(), err)
	}
	condition := state.Condition(ConditionDeprovisionBlocked, metav1.ConditionTrue, component.GetGeneration(),
		ReasonDeprovisionBlocked, "删除被依赖阻塞：依赖者 "+namesOf(dependents))
	status["conditions"] = conditionsValue(state.Set(conditions, condition))
	return g.applyStatus(ctx, component, status)
}

// clearBlocked drops a block that no longer holds, so a component that stays
// installed does not keep a stale condition.
func (g *Gate) clearBlocked(ctx context.Context, component *unstructured.Unstructured) error {
	status, err := statusOf(component)
	if err != nil {
		return err
	}
	conditions, err := conditionsFrom(status["conditions"])
	if err != nil {
		return fmt.Errorf("component %s: %w", component.GetName(), err)
	}
	kept := make([]metav1.Condition, 0, len(conditions))
	for _, condition := range conditions {
		if condition.Type == ConditionDeprovisionBlocked {
			continue
		}
		kept = append(kept, condition)
	}
	if len(kept) == len(conditions) {
		return nil
	}
	status["conditions"] = conditionsValue(kept)
	return g.applyStatus(ctx, component, status)
}

// statusOf reads the stored status map; an object without one starts empty, so
// the phase and health another writer already set are never dropped.
func statusOf(component *unstructured.Unstructured) (map[string]any, error) {
	status, found, err := unstructured.NestedMap(component.Object, "status")
	if err != nil {
		return nil, fmt.Errorf("component %s: %w", component.GetName(), err)
	}
	if !found || status == nil {
		status = map[string]any{}
	}
	return status, nil
}

// applyStatus writes the whole status only when it differs from the stored one,
// so an unchanged round writes nothing (C-06).
func (g *Gate) applyStatus(ctx context.Context, component *unstructured.Unstructured, status map[string]any) error {
	stored, found, err := unstructured.NestedMap(component.Object, "status")
	if err != nil {
		return fmt.Errorf("component %s: %w", component.GetName(), err)
	}
	if found && sameJSON(stored, status) {
		return nil
	}
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "SystemComponent",
		"metadata":   map[string]any{"name": component.GetName()},
		"status":     status,
	}}
	_, err = g.Status.Apply(ctx, write.SubjectBackupReconciler, write.Session{}, write.PartStatus, document)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// list reads every object of one kernel kind across namespaces.
func (g *Gate) list(ctx context.Context, kind string) ([]*unstructured.Unstructured, error) {
	plural, known := g.Resources[schema.GroupKind{Group: api.Group, Kind: kind}]
	if !known {
		return nil, fmt.Errorf("component gate: the resource name of %s is unknown", kind)
	}
	client := g.Dynamic.Resource(schema.GroupVersionResource{
		Group: api.Group, Version: v1beta1.GroupVersion.Version, Resource: plural,
	})
	namespace := ""
	if scope, _ := api.ScopeOf(kind); scope == api.ScopeNamespaced {
		namespace = metav1.NamespaceAll
	}
	list, err := client.Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	objects := make([]*unstructured.Unstructured, 0, len(list.Items))
	for index := range list.Items {
		objects = append(objects, &list.Items[index])
	}
	return objects, nil
}

// hasFinalizer reports whether an object carries one finalizer.
func hasFinalizer(object *unstructured.Unstructured, finalizer string) bool {
	for _, existing := range object.GetFinalizers() {
		if existing == finalizer {
			return true
		}
	}
	return false
}

// conditionsFrom decodes the condition list of a status map.
func conditionsFrom(value any) ([]metav1.Condition, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var conditions []metav1.Condition
	if err := json.Unmarshal(encoded, &conditions); err != nil {
		return nil, fmt.Errorf("decode conditions: %w", err)
	}
	return conditions, nil
}

// conditionsValue encodes a condition list as the value a status map carries.
func conditionsValue(conditions []metav1.Condition) any {
	if conditions == nil {
		return []any{}
	}
	encoded, err := json.Marshal(conditions)
	if err != nil {
		return []any{}
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return []any{}
	}
	return value
}
