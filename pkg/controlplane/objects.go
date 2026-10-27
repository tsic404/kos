package controlplane

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// kernelScheme resolves a kind to its typed object so a submitted manifest can
// be checked against the model instead of being written as free-form JSON.
var kernelScheme = api.Scheme()

// desiredObject is the state a write converges the cluster to: identity plus the
// fields a client owns. Status and server-assigned metadata are never part of
// it (C-03).
type desiredObject struct {
	Ref    ObjectRef
	Type   ObjectType
	Object *unstructured.Unstructured
}

// decodeObject validates one submitted manifest against the class it belongs to
// and returns the desired state. A kernel manifest is checked against the closed
// typed model; an extension object's spec belongs to the extension, so only the
// kernel-side identity rules are applied to it (C-01, C-19).
func decodeObject(objectType ObjectType, namespace string, raw map[string]any) (desiredObject, *Error) {
	if bodyKind := text(raw["kind"]); bodyKind != "" && bodyKind != objectType.Kind {
		return desiredObject{}, invalidField("请求体的 kind " + bodyKind + " 与请求路径的 kind " + objectType.Kind + " 不一致")
	}
	ref := ObjectRef{Group: objectType.Group, Kind: objectType.Kind, Name: nestedText(raw, "metadata", "name")}
	if apiVersion := text(raw["apiVersion"]); apiVersion != "" {
		group, version, ok := splitAPIVersion(apiVersion)
		if !ok {
			return desiredObject{}, invalidField("apiVersion 必须是 group/version 形式").about(ref)
		}
		if group != objectType.Group || (!objectType.IsKernel() && version != objectType.Version) {
			return desiredObject{}, invalidField("请求体的 apiVersion 与请求路径的对象类不一致").about(ref)
		}
	}
	bodyNamespace := nestedText(raw, "metadata", "namespace")
	if !objectType.Namespaced {
		if bodyNamespace != "" || namespace != "" {
			return desiredObject{}, invalidField("集群作用域对象不能带 namespace").about(ref)
		}
	} else {
		if bodyNamespace != "" && namespace != "" && bodyNamespace != namespace {
			return desiredObject{}, invalidField("metadata.namespace 与请求参数 namespace 不一致").about(ref)
		}
		if bodyNamespace == "" {
			bodyNamespace = namespace
		}
		if bodyNamespace == "" {
			return desiredObject{}, invalidField("命名空间作用域对象必须给出 metadata.namespace").about(ref)
		}
	}
	ref.Namespace = bodyNamespace
	if ref.Name == "" {
		return desiredObject{}, invalidField("对象必须给出 metadata.name").about(ref)
	}
	if problems := validation.IsDNS1123Subdomain(ref.Name); len(problems) > 0 {
		return desiredObject{}, invalidField("metadata.name 不是合法的对象名：" + problems[0]).about(ref)
	}
	if status, ok := raw["status"]; ok && !emptyValue(status) {
		return desiredObject{}, invalidField("status 由调和器写入，请求不能设置").about(ref)
	}

	if !objectType.IsKernel() {
		return desiredObject{Ref: ref, Type: objectType, Object: normalizeExtension(raw, objectType, ref.Namespace, ref.Name)}, nil
	}
	typed, err := kernelScheme.New(schema.GroupVersionKind{
		Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: objectType.Kind,
	})
	if err != nil {
		return desiredObject{}, internal(StageValidate, "无法解析对象类型 "+objectType.Kind).about(ref)
	}
	if failure := decodeStrict(raw, typed); failure != nil {
		return desiredObject{}, failure.about(ref)
	}
	return desiredObject{Ref: ref, Type: objectType, Object: normalize(typed, objectType.Kind, ref.Namespace, ref.Name)}, nil
}

