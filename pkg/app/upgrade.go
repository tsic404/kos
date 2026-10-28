package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
)

// digestAlgorithm is the form the package digest of C-32 and the spec and
// render digests of C-34 share.
const digestAlgorithm = "sha256:"

// target is one rendered desired state of an application: what the current spec
// asks for, rendered from one package (C-34).
type target struct {
	// definition is the package the target renders from.
	definition *v1beta1.ApplicationDefinition
	// objects are the manifests the target writes, owner-stamped.
	objects []*unstructured.Unstructured
	// applied is the baseline the target would establish.
	applied *v1beta1.AppliedState
}

// SpecDigest digests the parts of an application spec an upgrade answers: the
// definition, the declared namespace, the config and the volume declarations.
// Owner and visibility are not desired state of the workloads (C-34, C-60).
func SpecDigest(spec v1beta1.ApplicationSpec) (string, error) {
	config := any(map[string]any{})
	if len(spec.Config.Raw) > 0 {
		if err := json.Unmarshal(spec.Config.Raw, &config); err != nil {
			return "", fmt.Errorf("decode the application config: %w", err)
		}
	}
	volumes := append([]string(nil), spec.Volumes...)
	sort.Strings(volumes)
	canonical, err := json.Marshal(struct {
		DefinitionRef string `json:"definitionRef"`
		Namespace     string `json:"namespace"`
		Config        any    `json:"config"`
		Volumes       []string
	}{spec.DefinitionRef, spec.Namespace, config, volumes})
	if err != nil {
		return "", fmt.Errorf("encode the application spec: %w", err)
	}
	return digestOf(canonical), nil
}

// RenderDigest digests a manifest set. The objects are ordered by identity and
// each is encoded with sorted keys, so the same set always yields one digest
// (C-34, C-35).
func RenderDigest(objects []*unstructured.Unstructured) (string, error) {
	ordered := append([]*unstructured.Unstructured(nil), objects...)
	sort.Slice(ordered, func(left, right int) bool {
		return identityOfRef(objectRefOf(ordered[left])) < identityOfRef(objectRefOf(ordered[right]))
	})
	hash := sha256.New()
	for _, object := range ordered {
		encoded, err := json.Marshal(object.Object)
		if err != nil {
			return "", fmt.Errorf("encode the rendered object %s: %w", object.GetName(), err)
		}
		hash.Write(encoded)
	}
	return digestAlgorithm + hex.EncodeToString(hash.Sum(nil)), nil
}

// digestOf hashes canonical bytes.
func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return digestAlgorithm + hex.EncodeToString(sum[:])
}

// identityOfRef names one object in a manifest set.
func identityOfRef(ref v1beta1.ObjectRef) string {
	return ref.Kind + "|" + ref.Namespace + "|" + ref.Name
}

// encodeManifests keeps a manifest set in the status of the baseline (C-34).
func encodeManifests(objects []*unstructured.Unstructured) ([]runtime.RawExtension, error) {
	encoded := make([]runtime.RawExtension, 0, len(objects))
	for _, object := range objects {
		raw, err := json.Marshal(object.Object)
		if err != nil {
			return nil, fmt.Errorf("encode the manifest %s: %w", object.GetName(), err)
		}
		encoded = append(encoded, runtime.RawExtension{Raw: raw})
	}
	return encoded, nil
}

// decodeManifests reads the manifest set a baseline kept (C-34).
func decodeManifests(kept []runtime.RawExtension) ([]*unstructured.Unstructured, error) {
	objects := make([]*unstructured.Unstructured, 0, len(kept))
	for _, raw := range kept {
		if len(raw.Raw) == 0 {
			continue
		}
		decoded := map[string]any{}
		if err := json.Unmarshal(raw.Raw, &decoded); err != nil {
			return nil, fmt.Errorf("decode a kept manifest: %w", err)
		}
		objects = append(objects, &unstructured.Unstructured{Object: decoded})
	}
	return objects, nil
}

// encodeConfig keeps the resolved config in the baseline (C-34).
func encodeConfig(config map[string]any) (runtime.RawExtension, error) {
	raw, err := json.Marshal(config)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("encode the resolved config: %w", err)
	}
	return runtime.RawExtension{Raw: raw}, nil
}

