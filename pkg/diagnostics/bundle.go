package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/abi"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/markers"
)

// Difference kinds of a render diff entry.
const (
	// DifferenceMissing marks a rendered object the cluster does not hold.
	DifferenceMissing = "Missing"
	// DifferenceDrifted marks a rendered object whose live fields differ.
	DifferenceDrifted = "Drifted"
	// DifferenceUnreadable marks a rendered object the chain cannot read.
	DifferenceUnreadable = "Unreadable"
	// DifferenceOmitted marks an object the bundle never carries a value from.
	DifferenceOmitted = "Omitted"
)

// Bundle is the diagnostic package of C-26 and C-56: the five content classes
// plus the evidence chain. It carries no log lines and no credential values.
type Bundle struct {
	// Application is the application the bundle describes.
	Application ObjectRef `json:"application"`
	// ExportedAt is when the bundle was assembled.
	ExportedAt time.Time `json:"exportedAt"`
	// ConditionSnapshot is the condition class (C-26).
	ConditionSnapshot []SnapshotEntry `json:"conditionSnapshot"`
	// RecentEvents is the event class (C-26, C-64).
	RecentEvents RecentEvents `json:"recentEvents"`
	// RenderDiff is the render difference class (C-26).
	RenderDiff RenderDiff `json:"renderDiff"`
	// Capabilities is the capability class (C-09, C-26).
	Capabilities []Capability `json:"capabilities"`
	// VersionMatrix is the version class (C-26).
	VersionMatrix VersionMatrix `json:"versionMatrix"`
	// Evidence is the chain of C-26.
	Evidence Chain `json:"evidence"`
}

// Encode renders the bundle as the exported document. It is JSON so the file
// stays readable with ordinary tools.
func (b Bundle) Encode() ([]byte, error) {
	encoded, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode diagnostic bundle: %w", err)
	}
	return append(encoded, '\n'), nil
}

// SnapshotEntry is one object's conditions at export time.
type SnapshotEntry struct {
	// Object is the observed object.
	Object ObjectRef `json:"object"`
	// Source names where the state came from.
	Source string `json:"source"`
	// ObservedAt is when the state was read.
	ObservedAt time.Time `json:"observedAt"`
	// Phase is the object's flow stage.
	Phase string `json:"phase,omitempty"`
	// Health is the object's availability state.
	Health string `json:"health,omitempty"`
	// Conditions is the stored condition list.
	Conditions []Condition `json:"conditions"`
}

// Condition is one stored condition.
type Condition struct {
	// Type is the condition type.
	Type string `json:"type"`
	// Status is True, False or Unknown.
	Status string `json:"status"`
	// Reason is the machine-readable cause.
	Reason string `json:"reason,omitempty"`
	// Message explains the condition.
	Message string `json:"message,omitempty"`
	// ObservedGeneration is the generation the writer saw.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// LastTransitionTime is when the condition last changed.
	LastTransitionTime time.Time `json:"lastTransitionTime,omitempty"`
}

// RecentEvents is the bundle's event class: the events of the read window that
// touch the application, newest first. An unreadable source is reported with a
// reason, never as an empty set (C-64).
type RecentEvents struct {
	// Window is the read window.
	Window string `json:"window"`
	// Available is false when the source could not be read.
	Available bool `json:"available"`
	// Reason explains an unavailable list.
	Reason string `json:"reason,omitempty"`
	// Events is the event list, newest first.
	Events []Event `json:"events"`
}

// Event is one cluster event.
type Event struct {
	// Namespace is the event's namespace.
	Namespace string `json:"namespace"`
	// Name is the event name.
	Name string `json:"name"`
	// Involved is the object the event is about.
	Involved ObjectRef `json:"involvedObject"`
	// Type is Normal or Warning.
	Type string `json:"type,omitempty"`
	// Reason is the short machine-readable cause.
	Reason string `json:"reason,omitempty"`
	// Message is the human-readable text.
	Message string `json:"message,omitempty"`
	// Count is how often the event repeated.
	Count int32 `json:"count,omitempty"`
	// FirstSeen is the first occurrence.
	FirstSeen time.Time `json:"firstSeen,omitempty"`
	// LastSeen is the newest occurrence.
	LastSeen time.Time `json:"lastSeen,omitempty"`
}

