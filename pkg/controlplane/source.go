package controlplane

import (
	"context"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// clusterReadFailure maps a cluster read error to C-64's reasons. A missing
// object is still absent, but anything else names the cluster as the cause
// instead of answering with an empty result: an unusable credential is told
// apart from an unreachable API because only the first needs a repair before a
// retry can work. The cluster's own text stays in the log (C-12).
func (s *Server) clusterReadFailure(err error, ref ObjectRef) *Error {
	return ClusterReadFailure(err, ref, s.log)
}

// ClusterReadFailure maps a cluster read error to C-64's reasons for a caller
// outside the API handler. It is the same answer the handler gives, so a page
// source and the events route name a failure the same way (C-12, C-64).
func ClusterReadFailure(err error, ref ObjectRef, log *slog.Logger) *Error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	switch {
	case apierrors.IsNotFound(err):
		return notFound("对象 " + ref.String() + " 不存在").about(ref)
	case apierrors.IsUnauthorized(err), apierrors.IsForbidden(err):
		log.Error("cluster credential rejected", "object", ref.String(), "error", err)
		return clusterCredentialsInvalid("访问集群的凭据无效，读取不可用").about(ref)
	}
	log.Error("cluster read failed", "object", ref.String(), "error", err)
	return clusterUnavailable("集群 API 不可达，读取不可用").about(ref)
}
