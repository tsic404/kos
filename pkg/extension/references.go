package extension

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/write"
)

// ReferenceGate reports the extension objects that declare a reference to one
// kernel object (C-19). The volume reconciler asks it before releasing a
// deleting volume; a nil registry reports nothing.
type ReferenceGate struct {
	Dynamic  dynamic.Interface
	Registry *Registry
}

// Referrers implements the reference gate.
func (g ReferenceGate) Referrers(ctx context.Context, target write.Ref) ([]v1beta1.ObjectRef, error) {
	if g.Dynamic == nil || g.Registry == nil {
		return nil, nil
	}
	return Referrers(ctx, g.Dynamic, g.Registry, target)
}

// Referrers lists every served extension object whose declared reference paths
// name target (C-19). The kernel follows only the paths a class declares.
func Referrers(ctx context.Context, client dynamic.Interface, registry *Registry, target write.Ref) ([]v1beta1.ObjectRef, error) {
	if client == nil || registry == nil {
		return nil, nil
	}
	var referrers []v1beta1.ObjectRef
	for _, class := range registry.ObjectClasses() {
		if len(class.References) == 0 {
			continue
		}
		objects, err := listClassObjects(ctx, client, class)
		if err != nil {
			return nil, err
		}
		for _, object := range objects {
			if !referencesTarget(object, class.References, target) {
				continue
			}
			referrers = append(referrers, objectRefOf(object, class))
		}
	}
	sort.SliceStable(referrers, func(left, right int) bool {
		return objectRefKey(referrers[left]) < objectRefKey(referrers[right])
	})
	return referrers, nil
}

// reference is one parsed declarative reference.
type reference struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

// referencesTarget reports whether one object names target at any declared
// path.
func referencesTarget(object *unstructured.Unstructured, paths []string, target write.Ref) bool {
	for _, path := range paths {
		value, found, err := unstructured.NestedFieldNoCopy(object.Object, strings.Split(path, ".")...)
		if err != nil || !found {
			continue
		}
		for _, candidate := range decodeReferences(value) {
			if referenceMatches(candidate, object.GetNamespace(), target) {
				return true
			}
		}
	}
	return false
}

// decodeReferences reads a reference value: one reference object or a list of
// them. Anything else is not a reference and contributes no edge.
func decodeReferences(value any) []reference {
	switch typed := value.(type) {
	case map[string]any:
		if parsed, ok := decodeReference(typed); ok {
			return []reference{parsed}
		}
	case []any:
		references := make([]reference, 0, len(typed))
		for _, item := range typed {
			references = append(references, decodeReferences(item)...)
		}
		return references
	}
	return nil
}

// decodeReference reads the object reference shape extension objects carry:
// apiVersion (or group), kind, namespace and name.
func decodeReference(value map[string]any) (reference, bool) {
	kind, _ := value["kind"].(string)
	name, _ := value["name"].(string)
	if kind == "" || name == "" {
		return reference{}, false
	}
	group, _ := value["group"].(string)
	if group == "" {
		if apiVersion, ok := value["apiVersion"].(string); ok {
			if prefix, _, found := strings.Cut(apiVersion, "/"); found {
				group = prefix
			}
		}
	}
	namespace, _ := value["namespace"].(string)
	return reference{Group: group, Kind: kind, Namespace: namespace, Name: name}, true
}

// referenceMatches compares one reference with the target object. A reference
// that omits the namespace names the referring object's namespace.
func referenceMatches(candidate reference, objectNamespace string, target write.Ref) bool {
	if candidate.Group != target.Group || candidate.Kind != target.Kind || candidate.Name != target.Name {
		return false
	}
	namespace := candidate.Namespace
	if namespace == "" {
		namespace = objectNamespace
	}
	return namespace == target.Namespace
}

// objectRefOf renders one extension object as a status reference.
func objectRefOf(object *unstructured.Unstructured, class ObjectClass) v1beta1.ObjectRef {
	version := ""
	if len(class.Versions) > 0 {
		version = class.Versions[0]
	}
	return v1beta1.ObjectRef{
		Group:     class.Group,
		Version:   version,
		Kind:      class.Kind,
		Namespace: object.GetNamespace(),
		Name:      object.GetName(),
	}
}

// objectRefKey is the stable sort key of a reference.
func objectRefKey(ref v1beta1.ObjectRef) string {
	return ref.Group + "/" + ref.Kind + "/" + ref.Namespace + "/" + ref.Name
}

// listClassObjects lists every instance of one declared class across
// namespaces.
func listClassObjects(ctx context.Context, client dynamic.Interface, class ObjectClass) ([]*unstructured.Unstructured, error) {
	version := ""
	if len(class.Versions) > 0 {
		version = class.Versions[0]
	}
	resource := client.Resource(schema.GroupVersionResource{
		Group: class.Group, Version: version, Resource: class.Resource,
	})
	var (
		list *unstructured.UnstructuredList
		err  error
	)
	if class.Namespaced {
		list, err = resource.Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	} else {
		list, err = resource.List(ctx, metav1.ListOptions{})
	}
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s/%s: %w", class.Group, class.Kind, err)
	}
	objects := make([]*unstructured.Unstructured, 0, len(list.Items))
	for index := range list.Items {
		objects = append(objects, &list.Items[index])
	}
	return objects, nil
}