// RenderDiff is the bundle's render difference class: the objects the current
// definition renders and how the cluster differs from them. Only field paths are
// reported, never values, so a sensitive value cannot reach the bundle (C-14).
type RenderDiff struct {
	// Available is false when the definition could not be rendered.
	Available bool `json:"available"`
	// Reason explains an unavailable diff.
	Reason string `json:"reason,omitempty"`
	// Entries lists the differences, sorted by object.
	Entries []RenderDifference `json:"entries"`
}

// RenderDifference is one rendered object's difference.
type RenderDifference struct {
	// Object is the rendered object.
	Object ObjectRef `json:"object"`
	// Difference is Missing, Drifted, Unreadable or Omitted.
	Difference string `json:"difference"`
	// Fields are the differing field paths, never their values.
	Fields []string `json:"fields,omitempty"`
	// Reason explains an unreadable or omitted entry.
	Reason string `json:"reason,omitempty"`
	// ObservedAt is when the live object was read.
	ObservedAt time.Time `json:"observedAt,omitempty"`
}

// VersionMatrix is the bundle's version class: the kernel's served API versions,
// the installed extensions and the node versions.
type VersionMatrix struct {
	// KernelAPIVersions are the API versions the kernel serves at once (C-16).
	KernelAPIVersions []string `json:"kernelApiVersions"`
	// StorageVersion is the version the API server stores.
	StorageVersion string `json:"storageVersion"`
	// Extensions are the installed components.
	Extensions []Component `json:"extensions"`
	// Nodes are the nodes on the walked path.
	Nodes []NodeVersion `json:"nodes"`
}

// NodeVersion is one node's reported version.
type NodeVersion struct {
	// Node is the node name.
	Node string `json:"node"`
	// KubeletVersion is the reported kubelet version.
	KubeletVersion string `json:"kubeletVersion,omitempty"`
	// OSImage is the reported operating system image.
	OSImage string `json:"osImage,omitempty"`
}

// Bundle assembles the diagnostic bundle of one application (C-26, C-56).
func (s *Service) Bundle(ctx context.Context, namespace, name string) (Bundle, error) {
	assembly, err := s.assemble(ctx, namespace, name)
	if err != nil {
		return Bundle{}, err
	}
	bundle := Bundle{
		Application:       assembly.ref,
		ExportedAt:        s.now(),
		ConditionSnapshot: conditionSnapshot(assembly.observed),
		Capabilities:      sortedCapabilities(s.Environment.Capabilities),
		VersionMatrix:     s.versionMatrix(assembly),
		Evidence:          assembly.chain(),
	}
	bundle.RecentEvents = s.recentEvents(ctx, assembly)
	bundle.RenderDiff = s.renderDiff(ctx, assembly)
	return bundle, nil
}

// conditionSnapshot renders the observed conditions in chain order.
func conditionSnapshot(observed []Observed) []SnapshotEntry {
	entries := make([]SnapshotEntry, 0, len(observed))
	for _, item := range observed {
		phase, health, _ := stateOf(item.Object)
		entries = append(entries, SnapshotEntry{
			Object:     item.Ref,
			Source:     sourceName(item.Ref),
			ObservedAt: item.ObservedAt,
			Phase:      phase,
			Health:     health,
			Conditions: conditionsOf(item.Object),
		})
	}
	return entries
}

// conditionsOf reads the stored condition list of an object.
func conditionsOf(object *unstructured.Unstructured) []Condition {
	raw, found, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil || !found {
		return []Condition{}
	}
	conditions := make([]Condition, 0, len(raw))
	for _, entry := range raw {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		condition := Condition{}
		condition.Type, _ = fields["type"].(string)
		condition.Status, _ = fields["status"].(string)
		condition.Reason, _ = fields["reason"].(string)
		condition.Message, _ = fields["message"].(string)
		if generation, ok := fields["observedGeneration"].(int64); ok {
			condition.ObservedGeneration = generation
		}
		if stamp, ok := fields["lastTransitionTime"].(string); ok {
			condition.LastTransitionTime = parseTime(stamp)
		}
		conditions = append(conditions, condition)
	}
	return conditions
}