// decodeConfigValues reads the config a baseline kept (C-34).
func decodeConfigValues(raw runtime.RawExtension) (map[string]any, error) {
	values := map[string]any{}
	if len(raw.Raw) == 0 {
		return values, nil
	}
	if err := json.Unmarshal(raw.Raw, &values); err != nil {
		return nil, fmt.Errorf("decode the applied config: %w", err)
	}
	return values, nil
}

// VolumeFacts is what the upgrade preflight needs to know about one volume the
// application declares (C-34, C-38).
type VolumeFacts struct {
	// DesiredSize is the size the volume asks for.
	DesiredSize string
	// ActualSize is the size the volume is bound with.
	ActualSize string
	// DesiredPolicy is the storage policy the volume asks for.
	DesiredPolicy string
	// ActualPolicy is the storage policy the volume is bound with.
	ActualPolicy string
	// Found reports that the volume's claim exists.
	Found bool
	// Bound reports that the claim is bound, so it has a size and a policy the
	// upgrade can be judged against (C-38).
	Bound bool
}

// VolumeSource reads the facts a preflight needs about a declared volume. The
// volume module owns those fields; the application only asks (C-34).
type VolumeSource interface {
	VolumeFacts(ctx context.Context, namespace, name string) (VolumeFacts, error)
}

// buildTarget renders the desired state the current spec names (C-34).
func (c *Controller) buildTarget(application *v1beta1.Application, definition *v1beta1.ApplicationDefinition, config map[string]any) (*target, error) {
	objects, err := Render(definition.Spec, renderInput(application, config))
	if err != nil {
		return nil, err
	}
	owner := string(application.UID)
	for _, object := range objects {
		stampOwner(object, owner)
	}
	renderDigest, err := RenderDigest(objects)
	if err != nil {
		return nil, err
	}
	spec, err := SpecDigest(application.Spec)
	if err != nil {
		return nil, err
	}
	manifests, err := encodeManifests(objects)
	if err != nil {
		return nil, err
	}
	encodedConfig, err := encodeConfig(config)
	if err != nil {
		return nil, err
	}
	volumes := append([]string(nil), application.Spec.Volumes...)
	sort.Strings(volumes)
	return &target{
		definition: definition,
		objects:    objects,
		applied: &v1beta1.AppliedState{
			SpecDigest:       spec,
			DefinitionDigest: definition.Spec.Package.Digest,
			DataFormat:       definition.Spec.Package.DataFormat,
			RenderDigest:     renderDigest,
			Config:           encodedConfig,
			Volumes:          volumes,
			Render:           manifests,
		},
	}, nil
}

// preflight is the first step of an upgrade (C-34): it reports the config
// difference, the rendered manifest difference and the irreversible items, and
// never writes an object.
func (c *Controller) preflight(ctx context.Context, application *v1beta1.Application, applied *v1beta1.AppliedState, next *target) (v1beta1.UpgradeStatus, error) {
	report := v1beta1.UpgradeStatus{
		Target:       next.applied.DefinitionDigest,
		SpecDigest:   next.applied.SpecDigest,
		RenderDigest: next.applied.RenderDigest,
	}
	if applied == nil {
		return report, nil
	}

	configChanges, err := configDiff(applied.Config, next.applied.Config)
	if err != nil {
		return report, err
	}
	report.ConfigChanges = configChanges

	renderChanges, err := manifestDiff(applied.Render, next.objects)
	if err != nil {
		return report, err
	}
	report.RenderChanges = renderChanges

	irreversible, migrations, err := c.irreversibleItems(ctx, application, applied, next)
	if err != nil {
		return report, err
	}
	report.Irreversible = irreversible
	report.Migrations = migrations
	return report, nil
}

// irreversibleItems judges one target against the baseline it would replace: a
// data format change and a volume that is dropped or shrunk are refused, and a
// storage class change asks for a migration (C-34). The preflight and the
// external-change pause share this judgement, so a refusal and a pause can never
// disagree about which items are irreversible.
func (c *Controller) irreversibleItems(ctx context.Context, application *v1beta1.Application, applied *v1beta1.AppliedState, next *target) ([]v1beta1.IrreversibleItem, []v1beta1.ObjectRef, error) {
	items := dataFormatItem(applied, next)
	volumes, err := volumeItems(ctx, c.volumes, application, applied.Volumes, next.applied.Volumes)
	if err != nil {
		return nil, nil, err
	}
	return append(items, volumes.Irreversible...), volumes.Migrations, nil
}

