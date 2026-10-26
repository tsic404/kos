package controlplane

import (
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// kernelTypeOf resolves a kernel kind for a decode case.
func kernelTypeOf(t *testing.T, kind string) ObjectType {
	t.Helper()
	objectType, ok := kernelType(kind)
	if !ok {
		t.Fatalf("kind %q is not a kernel kind", kind)
	}
	return objectType
}

// applicationManifest builds a submitted application body.
func applicationManifest(mutate func(map[string]any)) map[string]any {
	manifest := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      "web",
			"namespace": "default",
			"uid":       "uid-of-older-read",
			"labels":    map[string]any{"managed-by": "kos-shell"},
		},
		"spec": map[string]any{},
	}
	if mutate != nil {
		mutate(manifest)
	}
	return manifest
}

// TestDecodeObjectKeepsClientOwnedFields checks that a submitted object becomes
// the desired state without server-assigned metadata (C-03).
func TestDecodeObjectKeepsClientOwnedFields(t *testing.T) {
	decoded, failure := decodeObject(kernelTypeOf(t, "Application"), "default", applicationManifest(nil))
	if failure != nil {
		t.Fatalf("decode object: %v", failure)
	}
	if decoded.Ref != (ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "web"}) {
		t.Fatalf("ref = %v", decoded.Ref)
	}
	metadata := decoded.Object.Object["metadata"].(map[string]any)
	if _, present := metadata["uid"]; present {
		t.Fatal("the desired state must not carry a uid")
	}
	if _, present := metadata["resourceVersion"]; present {
		t.Fatal("the desired state must not carry a resourceVersion")
	}
	if metadata["name"] != "web" || metadata["namespace"] != "default" {
		t.Fatalf("identity = %v", metadata)
	}
	if _, present := decoded.Object.Object["status"]; present {
		t.Fatal("the desired state must not carry a status")
	}
}

// TestDecodeObjectRejectsFieldsOutsideTheModel checks that a body the typed
// model does not declare fails instead of being written and pruned (C-01).
func TestDecodeObjectRejectsFieldsOutsideTheModel(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		manifest map[string]any
	}{
		{
			name: "unknown spec field",
			kind: "Application",
			manifest: applicationManifest(func(manifest map[string]any) {
				manifest["spec"] = map[string]any{"replicas": 3}
			}),
		},
		{
			name: "client-written status",
			kind: "Application",
			manifest: applicationManifest(func(manifest map[string]any) {
				manifest["status"] = map[string]any{"phase": "Running"}
			}),
		},
		{
			name:     "body kind differs from the path",
			kind:     "Volume",
			manifest: applicationManifest(nil),
		},
		{
			name: "namespace on a cluster-scoped kind",
			kind: "Provider",
			manifest: map[string]any{
				"apiVersion": "kos.tsic.top/v1beta1",
				"kind":       "Provider",
				"metadata":   map[string]any{"name": "network", "namespace": "default"},
			},
		},
		{
			name: "missing name",
			kind: "Application",
			manifest: applicationManifest(func(manifest map[string]any) {
				delete(manifest["metadata"].(map[string]any), "name")
			}),
		},
		{
			name: "missing namespace",
			kind: "Application",
			manifest: applicationManifest(func(manifest map[string]any) {
				delete(manifest["metadata"].(map[string]any), "namespace")
			}),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, failure := decodeObject(kernelTypeOf(t, testCase.kind), "", testCase.manifest); failure == nil {
				t.Fatal("the body must be rejected")
			} else if failure.Code != CodeInvalidField {
				t.Fatalf("code = %s, want %s", failure.Code, CodeInvalidField)
			}
		})
	}
}

// TestDecodeObjectReportsMissingExtension checks that a kind of another group is
// a missing capability, not a malformed request (C-19, F16).
func TestDecodeObjectReportsMissingExtension(t *testing.T) {
	_, failure := resolveType("Widget", "example.test/v1", nil)
	if failure == nil {
		t.Fatal("a kind of an uninstalled extension must not be served")
	}
	if failure.Code != CodeCapabilityMissing || failure.Stage != StageCapability {
		t.Fatalf("code = %s, stage = %s", failure.Code, failure.Stage)
	}
	if failure.Object == nil || failure.Object.Group != "example.test" || failure.Object.Kind != "Widget" {
		t.Fatalf("object reference = %v", failure.Object)
	}
}

// extensionKinds serves one declared class for the resolver cases.
type extensionKinds map[schema.GroupKind]ExtensionObjectClass