// recentEvents reads the events of the window that touch an observed object
// (C-64). Events are read per observed object with a field selector, so one
// export reads a slice of the event class instead of all of it: a namespaced
// object's events are read in its namespace, a cluster-scoped object's
// cluster-wide, because a node's events are recorded outside the application's
// namespace.
func (s *Service) recentEvents(ctx context.Context, a *assembly) RecentEvents {
	window := s.eventWindow()
	result := RecentEvents{Window: window.String(), Available: true, Events: []Event{}}
	listed := make([]Observed, 0, len(a.observed))
	for _, query := range eventQueries(a.observed) {
		found, err := s.Source.List(ctx, query)
		if err != nil {
			result.Available = false
			result.Reason = fmt.Sprintf("事件不可读：%v", err)
			return result
		}
		listed = append(listed, found...)
	}
	known := make(map[string]bool, len(a.observed))
	for _, observed := range a.observed {
		known[eventSubject(observed.Ref)] = true
	}
	now := s.now()
	cutoff := now.Add(-window)
	for _, observed := range listed {
		event, ok := decodeEvent(observed)
		if !ok {
			continue
		}
		if !known[eventSubject(event.Involved)] {
			continue
		}
		if event.LastSeen.Before(cutoff) {
			continue
		}
		result.Events = append(result.Events, event)
	}
	sort.Slice(result.Events, func(left, right int) bool {
		if !result.Events[left].LastSeen.Equal(result.Events[right].LastSeen) {
			return result.Events[left].LastSeen.After(result.Events[right].LastSeen)
		}
		return result.Events[left].Name < result.Events[right].Name
	})
	return result
}

// eventQueries names one event read per observed object, so C-64 filters by
// object rather than by namespace. A namespaced object is read in its own
// namespace; a cluster-scoped one is read cluster-wide.
func eventQueries(observed []Observed) []Target {
	queries := make([]Target, 0, len(observed))
	seen := make(map[Target]bool, len(observed))
	for _, entry := range observed {
		query := Target{
			Version: "v1", Kind: "Event", Resource: "events",
			Namespace:     entry.Ref.Namespace,
			FieldSelector: eventFieldSelector(entry.Ref),
		}
		if seen[query] {
			continue
		}
		seen[query] = true
		queries = append(queries, query)
	}
	return queries
}

// eventFieldSelector narrows one event read to the object it is about.
func eventFieldSelector(ref ObjectRef) string {
	return fields.Set{
		"involvedObject.kind": ref.Kind,
		"involvedObject.name": ref.Name,
	}.AsSelector().String()
}

// eventSubject is the filter key of an event's involved object. A namespace is
// part of the key only when both sides carry one: a node event has no namespace
// and must still match the node the chain observed.
func eventSubject(ref ObjectRef) string {
	if ref.Namespace == "" {
		return ref.Kind + "//" + ref.Name
	}
	return ref.Kind + "/" + ref.Namespace + "/" + ref.Name
}

// decodeEvent reads one stored event.
func decodeEvent(observed Observed) (Event, bool) {
	involved, found, err := unstructured.NestedMap(observed.Object.Object, "involvedObject")
	if err != nil || !found {
		return Event{}, false
	}
	kind, _ := involved["kind"].(string)
	name, _ := involved["name"].(string)
	namespace, _ := involved["namespace"].(string)
	if kind == "" || name == "" {
		return Event{}, false
	}
	event := Event{
		Namespace: observed.Object.GetNamespace(),
		Name:      observed.Object.GetName(),
		Involved:  ObjectRef{Kind: kind, Namespace: namespace, Name: name},
	}
	event.Type, _, _ = unstructured.NestedString(observed.Object.Object, "type")
	event.Reason, _, _ = unstructured.NestedString(observed.Object.Object, "reason")
	event.Message, _, _ = unstructured.NestedString(observed.Object.Object, "message")
	if count, found, err := unstructured.NestedInt64(observed.Object.Object, "count"); err == nil && found {
		event.Count = int32(count)
	}
	event.FirstSeen = eventTime(observed.Object, "firstTimestamp")
	if event.FirstSeen.IsZero() {
		event.FirstSeen = eventTime(observed.Object, "eventTime")
	}
	event.LastSeen = eventTime(observed.Object, "lastTimestamp")
	if event.LastSeen.IsZero() {
		event.LastSeen = eventTime(observed.Object, "eventTime")
	}
	if event.LastSeen.IsZero() {
		event.LastSeen = observed.Object.GetCreationTimestamp().Time
	}
	if event.FirstSeen.IsZero() {
		event.FirstSeen = event.LastSeen
	}
	return event, true
}

// eventTime reads one timestamp field of an event.
func eventTime(object *unstructured.Unstructured, field string) time.Time {
	value, found, err := unstructured.NestedString(object.Object, field)
	if err != nil || !found {
		return time.Time{}
	}
	return parseTime(value)
}