// dataFormatItem refuses an upgrade that changes the package's data format. KOS
// performs no such upgrade; the user has to migrate outside the product (C-34).
// A declaration that moves in either direction is a change: a package that
// starts declaring a format and one that drops its declaration both move the
// data the running version wrote.
func dataFormatItem(applied *v1beta1.AppliedState, next *target) []v1beta1.IrreversibleItem {
	from := applied.DataFormat
	to := next.applied.DataFormat
	if from == to {
		return nil
	}
	return []v1beta1.IrreversibleItem{{
		Kind:   v1beta1.IrreversibleDataFormat,
		Detail: dataFormatDetail(from, to),
	}}
}

// dataFormatDetail says which direction the declaration moved, because a dropped
// declaration is the same loss of information as a changed one (C-34).
func dataFormatDetail(from, to string) string {
	switch {
	case from == "":
		return fmt.Sprintf("the package starts declaring data format %q; KOS migrates no data", to)
	case to == "":
		return fmt.Sprintf("the package drops its data format declaration of %q; KOS migrates no data", from)
	default:
		return fmt.Sprintf("the package changes the data format from %q to %q; migrate outside KOS and confirm the external change", from, to)
	}
}

// volumeVerdict is what the preflight says about the declared volumes.
type volumeVerdict struct {
	// Irreversible lists the volume items the upgrade may not perform.
	Irreversible []v1beta1.IrreversibleItem
	// Migrations names the migration objects a storage-class change needs.
	Migrations []v1beta1.ObjectRef
}

// volumeItems judges the declared volumes: a declaration the upgrade drops and
// a volume the upgrade shrinks are irreversible, while a storage-class change
// is a migration requirement (C-34). A volume and its claim live in the
// namespace the application declares, because the claim is what its workloads
// mount (C-32, C-38).
func volumeItems(ctx context.Context, source VolumeSource, application *v1beta1.Application, appliedVolumes, desiredVolumes []string) (volumeVerdict, error) {
	verdict := volumeVerdict{}
	namespace := application.Spec.Namespace
	desired := make(map[string]bool, len(desiredVolumes))
	for _, name := range desiredVolumes {
		desired[name] = true
	}
	dropped := make([]string, 0)
	for _, name := range appliedVolumes {
		if !desired[name] {
			dropped = append(dropped, name)
		}
	}
	sort.Strings(dropped)
	for _, name := range dropped {
		ref := v1beta1.ObjectRef{Group: api.Group, Version: "v1beta1", Kind: "Volume", Namespace: namespace, Name: name}
		verdict.Irreversible = append(verdict.Irreversible, v1beta1.IrreversibleItem{
			Kind:   v1beta1.IrreversibleVolumeDelete,
			Object: &ref,
			Detail: "the upgrade drops the volume declaration " + name,
		})
	}
	if source == nil {
		return verdict, nil
	}
	for _, name := range desiredVolumes {
		facts, err := source.VolumeFacts(ctx, namespace, name)
		if err != nil {
			return verdict, err
		}
		if !facts.Found {
			// The volume module has not created the claim yet, so the upgrade
			// cannot shrink or rebind anything (C-38).
			continue
		}
		ref := v1beta1.ObjectRef{Group: api.Group, Version: "v1beta1", Kind: "Volume", Namespace: namespace, Name: name}
		shrinking, undecidable := shrinkVerdict(facts)
		if undecidable != "" {
			verdict.Irreversible = append(verdict.Irreversible, v1beta1.IrreversibleItem{
				Kind:   v1beta1.IrreversibleVolumeShrink,
				Object: &ref,
				Detail: undecidable,
			})
			continue
		}
		if shrinking {
			verdict.Irreversible = append(verdict.Irreversible, v1beta1.IrreversibleItem{
				Kind:   v1beta1.IrreversibleVolumeShrink,
				Object: &ref,
				Detail: fmt.Sprintf("the volume asks for %s but is bound with %s", facts.DesiredSize, facts.ActualSize),
			})
			continue
		}
		// A bound claim that asks for a class other than the one it was bound
		// with needs a migration, not a refusal (C-34). A claim that asks for no
		// class has nothing to compare.
		if facts.Bound && facts.DesiredPolicy != "" && facts.ActualPolicy != facts.DesiredPolicy {
			verdict.Migrations = append(verdict.Migrations, v1beta1.ObjectRef{
				Group: api.Group, Version: "v1beta1", Kind: "VolumeMigration",
				Namespace: namespace, Name: name,
			})
		}
	}
	return verdict, nil
}

