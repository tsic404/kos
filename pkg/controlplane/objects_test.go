package controlplane

import (
	"sort"
	"testing"
)

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
	decoded, failure := decodeObject("Application", "default", applicationManifest(nil))
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
			if _, failure := decodeObject(testCase.kind, "", testCase.manifest); failure == nil {
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
	manifest := applicationManifest(func(manifest map[string]any) {
		manifest["apiVersion"] = "example.test/v1"
		manifest["kind"] = "Widget"
	})
	_, failure := decodeObject("Widget", "", manifest)
	if failure == nil {
		t.Fatal("a kind of an uninstalled extension must not be served")
	}
	if failure.Code != CodeCapabilityMissing || failure.Stage != StageCapability {
		t.Fatalf("code = %s, stage = %s", failure.Code, failure.Stage)
	}
	if failure.Object == nil || failure.Object.Group != "example.test" || failure.Object.Name != "web" {
		t.Fatalf("object reference = %v", failure.Object)
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