// parseTime reads the timestamp layouts Kubernetes objects carry.
func parseTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000000Z07:00"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// renderDiff re-renders the application's definition and compares the result
// with the cluster (C-26).
func (s *Service) renderDiff(ctx context.Context, a *assembly) RenderDiff {
	application := a.application
	if strings.TrimSpace(application.Spec.DefinitionRef) == "" {
		return RenderDiff{Available: false, Reason: "应用没有声明定义"}
	}
	resource, _ := api.ResourceName("ApplicationDefinition")
	observed, err := s.Source.Get(ctx, Target{
		Group: api.Group, Version: v1beta1.GroupVersion.Version,
		Kind: "ApplicationDefinition", Resource: resource, Name: application.Spec.DefinitionRef,
	})
	if apierrors.IsNotFound(err) {
		return RenderDiff{Available: false, Reason: "应用定义不存在"}
	}
	if err != nil {
		return RenderDiff{Available: false, Reason: "读取应用定义失败：" + err.Error()}
	}
	definition := &v1beta1.ApplicationDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(observed.Object.Object, definition); err != nil {
		return RenderDiff{Available: false, Reason: "解码应用定义失败：" + err.Error()}
	}
	config, err := applicationConfig(application)
	if err != nil {
		return RenderDiff{Available: false, Reason: "读取应用配置失败：" + err.Error()}
	}
	resolved, err := app.Resolve(definition.Spec.Config, config)
	if err != nil {
		return RenderDiff{Available: false, Reason: "解析应用配置失败：" + err.Error()}
	}
	volumes := make(map[string]string, len(application.Spec.Volumes))
	for _, name := range application.Spec.Volumes {
		volumes[name] = name
	}
	objects, err := app.Render(definition.Spec, app.RenderInput{
		Name:      application.Name,
		Namespace: application.Spec.Namespace,
		Config:    resolved,
		Volumes:   volumes,
	})
	if err != nil {
		return RenderDiff{Available: false, Reason: "重新渲染失败：" + err.Error()}
	}
	diff := RenderDiff{Available: true, Entries: []RenderDifference{}}
	for _, object := range objects {
		entry, differs := s.diffObject(ctx, object)
		if differs {
			diff.Entries = append(diff.Entries, entry)
		}
	}
	sort.Slice(diff.Entries, func(left, right int) bool {
		return diff.Entries[left].Object.String() < diff.Entries[right].Object.String()
	})
	return diff
}

// diffObject compares one rendered object with its stored counterpart. It
// reports only whether the object differs and on which paths.
func (s *Service) diffObject(ctx context.Context, object *unstructured.Unstructured) (RenderDifference, bool) {
	gvk := object.GroupVersionKind()
	ref := ObjectRef{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind, Namespace: object.GetNamespace(), Name: object.GetName()}
	if object.GetKind() == "Secret" {
		return RenderDifference{Object: ref, Difference: DifferenceOmitted, Reason: "凭据不进入诊断包"}, true
	}
	target, ok := targetForRendered(ref, s.Environment)
	if !ok {
		return RenderDifference{Object: ref, Difference: DifferenceUnreadable, Reason: "该类不在诊断读取范围内"}, true
	}
	observed, err := s.Source.Get(ctx, target)
	if apierrors.IsNotFound(err) {
		return RenderDifference{Object: ref, Difference: DifferenceMissing, ObservedAt: s.now()}, true
	}
	if err != nil {
		return RenderDifference{Object: ref, Difference: DifferenceUnreadable, Reason: err.Error()}, true
	}
	fields := differingFields(object.Object, observed.Object.Object)
	if len(fields) == 0 {
		return RenderDifference{}, false
	}
	return RenderDifference{
		Object: ref, Difference: DifferenceDrifted, Fields: fields, ObservedAt: observed.ObservedAt,
	}, true
}

// targetForRendered resolves the read target of one rendered object.
func targetForRendered(ref ObjectRef, environment Environment) (Target, bool) {
	typed := v1beta1.ObjectRef{
		Group: ref.Group, Version: ref.Version, Kind: ref.Kind,
		Namespace: ref.Namespace, Name: ref.Name,
	}
	if target, ok := targetOf(typed); ok {
		return target, true
	}
	adapter, ok := environment.adapterFor(ref.Group, ref.Kind)
	if !ok {
		return Target{}, false
	}
	return Target{
		Group: ref.Group, Version: adapter.Version, Kind: ref.Kind,
		Resource: adapter.Resource, Namespace: ref.Namespace, Name: ref.Name,
	}, true
}