// shrinkVerdict judges a bound volume against the size it asks for. The second
// result explains why the bound volume cannot be judged: C-34 refuses an upgrade
// whose storage effect is unknown instead of letting it through.
func shrinkVerdict(facts VolumeFacts) (bool, string) {
	if !facts.Bound {
		return false, ""
	}
	if facts.DesiredSize == "" || facts.ActualSize == "" {
		return false, "the bound volume does not report both its requested and its bound size"
	}
	desired, err := resource.ParseQuantity(facts.DesiredSize)
	if err != nil {
		return false, fmt.Sprintf("the requested size %q cannot be read", facts.DesiredSize)
	}
	actual, err := resource.ParseQuantity(facts.ActualSize)
	if err != nil {
		return false, fmt.Sprintf("the bound size %q cannot be read", facts.ActualSize)
	}
	return desired.Cmp(actual) < 0, ""
}

// configDiff lists the config fields an upgrade rewrites, sorted by field.
func configDiff(applied, desired runtime.RawExtension) ([]v1beta1.ConfigChange, error) {
	from, err := decodeConfigValues(applied)
	if err != nil {
		return nil, err
	}
	to, err := decodeConfigValues(desired)
	if err != nil {
		return nil, err
	}
	fields := make(map[string]bool, len(from)+len(to))
	for field := range from {
		fields[field] = true
	}
	for field := range to {
		fields[field] = true
	}
	names := make([]string, 0, len(fields))
	for field := range fields {
		names = append(names, field)
	}
	sort.Strings(names)
	changes := make([]v1beta1.ConfigChange, 0)
	for _, field := range names {
		before, beforeSet := from[field]
		after, afterSet := to[field]
		if beforeSet && afterSet && valuesEqual(before, after) {
			continue
		}
		changes = append(changes, v1beta1.ConfigChange{
			Field: field,
			From:  encodeValue(before, beforeSet),
			To:    encodeValue(after, afterSet),
		})
	}
	return changes, nil
}

// valuesEqual compares two decoded config values by their encoding.
func valuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

