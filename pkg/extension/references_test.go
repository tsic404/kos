package extension

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/write"
)

// TestReferencesTargetFollowsDeclaredPaths checks C-19's reference reading: only
// declared paths count, a single reference and a list both resolve, an omitted
// namespace names the referring object's namespace, and another object is left
// alone.
func TestReferencesTargetFollowsDeclaredPaths(t *testing.T) {
	volume := write.Ref{
		Group: "kos.tsic.top", Version: "v1beta1", Kind: "Volume",
		Namespace: "vms", Name: "data",
	}
	paths := []string{"spec.volumeRef", "spec.attachments"}
	object := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "vm-one", "namespace": "vms"},
		"spec": map[string]any{
			"volumeRef": map[string]any{
				"apiVersion": "kos.tsic.top/v1beta1", "kind": "Volume", "name": "data",
			},
			"attachments": []any{
				map[string]any{"apiVersion": "kos.tsic.top/v1beta1", "kind": "Volume", "namespace": "other", "name": "data"},
			},
			"unrelated": map[string]any{"kind": "Volume", "name": "data"},
		},
	}}
	if !referencesTarget(object, paths, volume) {
		t.Fatal("a reference at a declared path was not resolved")
	}
	if referencesTarget(object, []string{"spec.unrelated"}, volume) {
		t.Fatal("an undeclared path produced a reference")
	}
	if referencesTarget(object, paths, write.Ref{
		Group: "kos.tsic.top", Version: "v1beta1", Kind: "Volume", Namespace: "vms", Name: "other",
	}) {
		t.Fatal("a reference to another object matched")
	}

	// An omitted namespace is the referring object's namespace.
	if referencesTarget(object, paths, write.Ref{
		Group: "kos.tsic.top", Version: "v1beta1", Kind: "Volume", Namespace: "default", Name: "data",
	}) {
		t.Fatal("the default namespace matched a reference in another namespace")
	}
}

// TestRetentionForMatchesAClass checks C-29's declaration lookup: the class key,
// kind and resource all match, an undeclared class is not kept, and the
// class-wide entry covers every class.
func TestRetentionForMatchesAClass(t *testing.T) {
	class := ObjectClass{Group: "vm.example.test", Kind: "VirtualMachine", Resource: "virtualmachines"}
	for _, resource := range []string{"vm.example.test/VirtualMachine", "VirtualMachine", "virtualmachines"} {
		keep, external, found := retentionFor([]Retention{{Resource: resource, Keep: true}}, class)
		if !found || !keep || external != "" {
			t.Fatalf("retention %q = keep %v external %q found %v", resource, keep, external, found)
		}
	}
	if keep, _, found := retentionFor(nil, class); found || keep {
		t.Fatal("an undeclared class was kept")
	}
	if keep, external, found := retentionFor([]Retention{{Resource: ClassObjectClasses, ExternalReference: "s3://bucket"}}, class); !found || keep || external != "s3://bucket" {
		t.Fatalf("class-wide external entry = keep %v external %q found %v", keep, external, found)
	}
}
