package diagnostics

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Target names one object class and, for a get, one object of it. The resource
// and version travel with the target because the chain also reads extension
// classes whose resource name only the extension declaration knows (C-19).
type Target struct {
	// Group is the API group; empty for the core group.
	Group string
	// Version is the API version the class is read in.
	Version string
	// Kind is the object kind.
	Kind string
	// Resource is the plural resource name.
	Resource string
	// Namespace is empty for cluster-scoped classes.
	Namespace string
	// Name is the object name; empty for a list.
	Name string
	// FieldSelector narrows a list the way C-64 filters events by object; empty
	// lists the whole class in the target's scope.
	FieldSelector string
}

// Observed is one object as a source read it, with the moment it was observed
// (C-13).
type Observed struct {
	// Ref identifies the object.
	Ref ObjectRef
	// Object is the stored object.
	Object *unstructured.Unstructured
	// ObservedAt is when the source read it.
	ObservedAt time.Time
}

// Source reads the objects the chain walks. ClusterSource is the production
// implementation; a test substitutes its own to inject a state or a failure.
type Source interface {
	// Get reads one object. A missing object is reported as a NotFound error.
	Get(ctx context.Context, target Target) (Observed, error)
	// List reads every object of one class in one namespace.
	List(ctx context.Context, target Target) ([]Observed, error)
}

// ClusterSource reads objects through the dynamic client.
type ClusterSource struct {
	// Dynamic is the client; required.
	Dynamic dynamic.Interface
	// Now supplies the observation time; defaults to time.Now.
	Now func() time.Time
}

// Get implements Source.
func (s ClusterSource) Get(ctx context.Context, target Target) (Observed, error) {
	now := s.now()
	object, err := s.resource(target).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return Observed{}, err
	}
	return Observed{Ref: refOf(target, object), Object: object, ObservedAt: now}, nil
}

// List implements Source.
func (s ClusterSource) List(ctx context.Context, target Target) ([]Observed, error) {
	now := s.now()
	list, err := s.resource(target).List(ctx, metav1.ListOptions{FieldSelector: target.FieldSelector})
	if err != nil {
		return nil, err
	}
	observed := make([]Observed, 0, len(list.Items))
	for index := range list.Items {
		object := &list.Items[index]
		observed = append(observed, Observed{Ref: refOf(target, object), Object: object, ObservedAt: now})
	}
	return observed, nil
}

// resource returns the client of one target.
func (s ClusterSource) resource(target Target) dynamic.ResourceInterface {
	resource := s.Dynamic.Resource(schema.GroupVersionResource{
		Group: target.Group, Version: target.Version, Resource: target.Resource,
	})
	if target.Namespace == "" {
		return resource
	}
	return resource.Namespace(target.Namespace)
}

// now reads the clock, defaulting to the wall clock.
func (s ClusterSource) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// refOf builds the reference of an object read through target.
func refOf(target Target, object *unstructured.Unstructured) ObjectRef {
	return ObjectRef{
		Group:     target.Group,
		Version:   target.Version,
		Kind:      target.Kind,
		Namespace: object.GetNamespace(),
		Name:      object.GetName(),
	}
}

// nativeClass is the version and resource of a native class the chain reads.
type nativeClass struct {
	version  string
	resource string
}

// nativeClasses names every native class the diagnostics path reads. Packages
// render native objects (C-32), so the chain knows their resources without
// consulting discovery.
var nativeClasses = map[schema.GroupKind]nativeClass{
	{Kind: "ConfigMap"}:                           {version: "v1", resource: "configmaps"},
	{Kind: "Event"}:                               {version: "v1", resource: "events"},
	{Kind: "Node"}:                                {version: "v1", resource: "nodes"},
	{Kind: "Pod"}:                                 {version: "v1", resource: "pods"},
	{Kind: "Service"}:                             {version: "v1", resource: "services"},
	{Group: "apps", Kind: "DaemonSet"}:            {version: "v1", resource: "daemonsets"},
	{Group: "apps", Kind: "Deployment"}:           {version: "v1", resource: "deployments"},
	{Group: "apps", Kind: "ReplicaSet"}:           {version: "v1", resource: "replicasets"},
	{Group: "apps", Kind: "StatefulSet"}:          {version: "v1", resource: "statefulsets"},
	{Group: "batch", Kind: "CronJob"}:             {version: "v1", resource: "cronjobs"},
	{Group: "batch", Kind: "Job"}:                 {version: "v1", resource: "jobs"},
	{Group: "networking.k8s.io", Kind: "Ingress"}: {version: "v1", resource: "ingresses"},
}

// nativeGroups are the groups the run kernel serves natively. A rendered object
// in any other group outside the kernel group belongs to an extension class
// (C-19).
var nativeGroups = map[string]bool{
	"":                             true,
	"admissionregistration.k8s.io": true,
	"apiextensions.k8s.io":         true,
	"apiregistration.k8s.io":       true,
	"apps":                         true,
	"autoscaling":                  true,
	"batch":                        true,
	"certificates.k8s.io":          true,
	"coordination.k8s.io":          true,
	"discovery.k8s.io":             true,
	"events.k8s.io":                true,
	"flowcontrol.apiserver.k8s.io": true,
	"node.k8s.io":                  true,
	"networking.k8s.io":            true,
	"policy":                       true,
	"rbac.authorization.k8s.io":    true,
	"scheduling.k8s.io":            true,
	"storage.k8s.io":               true,
}