// encodeValue renders one config value as JSON; an absent value stays empty.
func encodeValue(value any, present bool) string {
	if !present {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// manifestDiff lists the manifests an upgrade creates, rewrites or drops (C-34).
func manifestDiff(applied []runtime.RawExtension, desired []*unstructured.Unstructured) ([]v1beta1.RenderChange, error) {
	before, err := decodeManifests(applied)
	if err != nil {
		return nil, err
	}
	previous := make(map[string]*unstructured.Unstructured, len(before))
	for _, object := range before {
		previous[identityOfRef(objectRefOf(object))] = object
	}
	changes := make([]v1beta1.RenderChange, 0)
	written := make(map[string]bool, len(desired))
	for _, object := range desired {
		ref := objectRefOf(object)
		key := identityOfRef(ref)
		written[key] = true
		old, exists := previous[key]
		if !exists {
			changes = append(changes, v1beta1.RenderChange{Object: ref, Operation: v1beta1.RenderCreate})
			continue
		}
		fields := changedPaths(old, object)
		if len(fields) == 0 {
			continue
		}
		changes = append(changes, v1beta1.RenderChange{Object: ref, Operation: v1beta1.RenderUpdate, Fields: fields})
	}
	dropped := make([]string, 0)
	for key := range previous {
		if !written[key] {
			dropped = append(dropped, key)
		}
	}
	sort.Strings(dropped)
	for _, key := range dropped {
		changes = append(changes, v1beta1.RenderChange{Object: objectRefOf(previous[key]), Operation: v1beta1.RenderDelete})
	}
	return changes, nil
}

// changedPaths lists the field paths two manifests differ on, sorted. Only the
// paths the desired manifest carries are judged: a field the server defaults in
// is not a difference the upgrade makes.
func changedPaths(before, after *unstructured.Unstructured) []string {
	left := flattenObject(before)
	right := flattenObject(after)
	changed := make([]string, 0)
	for path, value := range right {
		if other, present := left[path]; !present || other != value {
			changed = append(changed, path)
		}
	}
	for path := range left {
		if _, present := right[path]; !present {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

// ignored paths never take part in a difference: the server owns them and
// writes them without being asked.
var ignoredPaths = map[string]bool{
	"apiVersion":                          true,
	"kind":                                true,
	"status":                              true,
	"metadata.creationTimestamp":          true,
	"metadata.generation":                 true,
	"metadata.resourceVersion":            true,
	"metadata.uid":                        true,
	"metadata.selfLink":                   true,
	"metadata.managedFields":              true,
	"metadata.deletionTimestamp":          true,
	"metadata.deletionGracePeriodSeconds": true,
	"metadata.annotations":                true,
}

// flattenObject renders one object as field path to canonical JSON value. Only
// the paths a desired manifest carries are compared, so a field the server
// defaults in is never read as an external change.
func flattenObject(object *unstructured.Unstructured) map[string]string {
	flat := make(map[string]string)
	flattenValue(object.Object, "", flat)
	for path := range flat {
		if ignoredPaths[path] || hasIgnoredPrefix(path) {
			delete(flat, path)
		}
	}
	return flat
}

// hasIgnoredPrefix reports a path under an ignored root.
func hasIgnoredPrefix(path string) bool {
	for _, root := range []string{"status.", "metadata.managedFields.", "metadata.annotations."} {
		if len(path) > len(root) && path[:len(root)] == root {
			return true
		}
	}
	return false
}

// flattenValue walks one decoded object and records its leaves.
func flattenValue(value any, path string, flat map[string]string) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			flattenValue(typed[key], joinPath(path, key), flat)
		}
	case []any:
		for index, item := range typed {
			flattenValue(item, fmt.Sprintf("%s[%d]", path, index), flat)
		}
	default:
		flat[path] = encodeValue(value, true)
	}
}

// joinPath appends one key to a flattened path.
func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// driftEntries compares the live objects with the manifest set a baseline wrote
// and reports every field KOS owns whose value changed outside KOS (C-35).
func (c *Controller) driftEntries(ctx context.Context, applied *v1beta1.AppliedState) ([]v1beta1.DriftEntry, error) {
	expected, err := decodeManifests(applied.Render)
	if err != nil {
		return nil, err
	}
	sort.Slice(expected, func(left, right int) bool {
		return identityOfRef(objectRefOf(expected[left])) < identityOfRef(objectRefOf(expected[right]))
	})
	entries := make([]v1beta1.DriftEntry, 0)
	for _, object := range expected {
		ref := objectRefOf(object)
		live, err := c.store.Get(ctx, referenceOf(ref))
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, err
			}
			entries = append(entries, v1beta1.DriftEntry{
				Object:   ref,
				Path:     "",
				External: "",
				Expected: "present",
			})
			continue
		}
		desired := flattenObject(object)
		observed := flattenObject(live)
		changed := make([]string, 0)
		for path, value := range desired {
			if other, present := observed[path]; !present || other != value {
				changed = append(changed, path)
			}
		}
		sort.Strings(changed)
		for _, path := range changed {
			entries = append(entries, v1beta1.DriftEntry{
				Object:   ref,
				Path:     path,
				External: observed[path],
				Expected: desired[path],
			})
		}
	}
	return entries, nil
}

// observedRenderDigest digests one manifest set as the cluster actually holds
// it: every object the set names is read, and each field the manifest carries
// takes the value the cluster stores. An object that is missing, or a field
// written differently, therefore digests differently from the manifest it was
// rendered from, which makes this the observation C-35 asks for rather than a
// second render of the same spec.
func (c *Controller) observedRenderDigest(ctx context.Context, objects []*unstructured.Unstructured) (string, error) {
	observed := make([]*unstructured.Unstructured, 0, len(objects))
	for _, object := range objects {
		live, err := c.store.Get(ctx, referenceOf(objectRefOf(object)))
		if apierrors.IsNotFound(err) {
			// The object the target names is not in the cluster, so the observed
			// set cannot be the target set.
			continue
		}
		if err != nil {
			return "", err
		}
		observed = append(observed, observedObject(object, live))
	}
	return RenderDigest(observed)
}

