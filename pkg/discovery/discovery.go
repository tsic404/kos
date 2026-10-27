// Package discovery runs the discovery adapters of C-25: each adapter reads
// the object classes it covers and reports what it finds. Discovery never
// writes an object's spec and never creates a downstream object for an object
// it found; adoption stays a separate, explicit action.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/pkg/extension"
)

// Adapter is one running discovery adapter (C-25). IsActive follows the
// component readiness of C-28: an inactive adapter reports nothing and leaves
// already discovered objects in place.
type Adapter struct {
	// ID is the adapter identity.
	ID string
	// Covers lists the object classes the adapter reads.
	Covers []schema.GroupKind
	// Scope limits the namespaces and labels the adapter reads.
	Scope Scope
	// Collector is the collection identity the adapter may write device facts
	// as; empty means it writes no device facts.
	Collector string
	// Fields lists the dotted paths the adapter reports per object.
	Fields []string
	// IsActive tells whether the component contribution is active (C-28).
	IsActive bool
}

// Scope is the reach of one adapter (C-25): an empty namespace list reads
// every namespace and an empty selector matches every object.
type Scope struct {
	Namespaces    []string
	LabelSelector string
}

// Resources maps a covered class to the cluster resource that serves it. The
// adapter declaration names kinds; the version and plural are cluster facts, so
// the caller supplies them.
type Resources map[schema.GroupKind]schema.GroupVersionResource

// Ref identifies one discovered object.
type Ref struct {
	// Group is the object's API group.
	Group string
	// Kind is the object's kind.
	Kind string
	// Namespace is empty for a cluster-scoped object.
	Namespace string
	// Name is the object's name.
	Name string
}

// String renders the reference for messages.
func (r Ref) String() string {
	if r.Namespace == "" {
		return r.Group + "/" + r.Kind + "/" + r.Name
	}
	return r.Group + "/" + r.Kind + "/" + r.Namespace + "/" + r.Name
}

// Report is one adapter's observation of one object.
type Report struct {
	// Adapter is the reporting adapter.
	Adapter string
	// Collector is the collection identity the adapter declared (C-25); a
	// device writer uses it to know which hostname it may write as.
	Collector string
	// Ref identifies the object.
	Ref Ref
	// Fields holds the declared field set that was found, keyed by dotted path.
	Fields map[string]any
	// ObservedAt is when the adapter read the object.
	ObservedAt time.Time
}

// Runner executes the adapters against one cluster.
type Runner struct {
	// Dynamic reads the covered classes; required.
	Dynamic dynamic.Interface
	// Resources resolves each covered class; required.
	Resources Resources
	// Adapters are the loaded adapters.
	Adapters []Adapter
	// Now supplies the observation time; nil means the wall clock.
	Now func() time.Time
}

// Run reads every active adapter's covered classes and returns the reports. It
// performs reads only: no object is written and no downstream is created. One
// adapter's failure is collected and the run continues, so the other adapters'
// reports and sources survive it (C-25).
func (r *Runner) Run(ctx context.Context) ([]Report, error) {
	if r.Dynamic == nil {
		return nil, errors.New("discovery: a dynamic client is required")
	}
	now := r.now()
	var reports []Report
	var problems []error
	for _, adapter := range r.Adapters {
		if !adapter.IsActive {
			continue
		}
		selector, err := labels.Parse(adapter.Scope.LabelSelector)
		if err != nil {
			problems = append(problems, fmt.Errorf("discovery adapter %s: %w", adapter.ID, err))
			continue
		}
		for _, cover := range adapter.Covers {
			resource, known := r.Resources[cover]
			if !known {
				problems = append(problems, fmt.Errorf("discovery adapter %s: no resource for %s", adapter.ID, cover.String()))
				continue
			}
			objects, err := r.list(ctx, resource, adapter.Scope.Namespaces, selector)
			if err != nil {
				problems = append(problems, fmt.Errorf("discovery adapter %s: read %s: %w", adapter.ID, cover.String(), err))
				continue
			}
			for _, object := range objects {
				reports = append(reports, Report{
					Adapter:   adapter.ID,
					Collector: adapter.Collector,
					Ref: Ref{
						Group:     resource.Group,
						Kind:      cover.Kind,
						Namespace: object.GetNamespace(),
						Name:      object.GetName(),
					},
					Fields:     fieldsOf(object.Object, adapter.Fields),
					ObservedAt: now,
				})
			}
		}
	}
	return reports, errors.Join(problems...)
}

