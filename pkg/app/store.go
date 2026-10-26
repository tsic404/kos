package app

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/pkg/write"
)

// WriterStore is the production Store: every write runs through the admitted
// write path, so the application reconciler writes only what C-04 allocates.
type WriterStore struct {
	writer *write.Writer
}

// NewWriterStore builds a store over one write path.
func NewWriterStore(writer *write.Writer) *WriterStore {
	return &WriterStore{writer: writer}
}

// Get reads one object.
func (s *WriterStore) Get(ctx context.Context, ref write.Ref) (*unstructured.Unstructured, error) {
	return s.writer.Get(ctx, ref)
}

// List reads one resource in one namespace with a label selector.
func (s *WriterStore) List(ctx context.Context, target schema.GroupVersionResource, namespace, selector string) ([]unstructured.Unstructured, error) {
	resource := s.writer.Dynamic.Resource(target)
	var readable dynamic.ResourceInterface = resource
	if namespace != "" {
		readable = resource.Namespace(namespace)
	}
	list, err := readable.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// Apply writes one rendered object inside its declared namespace (C-04, C-32).
func (s *WriterStore) Apply(ctx context.Context, namespace string, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.scoped(namespace).Apply(ctx, write.SubjectApplicationReconciler, write.Session{}, write.PartObject, document)
}

// ApplyStatus writes the status of one application (C-03).
func (s *WriterStore) ApplyStatus(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.writer.Apply(ctx, write.SubjectApplicationReconciler, write.Session{}, write.PartStatus, document)
}

// CreateRecord creates one immutable record in the KOS namespace (C-04, C-33).
func (s *WriterStore) CreateRecord(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.writer.Create(ctx, write.SubjectApplicationReconciler, write.Session{}, document)
}

// Delete removes one rendered object inside its declared namespace (C-04).
func (s *WriterStore) Delete(ctx context.Context, namespace string, ref write.Ref) error {
	return s.scoped(namespace).Delete(ctx, write.SubjectApplicationReconciler, write.Session{}, ref)
}

// SetFinalizers writes the finalizer list of one application (C-33).
func (s *WriterStore) SetFinalizers(ctx context.Context, ref write.Ref, finalizers []string) (*unstructured.Unstructured, error) {
	return s.writer.SetFinalizers(ctx, write.SubjectApplicationReconciler, ref, finalizers)
}

// scoped returns a writer whose downstream scope is the one declared namespace
// of C-32.
func (s *WriterStore) scoped(namespace string) *write.Writer {
	scoped := *s.writer
	scoped.Scope = write.Scope{
		Groups:     s.writer.Scope.Groups,
		Namespaces: map[string]bool{namespace: true},
	}
	return &scoped
}
