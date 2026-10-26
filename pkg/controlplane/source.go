package controlplane

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
)

// ObservedObject is one object together with the time its source observed it.
type ObservedObject struct {
	// Object is the object as the source returned it.
	Object map[string]any
	// ObservedAt is when the source observed it.
	ObservedAt time.Time
}

// ObjectSource reads domain objects for the projection path. Each read reports
// its own observation time, which is what the stale marker is computed from
// (C-13); a source backed by a cache reports the cache's fill time.
type ObjectSource interface {
	// List returns every object of a kind; an empty namespace means all of them.
	List(ctx context.Context, kind, namespace string) ([]ObservedObject, error)
	// Get returns one object.
	Get(ctx context.Context, kind, namespace, name string) (ObservedObject, error)
}

// NewClusterSource reads through the cluster client the control plane holds and
// stamps reads with the read time.
func NewClusterSource(client dynamic.Interface) ObjectSource {
	return &clusterSource{client: client}
}

// clusterSource is the live source: the observation time is the time of the
// read, so its projections are fresh.
type clusterSource struct {
	client dynamic.Interface
}

// List implements ObjectSource.
func (s *clusterSource) List(ctx context.Context, kind, namespace string) ([]ObservedObject, error) {
	resource, err := resourceOf(s.client, kind, namespace)
	if err != nil {
		return nil, err
	}
	list, err := resource.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	observedAt := time.Now()
	objects := make([]ObservedObject, 0, len(list.Items))
	for index := range list.Items {
		objects = append(objects, ObservedObject{Object: list.Items[index].Object, ObservedAt: observedAt})
	}
	return objects, nil
}

// Get implements ObjectSource.
func (s *clusterSource) Get(ctx context.Context, kind, namespace, name string) (ObservedObject, error) {
	resource, err := resourceOf(s.client, kind, namespace)
	if err != nil {
		return ObservedObject{}, err
	}
	object, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ObservedObject{}, err
	}
	return ObservedObject{Object: object.Object, ObservedAt: time.Now()}, nil
}

// resourceOf resolves the client resource of a kernel kind. Cluster-scoped kinds
// ignore the namespace.
func resourceOf(client dynamic.Interface, kind, namespace string) (dynamic.ResourceInterface, error) {
	scope, ok := api.ScopeOf(kind)
	if !ok {
		return nil, fmt.Errorf("kind %q is not a kernel kind", kind)
	}
	resource := client.Resource(gvr(kind))
	if scope == api.ScopeNamespaced {
		return resource.Namespace(namespace), nil
	}
	return resource.Namespace(""), nil
}

// gvr is the group, version and resource of a kernel kind.
func gvr(kind string) schema.GroupVersionResource {
	name, _ := api.ResourceName(kind)
	return schema.GroupVersionResource{
		Group:    api.Group,
		Version:  v1beta1.GroupVersion.Version,
		Resource: name,
	}
}
