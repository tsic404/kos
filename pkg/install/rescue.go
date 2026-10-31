package install

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

// Rescue is the read-only rescue mode of C-54: it reads native resources
// straight from the API server, skipping capability checks and caches. The type
// holds no writer and starts no reconciler, so "read only" is structural rather
// than a flag a caller could flip.
type Rescue struct {
	dynamic   dynamic.Interface
	discovery discovery.DiscoveryInterface
}

// NewRescue builds the rescue reader.
func NewRescue(client dynamic.Interface, discoveryClient discovery.DiscoveryInterface) (*Rescue, error) {
	if client == nil || discoveryClient == nil {
		return nil, fmt.Errorf("rescue mode needs a dynamic client and discovery")
	}
	return &Rescue{dynamic: client, discovery: discoveryClient}, nil
}

// NativeView is one native resource and the objects the server holds for it.
type NativeView struct {
	// Resource is the resource that was read.
	Resource schema.GroupVersionResource
	// Objects are what the API server returned; they are not projected through
	// any KOS cache or status writer.
	Objects []*unstructured.Unstructured
}

// Native projects every listable resource the API server serves. Callers filter
// the view; the rescue path itself never narrows the truth.
func (r *Rescue) Native(ctx context.Context) ([]NativeView, error) {
	lists, err := r.discovery.ServerPreferredResources()
	if err != nil {
		return nil, fmt.Errorf("discover native resources: %w", err)
	}
	views := make([]NativeView, 0, len(lists))
	for _, list := range lists {
		groupVersion, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			return nil, fmt.Errorf("parse discovered group version %q: %w", list.GroupVersion, err)
		}
		for _, resource := range list.APIResources {
			if !listable(resource) {
				continue
			}
			objects, err := r.list(ctx, groupVersion, resource)
			if err != nil {
				return nil, fmt.Errorf("read %s/%s: %w", list.GroupVersion, resource.Name, err)
			}
			views = append(views, NativeView{
				Resource: schema.GroupVersionResource{
					Group: groupVersion.Group, Version: groupVersion.Version, Resource: resource.Name,
				},
				Objects: objects,
			})
		}
	}
	return views, nil
}

// List reads one resource in one namespace; empty means every namespace, and a
// cluster-scoped resource ignores it. It is the query path of rescue mode.
func (r *Rescue) List(ctx context.Context, resource schema.GroupVersionResource, namespace string) ([]*unstructured.Unstructured, error) {
	var lister dynamic.ResourceInterface = r.dynamic.Resource(resource)
	if namespace != "" {
		lister = r.dynamic.Resource(resource).Namespace(namespace)
	}
	list, err := lister.List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", resource.Resource, err)
	}
	objects := make([]*unstructured.Unstructured, 0, len(list.Items))
	for index := range list.Items {
		objects = append(objects, &list.Items[index])
	}
	return objects, nil
}

// list reads one discovered resource in its scope.
func (r *Rescue) list(ctx context.Context, groupVersion schema.GroupVersion, resource metav1.APIResource) ([]*unstructured.Unstructured, error) {
	namespace := ""
	if resource.Namespaced {
		namespace = metav1.NamespaceAll
	}
	return r.List(ctx, schema.GroupVersionResource{
		Group: groupVersion.Group, Version: groupVersion.Version, Resource: resource.Name,
	}, namespace)
}