// differingFields returns the paths where the stored object no longer matches
// the render, in both directions: a rendered path whose value drifted, and a
// live path an apply manager owns that the render no longer names (a field the
// template dropped or renamed). It returns paths, never values (C-14).
func differingFields(desired, live map[string]any) []string {
	wanted := map[string]string{}
	collectLeaves(desired, "", wanted)
	actual := map[string]string{}
	collectLeaves(live, "", actual)
	fields := make([]string, 0)
	for path, value := range wanted {
		if other, found := actual[path]; !found || other != value {
			fields = append(fields, path)
		}
	}
	fields = append(fields, residualFields(actual, wanted, appliedPaths(live))...)
	sort.Strings(fields)
	return fields
}

// residualFields returns the live-only paths an apply manager owns. Fields the
// API server or a controller defaults are not owned by an apply manager, so
// they never read as drift.
func residualFields(live, desired map[string]string, owned map[string]bool) []string {
	residual := make([]string, 0)
	for path := range live {
		if _, rendered := desired[path]; rendered {
			continue
		}
		if !owned[path] && !hasOwnedAncestor(owned, path) {
			continue
		}
		if isAttributionPath(path) {
			continue
		}
		residual = append(residual, path)
	}
	sort.Strings(residual)
	return residual
}

// hasOwnedAncestor reports whether one of path's parents is owned as a whole.
func hasOwnedAncestor(owned map[string]bool, path string) bool {
	for index := len(path) - 1; index > 0; index-- {
		if path[index] != '.' && path[index] != '[' {
			continue
		}
		if owned[path[:index]] {
			return true
		}
	}
	return false
}

// isAttributionPath reports the metadata the kernel itself stamps on an object;
// a render never carries it and it is not drift (C-02).
func isAttributionPath(path string) bool {
	if strings.HasPrefix(path, "metadata.ownerReferences") {
		return true
	}
	for _, key := range markers.Keys() {
		if path == "metadata.labels."+key || path == "metadata.annotations."+key {
			return true
		}
	}
	return false
}

// appliedPaths returns the field paths a server-side apply manager owns on one
// object.
func appliedPaths(live map[string]any) map[string]bool {
	paths := map[string]bool{}
	metadata, _ := live["metadata"].(map[string]any)
	entries, _ := metadata["managedFields"].([]any)
	for _, entry := range entries {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if operation, _ := fields["operation"].(string); operation != "Apply" {
			continue
		}
		tree, ok := fields["fieldsV1"].(map[string]any)
		if !ok {
			continue
		}
		expandManaged(tree, "", live, paths)
	}
	return paths
}

// expandManaged walks a fieldsV1 tree into dotted paths. The tree marks fields
// as f:<name>, associative-list keys as k:<json>, list indices as i:<n> and set
// members as v:<value>; only an f: entry extends the path.
func expandManaged(node map[string]any, path string, live any, paths map[string]bool) {
	for key, child := range node {
		switch {
		case strings.HasPrefix(key, "f:"):
			name := strings.TrimPrefix(key, "f:")
			childPath := name
			if path != "" {
				childPath = path + "." + name
			}
			typed, ok := child.(map[string]any)
			if !ok || len(typed) == 0 {
				paths[childPath] = true
				continue
			}
			expandManaged(typed, childPath, fieldOf(live, name), paths)
		case strings.HasPrefix(key, "k:"):
			index, element := associativeIndex(key[2:], live)
			if index < 0 {
				continue
			}
			if typed, ok := child.(map[string]any); ok {
				expandManaged(typed, fmt.Sprintf("%s[%d]", path, index), element, paths)
			}
		case strings.HasPrefix(key, "i:"):
			index, element := indexedElement(key[2:], live)
			if typed, ok := child.(map[string]any); ok {
				expandManaged(typed, fmt.Sprintf("%s[%d]", path, index), element, paths)
			}
		}
	}
}

// fieldOf reads one map field of a live value.
func fieldOf(live any, name string) any {
	object, ok := live.(map[string]any)
	if !ok {
		return nil
	}
	return object[name]
}

// associativeIndex resolves a fieldsV1 associative key against the live list.
func associativeIndex(key string, live any) (int, any) {
	items, ok := live.([]any)
	if !ok {
		return -1, nil
	}
	want := map[string]any{}
	if err := json.Unmarshal([]byte(key), &want); err != nil {
		return -1, nil
	}
	for index, item := range items {
		element, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fieldsMatch(element, want) {
			return index, element
		}
	}
	return -1, nil
}