// ObjectClass implements ExtensionKinds.
func (k extensionKinds) ObjectClass(group, kind string) (ExtensionObjectClass, bool) {
	class, ok := k[schema.GroupKind{Group: group, Kind: kind}]
	return class, ok
}

// TestResolveTypeServesOnlyDeclaredClasses checks that an active declaration
// serves its class and an undeclared class stays a missing capability (C-19).
func TestResolveTypeServesOnlyDeclaredClasses(t *testing.T) {
	kinds := extensionKinds{
		{Group: "widgets.example.test", Kind: "Widget"}: {
			Versions: []string{"v1"}, Resource: "widgets", Namespaced: true,
		},
	}
	served, failure := resolveType("Widget", "widgets.example.test/v1", kinds)
	if failure != nil {
		t.Fatalf("resolve served class: %v", failure)
	}
	if served.Resource != "widgets" || !served.Namespaced || served.Version != "v1" {
		t.Fatalf("served type = %+v", served)
	}
	if _, failure := resolveType("Widget", "widgets.example.test/v2", kinds); failure == nil ||
		failure.Code != CodeCapabilityMissing {
		t.Fatalf("an unserved version resolved: %v", failure)
	}
	if _, failure := resolveType("Gadget", "widgets.example.test/v1", kinds); failure == nil ||
		failure.Code != CodeCapabilityMissing {
		t.Fatalf("an undeclared class resolved: %v", failure)
	}
	if _, failure := resolveType("Widget", "", kinds); failure == nil ||
		failure.Code != CodeInvalidField {
		t.Fatalf("a class without an API group resolved: %v", failure)
	}
}

// TestDecodeExtensionObjectKeepsTheExtensionSpec checks that an extension
// object's spec passes through untouched while identity rules still apply
// (C-19).
func TestDecodeExtensionObjectKeepsTheExtensionSpec(t *testing.T) {
	objectType := ObjectType{
		Group: "widgets.example.test", Version: "v1", Kind: "Widget",
		Resource: "widgets", Namespaced: true,
	}
	manifest := map[string]any{
		"apiVersion": "widgets.example.test/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": "alpha", "namespace": "default"},
		"spec":       map[string]any{"size": int64(3), "nested": map[string]any{"enabled": true}},
	}
	decoded, failure := decodeObject(objectType, "default", manifest)
	if failure != nil {
		t.Fatalf("decode extension object: %v", failure)
	}
	if decoded.Ref != (ObjectRef{Group: "widgets.example.test", Kind: "Widget", Namespace: "default", Name: "alpha"}) {
		t.Fatalf("ref = %v", decoded.Ref)
	}
	spec, _, err := unstructured.NestedMap(decoded.Object.Object, "spec")
	if err != nil || spec["size"] != int64(3) {
		t.Fatalf("spec = %v (err %v)", spec, err)
	}
	if decoded.Object.Object["apiVersion"] != "widgets.example.test/v1" {
		t.Fatalf("apiVersion = %v", decoded.Object.Object["apiVersion"])
	}
}

// TestChangesNamesMovedFields checks the diff a plan reports (C-62).
func TestChangesNamesMovedFields(t *testing.T) {
	current := map[string]any{
		"metadata": map[string]any{
			"labels":      map[string]any{"managed-by": "kos-shell", "tier": "bronze"},
			"annotations": map[string]any{"example.test/note": "kept"},
		},
		"spec": map[string]any{},
	}
	desired := map[string]any{
		"metadata": map[string]any{
			"labels":      map[string]any{"managed-by": "kos-shell", "tier": "gold"},
			"annotations": map[string]any{"example.test/note": "kept", "example.test/release": "v2"},
		},
		"spec": map[string]any{},
	}

	differences := changes(current, desired)
	paths := make([]string, 0, len(differences))
	for _, difference := range differences {
		paths = append(paths, difference.Path)
	}
	sort.Strings(paths)
	want := []string{"metadata.annotations.example.test/release", "metadata.labels.tier"}
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("changed paths = %v, want %v", paths, want)
	}
	for _, difference := range differences {
		if difference.Path == "metadata.labels.tier" && (difference.From != "bronze" || difference.To != "gold") {
			t.Fatalf("tier change = %v", difference)
		}
		if difference.Path == "metadata.annotations.example.test/release" && (difference.From != nil || difference.To != "v2") {
			t.Fatalf("added annotation change = %v", difference)
		}
	}

	if len(changes(current, current)) != 0 {
		t.Fatal("identical desired state must produce no change")
	}
}
