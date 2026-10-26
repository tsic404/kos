package identity

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/write"
)

// OwnedObject names one kernel object an account owns (C-60).
type OwnedObject struct {
	// Kind is the object's kind.
	Kind string `json:"kind"`
	// Namespace is empty for a cluster-scoped kind.
	Namespace string `json:"namespace,omitempty"`
	// Name is the object's name.
	Name string `json:"name"`
}

// OwnedObjects finds what an account still owns. Account deletion asks this
// before removing the account (C-60, C-63).
type OwnedObjects interface {
	ListOwned(ctx context.Context, username string) ([]OwnedObject, error)
}

// ClusterOwnedObjects reads ownership from the kernel objects themselves:
// spec.owner is written by the API and is the authority C-60 names.
type ClusterOwnedObjects struct {
	// Dynamic reads the kernel objects; required.
	Dynamic dynamic.Interface
}

// ListOwned lists every owned object of every owner-carrying kind, sorted for a
// stable response.
func (c ClusterOwnedObjects) ListOwned(ctx context.Context, username string) ([]OwnedObject, error) {
	if c.Dynamic == nil {
		return nil, ErrOwnershipUnavailable
	}
	owned := make([]OwnedObject, 0, 4)
	for _, kind := range write.OwnedKinds() {
		plural, ok := api.ResourceName(kind)
		if !ok {
			return nil, fmt.Errorf("identity: kind %q has no resource name", kind)
		}
		resource := c.Dynamic.Resource(schema.GroupVersionResource{
			Group:    api.Group,
			Version:  v1beta1.GroupVersion.Version,
			Resource: plural,
		})
		var scoped dynamic.ResourceInterface = resource
		if scope, _ := api.ScopeOf(kind); scope == api.ScopeNamespaced {
			scoped = resource.Namespace("")
		}
		list, err := scoped.List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("identity: list %s: %w", kind, err)
		}
		for index := range list.Items {
			object := &list.Items[index]
			if owner, _, _ := unstructured.NestedString(object.Object, "spec", "owner"); owner != username {
				continue
			}
			owned = append(owned, OwnedObject{Kind: kind, Namespace: object.GetNamespace(), Name: object.GetName()})
		}
	}
	sort.Slice(owned, func(left, right int) bool {
		if owned[left].Kind != owned[right].Kind {
			return owned[left].Kind < owned[right].Kind
		}
		if owned[left].Namespace != owned[right].Namespace {
			return owned[left].Namespace < owned[right].Namespace
		}
		return owned[left].Name < owned[right].Name
	})
	return owned, nil
}
