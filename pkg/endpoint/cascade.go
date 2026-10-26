package endpoint

import (
	"context"

	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// Actions are the application-side operations of C-45: the reverse lookup that
// answers "which endpoints expose this application" and the cascade that
// follows it on deletion. Both read the same set, so what a panel lists and
// what a deletion touches cannot drift.
//
// The cascade writes as the application reconciler, which is the subject C-04
// allocates cascade endpoints to; the endpoint reconciler itself never deletes
// a kernel object. The cascade does not touch the endpoint ledger: the ledger
// belongs to the endpoint reconciler, and its own delete event removes the
// entry and settles the survivors of the keys it held (C-44, C-06).
type Actions struct {
	// Writer executes the cascade deletes.
	Writer *write.Writer
}

// NewActions builds the C-45 actions over one cluster.
func NewActions(writer *write.Writer) *Actions {
	return &Actions{Writer: writer}
}

// CascadeResult is what one application deletion did to its endpoints.
type CascadeResult struct {
	// Deleted are the taken-over endpoints the cascade removed.
	Deleted []write.Ref `json:"deleted"`
	// Retained are the endpoints KOS does not manage, kept with a conflict.
	Retained []write.Ref `json:"retained"`
}

// EndpointsOf returns the endpoints in the application's namespace whose target
// is the application (C-45).
func (a *Actions) EndpointsOf(ctx context.Context, app write.Ref) ([]*unstructured.Unstructured, error) {
	lister := clusterLister{client: a.Writer.Dynamic, resources: a.Writer.Resources}
	objects, err := lister.List(ctx, "Endpoint", app.Namespace)
	if err != nil {
		return nil, err
	}
	endpoints := make([]*unstructured.Unstructured, 0, len(objects))
	for _, object := range objects {
		if TargetsApp(object, app.Name) {
			endpoints = append(endpoints, object)
		}
	}
	sort.Slice(endpoints, func(left, right int) bool {
		return endpoints[left].GetName() < endpoints[right].GetName()
	})
	return endpoints, nil
}

// DeleteForApplication removes the endpoints a deleted application took over and
// keeps the ones it did not (C-45). An unmanaged endpoint is not deleted and its
// spec and markers are untouched; the ledger marks it as pointing at a missing
// application, which is what the projection shows.
func (a *Actions) DeleteForApplication(ctx context.Context, app write.Ref) (CascadeResult, error) {
	endpoints, err := a.EndpointsOf(ctx, app)
	if err != nil {
		return CascadeResult{}, fmt.Errorf("list endpoints of %s: %w", app.Key(), err)
	}
	result := CascadeResult{Deleted: []write.Ref{}, Retained: []write.Ref{}}
	for _, object := range endpoints {
		ref := write.RefOf(object)
		entry := EntryOf(object)
		if entry.Attribution == markers.AttributionUnmanaged {
			// The entry stays in the ledger; the endpoint reconciler's
			// application round marks its missing target (C-45).
			result.Retained = append(result.Retained, ref)
			continue
		}
		if err := a.Writer.Delete(ctx, write.SubjectApplicationReconciler, write.Session{}, ref); err != nil {
			if !apierrors.IsNotFound(err) {
				return result, fmt.Errorf("delete endpoint %s: %w", ref.Key(), err)
			}
		}
		result.Deleted = append(result.Deleted, ref)
	}
	return result, nil
}

// ApplicationRef names an application in the storage version.
func ApplicationRef(namespace, name string) write.Ref {
	return write.Ref{Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "Application", Namespace: namespace, Name: name}
}