// observedObject rebuilds one manifest from the live object: the shape the
// manifest describes, with the values the cluster holds at those paths.
func observedObject(manifest, live *unstructured.Unstructured) *unstructured.Unstructured {
	rebuilt, _ := observedValue(manifest.Object, live.Object)
	object, _ := rebuilt.(map[string]any)
	return &unstructured.Unstructured{Object: object}
}

// observedValue walks one manifest node and keeps the live value at every leaf.
// Maps keep the manifest's keys, lists keep the manifest's shape, and a node the
// live object no longer carries is dropped.
func observedValue(manifest, live any) (any, bool) {
	switch typed := manifest.(type) {
	case map[string]any:
		liveMap, ok := live.(map[string]any)
		if !ok {
			return nil, false
		}
		rebuilt := make(map[string]any, len(typed))
		for key, value := range typed {
			other, present := liveMap[key]
			if !present {
				continue
			}
			if kept, ok := observedValue(value, other); ok {
				rebuilt[key] = kept
			}
		}
		return rebuilt, true
	case []any:
		liveList, ok := live.([]any)
		if !ok {
			return nil, false
		}
		rebuilt := make([]any, 0, len(typed))
		for index, value := range typed {
			if index >= len(liveList) {
				continue
			}
			if kept, ok := observedValue(value, liveList[index]); ok {
				rebuilt = append(rebuilt, kept)
			}
		}
		return rebuilt, true
	default:
		return live, true
	}
}

// driftedObjects names the objects whose external values a policy keeps, so a
// round does not write them back (C-35).
func driftedObjects(entries []v1beta1.DriftEntry) map[string]bool {
	objects := make(map[string]bool, len(entries))
	for _, entry := range entries {
		objects[identityOfRef(entry.Object)] = true
	}
	return objects
}

// configPaths lists the manifest paths a config value can move, keyed by object
// identity and path. A difference on a path outside this set cannot be written
// back through the user's config, so the adopt policy refuses to record it
// (C-35).
func configPaths(spec v1beta1.ApplicationDefinitionSpec, input RenderInput, config map[string]any) (map[string]bool, error) {
	base, err := Render(spec, input)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]bool)
	for _, field := range spec.Config {
		probe := make(map[string]any, len(config)+1)
		for key, value := range config {
			probe[key] = value
		}
		probe[field.Name] = probeValue(field, config[field.Name])
		probedInput := input
		probedInput.Config = probe
		objects, err := Render(spec, probedInput)
		if err != nil {
			return nil, err
		}
		probed := make(map[string]*unstructured.Unstructured, len(objects))
		for _, object := range objects {
			probed[identityOfRef(objectRefOf(object))] = object
		}
		for _, object := range base {
			other, exists := probed[identityOfRef(objectRefOf(object))]
			if !exists {
				continue
			}
			key := identityOfRef(objectRefOf(object))
			for _, path := range changedPaths(object, other) {
				paths[key+"|"+path] = true
			}
		}
	}
	return paths, nil
}

// probeValue picks a value no configuration is likely to hold for one field, so
// a render with it shows which paths the field moves (C-35).
func probeValue(field v1beta1.ConfigField, current any) any {
	switch field.Type {
	case TypeInteger:
		return int64(987654321)
	case TypeNumber:
		return float64(987654321.5)
	case TypeBoolean:
		if value, ok := current.(bool); ok {
			return !value
		}
		return true
	default:
		return "kos-drift-probe"
	}
}

// recordableDrift returns the drift entries the adopt policy may record. A set
// the config structure cannot express is refused whole, so the drift condition
// stays without a record that could not be acted on (C-35).
func recordableDrift(entries []v1beta1.DriftEntry, expressible map[string]bool) ([]v1beta1.DriftEntry, bool) {
	if len(entries) == 0 {
		return nil, false
	}
	for _, entry := range entries {
		if !expressible[identityOfRef(entry.Object)+"|"+entry.Path] {
			return nil, false
		}
	}
	return entries, true
}
