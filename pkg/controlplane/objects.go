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
)

// kernelScheme resolves a kind to its typed object so a submitted manifest can
// be checked against the model instead of being written as free-form JSON.
var kernelScheme = api.Scheme()

// desiredObject is the state a write converges the cluster to: identity plus the
// fields a client owns. Status and server-assigned metadata are never part of
// it (C-03).
type desiredObject struct {
	Ref    ObjectRef
	Object *unstructured.Unstructured
}

// decodeObject validates one submitted manifest against the closed kernel model
// and returns the desired state. A field the typed model does not declare is
// rejected rather than written and pruned, so a client learns about the mistake
// instead of seeing it disappear (C-01, C-12).
func decodeObject(kind, namespace string, raw map[string]any) (desiredObject, *Error) {
	if bodyKind := text(raw["kind"]); bodyKind != "" && bodyKind != kind {
		return desiredObject{}, invalidField("请求体的 kind " + bodyKind + " 与请求路径的 kind " + kind + " 不一致")
	}
	group := api.Group
	if apiVersion := text(raw["apiVersion"]); apiVersion != "" {
		if parsed, _, ok := splitAPIVersion(apiVersion); ok {
			group = parsed
		}
	}
	ref := ObjectRef{Group: group, Kind: kind, Name: nestedText(raw, "metadata", "name")}
	if failure := resolveKind(kind, text(raw["apiVersion"])); failure != nil {
		return desiredObject{}, failure.about(ref)
	}
	scope, _ := api.ScopeOf(kind)
	bodyNamespace := nestedText(raw, "metadata", "namespace")
	if scope == api.ScopeCluster {
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
	ref = kernelRef(kind, bodyNamespace, ref.Name)
	if ref.Name == "" {
		return desiredObject{}, invalidField("对象必须给出 metadata.name").about(ref)
	}
	if problems := validation.IsDNS1123Subdomain(ref.Name); len(problems) > 0 {
		return desiredObject{}, invalidField("metadata.name 不是合法的对象名：" + problems[0]).about(ref)
	}
	if status, ok := raw["status"]; ok && !emptyValue(status) {
		return desiredObject{}, invalidField("status 由调和器写入，请求不能设置").about(ref)
	}

	typed, err := kernelScheme.New(schema.GroupVersionKind{
		Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: kind,
	})
	if err != nil {
		return desiredObject{}, internal(StageValidate, "无法解析对象类型 "+kind).about(ref)
	}
	if failure := decodeStrict(raw, typed); failure != nil {
		return desiredObject{}, failure.about(ref)
	}
	return desiredObject{Ref: ref, Object: normalize(typed, kind, ref.Namespace, ref.Name)}, nil
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