// normalizeExtension keeps the client-owned part of an extension object: the
// extension defines its spec, so the kernel checks identity only (C-19).
func normalizeExtension(raw map[string]any, objectType ObjectType, namespace, name string) *unstructured.Unstructured {
	metadata := map[string]any{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	if source, ok := raw["metadata"].(map[string]any); ok {
		copyKey(metadata, source, "labels")
		copyKey(metadata, source, "annotations")
	}
	object := map[string]any{
		"apiVersion": objectType.Group + "/" + objectType.Version,
		"kind":       objectType.Kind,
		"metadata":   metadata,
	}
	if spec, ok := raw["spec"]; ok {
		object["spec"] = spec
	}
	return &unstructured.Unstructured{Object: object}
}

// decodeStrict decodes a manifest into its typed object, rejecting fields the
// model does not define.
func decodeStrict(raw map[string]any, typed runtime.Object) *Error {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return invalid("请求体不是合法对象：" + err.Error())
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(typed); err != nil {
		message := err.Error()
		if field, found := strings.CutPrefix(message, "json: unknown field "); found {
			return invalidField("对象含模型未定义的字段 " + field)
		}
		return invalidField("对象字段不符合模型定义：" + message)
	}
	return nil
}

// withCreationAttribution stamps the managed-by marker the creator writes
// (C-02). Only a create calls it: the control plane is the creator of every
// object it creates, and without the marker no later KOS subject may write the
// object's status or finalizers (C-03).
func withCreationAttribution(object desiredObject) desiredObject {
	if object.Object.GetLabels()[markers.ManagedBy] != "" {
		return object
	}
	labels := object.Object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[markers.ManagedBy] = write.ManagedByValue
	object.Object.SetLabels(labels)
	return object
}

// withStoredAttribution keeps the attribution and decline markers an existing
// object already carries (C-02): an update neither drops a marker the body omits
// nor rewrites one it names. Only a key the body leaves out is filled back, and
// the result must still describe the stored object's attribution, so a plain
// write cannot forge an adoption record or grant a marker.
func withStoredAttribution(object desiredObject, current map[string]any) (desiredObject, *Error) {
	for _, key := range markers.Keys() {
		class, isMarker := markers.ClassOf(key)
		if !isMarker || (class != markers.ClassAttribution && class != markers.ClassDecline) {
			continue
		}
		stored := nestedText(current, "metadata", carrierField(key), key)
		if stored == "" || markerOf(object.Object, key) != "" {
			continue
		}
		setMarker(object.Object, key, stored)
	}
	if failure := checkStoredAttribution(object.Object, current); failure != nil {
		return desiredObject{}, failure.about(object.Ref)
	}
	return object, nil
}

// carrierField names the metadata field a marker key lives on.
func carrierField(key string) string {
	if carrier, _ := markers.CarrierOf(key); carrier == markers.CarrierAnnotation {
		return "annotations"
	}
	return "labels"
}

// markerOf reads one marker from its carrier on a desired object.
func markerOf(object *unstructured.Unstructured, key string) string {
	if carrierField(key) == "annotations" {
		return object.GetAnnotations()[key]
	}
	return object.GetLabels()[key]
}

// setMarker writes one marker on its carrier, allocating the map on first use.
func setMarker(object *unstructured.Unstructured, key, value string) {
	if carrierField(key) == "annotations" {
		annotations := object.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[key] = value
		object.SetAnnotations(annotations)
		return
	}
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[key] = value
	object.SetLabels(labels)
}

// checkStoredAttribution refuses a write whose markers would escape C-02: the
// result must satisfy the marker rules and agree with the stored attribution key
// by key. A body that repeats the stored markers unchanged still passes.
func checkStoredAttribution(object *unstructured.Unstructured, current map[string]any) *Error {
	if err := markers.Validate(object.GetLabels(), object.GetAnnotations()); err != nil {
		return invalidField("标记不满足归属规则：" + err.Error())
	}
	attribution, err := markers.AttributionOf(object.GetLabels())
	if err != nil {
		return invalidField("归属不可判定：" + err.Error())
	}
	storedAttribution, err := markers.AttributionOf(storedMarkerLabels(current))
	if err != nil {
		return invalidField("存储态归属不可判定：" + err.Error())
	}
	if attribution != storedAttribution {
		return invalidField("一次更新不能改变对象的归属状态")
	}
	for _, key := range markers.Keys() {
		class, isMarker := markers.ClassOf(key)
		if !isMarker || (class != markers.ClassAttribution && class != markers.ClassDecline) {
			continue
		}
		if markerOf(object, key) != nestedText(current, "metadata", carrierField(key), key) {
			return invalidField("一次更新不能改写标记 " + key)
		}
	}
	return nil
}

// storedMarkerLabels reads the stored marker labels that decide attribution.
func storedMarkerLabels(current map[string]any) map[string]string {
	labels := map[string]string{}
	for _, key := range markers.Keys() {
		if carrierField(key) == "annotations" {
			continue
		}
		if value := nestedText(current, "metadata", "labels", key); value != "" {
			labels[key] = value
		}
	}
	return labels
}

// normalize keeps the client-owned part of a decoded object: identity, labels,
// annotations and spec. Everything else is assigned by the server (C-03).
func normalize(typed runtime.Object, kind, namespace, name string) *unstructured.Unstructured {
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(typed)
	if err != nil {
		return &unstructured.Unstructured{Object: map[string]any{}}
	}
	metadata := map[string]any{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	if source, ok := encoded["metadata"].(map[string]any); ok {
		copyKey(metadata, source, "labels")
		copyKey(metadata, source, "annotations")
	}
	object := map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       kind,
		"metadata":   metadata,
	}
	if spec, ok := encoded["spec"]; ok {
		object["spec"] = spec
	}
	return &unstructured.Unstructured{Object: object}
}

// copyKey copies a non-empty map field from source to target.
func copyKey(target, source map[string]any, key string) {
	value, ok := source[key]
	if !ok || emptyValue(value) {
		return
	}
	target[key] = value
}

// changes lists every field a write would rewrite, comparing the client-owned
// part of the current object with the desired state (C-62).
func changes(current, desired map[string]any) []Change {
	before := comparable(current)
	after := comparable(desired)
	paths := make([]string, 0, len(before)+len(after))
	for path := range before {
		paths = append(paths, path)
	}
	for path := range after {
		if _, seen := before[path]; !seen {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	differences := make([]Change, 0, len(paths))
	for _, path := range paths {
		from, beforeSet := before[path]
		to, afterSet := after[path]
		if beforeSet && afterSet && equality.Semantic.DeepEqual(from, to) {
			continue
		}
		difference := Change{Path: path}
		if beforeSet {
			difference.From = from
		}
		if afterSet {
			difference.To = to
		}
		differences = append(differences, difference)
	}
	return differences
}

// comparable flattens the client-owned groups of an object into path to value
// entries, so a diff names the field that moved.
func comparable(object map[string]any) map[string]any {
	fields := map[string]any{}
	metadata, _ := object["metadata"].(map[string]any)
	flatten("spec", object["spec"], fields)
	flatten("metadata.labels", metadata["labels"], fields)
	flatten("metadata.annotations", metadata["annotations"], fields)
	return fields
}

// flatten records every leaf below path; an empty container records nothing, so
// removing the last label reads as a field removal.
func flatten(path string, value any, fields map[string]any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			flatten(path+"."+key, child, fields)
		}
	case nil:
		return
	default:
		fields[path] = typed
	}
}

// text reads a value as a string, treating a missing or non-string value as
// empty.
func text(value any) string {
	parsed, _ := value.(string)
	return parsed
}

// nestedText reads a string below a chain of map keys.
func nestedText(object map[string]any, path ...string) string {
	var value any = object
	for _, key := range path {
		parent, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = parent[key]
	}
	return text(value)
}

// emptyValue reports a missing, null or empty container value.
func emptyValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case map[string]any:
		return len(typed) == 0
	case []any:
		return len(typed) == 0
	default:
		return false
	}
}