// matchedFields reports whether element carries every key of want.
func fieldsMatch(element, want map[string]any) bool {
	for key, value := range want {
		actual, found := element[key]
		if !found {
			return false
		}
		actualEncoded, err := json.Marshal(actual)
		if err != nil {
			return false
		}
		wantEncoded, err := json.Marshal(value)
		if err != nil {
			return false
		}
		if string(actualEncoded) != string(wantEncoded) {
			return false
		}
	}
	return true
}

// indexedElement resolves a fieldsV1 list index against the live list.
func indexedElement(key string, live any) (int, any) {
	items, ok := live.([]any)
	if !ok {
		return 0, nil
	}
	index, err := strconv.Atoi(key)
	if err != nil || index < 0 || index >= len(items) {
		return 0, nil
	}
	return index, items[index]
}

// collectLeaves flattens one JSON tree into path to encoded-leaf entries.
func collectLeaves(node any, path string, leaves map[string]string) {
	switch typed := node.(type) {
	case map[string]any:
		if path == "status" || path == "metadata.managedFields" {
			return
		}
		if len(typed) == 0 && path != "" {
			leaves[path] = "{}"
			return
		}
		for key, value := range typed {
			child := key
			if path != "" {
				child = path + "." + key
			}
			collectLeaves(value, child, leaves)
		}
	case []any:
		if len(typed) == 0 && path != "" {
			leaves[path] = "[]"
			return
		}
		for index, value := range typed {
			collectLeaves(value, fmt.Sprintf("%s[%d]", path, index), leaves)
		}
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			leaves[path] = fmt.Sprintf("%v", typed)
			return
		}
		leaves[path] = string(encoded)
	}
}

// applicationConfig decodes the application's submitted config values.
func applicationConfig(application *v1beta1.Application) (map[string]any, error) {
	if len(application.Spec.Config.Raw) == 0 {
		return nil, nil
	}
	values := map[string]any{}
	if err := json.Unmarshal(application.Spec.Config.Raw, &values); err != nil {
		return nil, fmt.Errorf("decode spec.config: %w", err)
	}
	return values, nil
}

// versionMatrix renders the bundle's version class.
func (s *Service) versionMatrix(a *assembly) VersionMatrix {
	matrix := VersionMatrix{
		KernelAPIVersions: append([]string(nil), abi.Versions...),
		StorageVersion:    abi.StorageVersion,
		Extensions:        sortedComponents(s.Environment.Components),
		Nodes:             []NodeVersion{},
	}
	for _, observed := range a.observed {
		if observed.Ref.Kind != "Node" {
			continue
		}
		version := NodeVersion{Node: observed.Ref.Name}
		version.KubeletVersion, _, _ = unstructured.NestedString(observed.Object.Object, "status", "nodeInfo", "kubeletVersion")
		version.OSImage, _, _ = unstructured.NestedString(observed.Object.Object, "status", "nodeInfo", "osImage")
		matrix.Nodes = append(matrix.Nodes, version)
	}
	sort.Slice(matrix.Nodes, func(left, right int) bool {
		return matrix.Nodes[left].Node < matrix.Nodes[right].Node
	})
	return matrix
}

// sortedCapabilities returns the catalogue sorted by name and version.
func sortedCapabilities(capabilities []Capability) []Capability {
	sorted := append([]Capability(nil), capabilities...)
	sort.Slice(sorted, func(left, right int) bool {
		if sorted[left].Name != sorted[right].Name {
			return sorted[left].Name < sorted[right].Name
		}
		return sorted[left].Version < sorted[right].Version
	})
	if sorted == nil {
		return []Capability{}
	}
	return sorted
}

// sortedComponents returns the installed components sorted by identifier.
func sortedComponents(components []Component) []Component {
	sorted := append([]Component(nil), components...)
	sort.Slice(sorted, func(left, right int) bool {
		if sorted[left].Identifier != sorted[right].Identifier {
			return sorted[left].Identifier < sorted[right].Identifier
		}
		return sorted[left].Version < sorted[right].Version
	})
	if sorted == nil {
		return []Component{}
	}
	return sorted
}

// eventWindow is how far back the bundle reads events (C-64).
func (s *Service) eventWindow() time.Duration {
	if s.EventWindow <= 0 {
		return time.Hour
	}
	return s.EventWindow
}