// list reads one class in the declared scope.
func (r *Runner) list(ctx context.Context, resource schema.GroupVersionResource, namespaces []string, selector labels.Selector) ([]*unstructured.Unstructured, error) {
	var objects []*unstructured.Unstructured
	if len(namespaces) == 0 {
		list, err := r.Dynamic.Resource(resource).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return nil, err
		}
		for index := range list.Items {
			objects = append(objects, &list.Items[index])
		}
		return objects, nil
	}
	for _, namespace := range namespaces {
		list, err := r.Dynamic.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return nil, err
		}
		for index := range list.Items {
			objects = append(objects, &list.Items[index])
		}
	}
	return objects, nil
}

// fieldsOf reads the declared dotted paths that exist on one object.
func fieldsOf(object map[string]any, paths []string) map[string]any {
	if len(paths) == 0 {
		return nil
	}
	found := map[string]any{}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if value, ok := valueAt(object, path); ok {
			found[path] = value
		}
	}
	if len(found) == 0 {
		return nil
	}
	return found
}

// valueAt reads one dotted path from a decoded object.
func valueAt(object map[string]any, path string) (any, bool) {
	var value any = object
	for _, key := range strings.Split(path, ".") {
		parent, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok = parent[key]
		if !ok {
			return nil, false
		}
	}
	return value, true
}

// Index keeps the most recent reports by object and answers which adapters
// reported it (C-25: one object may have several sources).
type Index struct {
	mu      sync.RWMutex
	sources map[string][]string
}

// NewIndex builds an empty index.
func NewIndex() *Index {
	return &Index{sources: map[string][]string{}}
}

// Replace swaps the index content for one run's reports. An adapter that is no
// longer active stops reporting; its reports leave the index.
func (i *Index) Replace(reports []Report) {
	sources := map[string]map[string]bool{}
	for _, report := range reports {
		key := report.Ref.key()
		if sources[key] == nil {
			sources[key] = map[string]bool{}
		}
		sources[key][report.Adapter] = true
	}
	indexed := make(map[string][]string, len(sources))
	for key, adapters := range sources {
		ids := make([]string, 0, len(adapters))
		for id := range adapters {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		indexed[key] = ids
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.sources = indexed
}

// Sources returns the adapters that reported one object, sorted. The result is
// nil when no adapter reported it.
func (i *Index) Sources(group, kind, namespace, name string) []string {
	if i == nil {
		return nil
	}
	key := Ref{Group: group, Kind: kind, Namespace: namespace, Name: name}.key()
	i.mu.RLock()
	defer i.mu.RUnlock()
	return append([]string(nil), i.sources[key]...)
}

// key is the index identity of one object.
func (r Ref) key() string {
	return r.Group + "/" + r.Kind + "/" + r.Namespace + "/" + r.Name
}

// FromDeclaration converts one loaded declaration into a running adapter. The
// caller decides activation by setting IsActive.
func FromDeclaration(declaration extension.DiscoveryAdapter) Adapter {
	adapter := Adapter{
		ID:        declaration.ID,
		Collector: declaration.Collector,
		IsActive:  true,
		Fields:    append([]string(nil), declaration.Fields...),
		Scope: Scope{
			Namespaces:    append([]string(nil), declaration.Scope.Namespaces...),
			LabelSelector: declaration.Scope.LabelSelector,
		},
	}
	for _, cover := range declaration.Covers {
		adapter.Covers = append(adapter.Covers, cover.Schema())
	}
	return adapter
}

// now returns the observation time of a run.
func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now().UTC()
}
