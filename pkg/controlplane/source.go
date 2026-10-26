package controlplane

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
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
	// List returns every object of a class; an empty namespace means all of them.
	List(ctx context.Context, object ObjectType, namespace string) ([]ObservedObject, error)
	// Get returns one object.
	Get(ctx context.Context, object ObjectType, namespace, name string) (ObservedObject, error)
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
func (s *clusterSource) List(ctx context.Context, object ObjectType, namespace string) ([]ObservedObject, error) {
	list, err := resourceOf(s.client, object, namespace).List(ctx, metav1.ListOptions{})
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
func (s *clusterSource) Get(ctx context.Context, object ObjectType, namespace, name string) (ObservedObject, error) {
	stored, err := resourceOf(s.client, object, namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ObservedObject{}, err
	}
	return ObservedObject{Object: stored.Object, ObservedAt: time.Now()}, nil
}

// resourceOf resolves the client resource of a class; a cluster-scoped class
// ignores the namespace.
func resourceOf(client dynamic.Interface, object ObjectType, namespace string) dynamic.ResourceInterface {
	resource := client.Resource(object.GVR())
	if object.Namespaced {
		return resource.Namespace(namespace)
	}
	return resource.Namespace("")
}
