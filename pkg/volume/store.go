package volume

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/pkg/write"
)

// Store is the object access one volume round needs. NewWriterStore is the
// production implementation; a test substitutes its own to inject a failure.
type Store interface {
	// Get reads one kernel object.
	Get(ctx context.Context, ref write.Ref) (*unstructured.Unstructured, error)
	// GetResource reads one object of an explicit resource; native kinds such
	// as a claim, a persistent volume or a node are read this way because the
	// write path only reaches what it is allocated to write.
	GetResource(ctx context.Context, resource schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error)
	// List reads one resource in one namespace with a label selector.
	List(ctx context.Context, resource schema.GroupVersionResource, namespace, selector string) ([]unstructured.Unstructured, error)
	// CreateClaim creates the volume's bound claim (C-04).
	CreateClaim(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// ApplyClaim writes the volume's bound claim, which is how an expansion is
	// expressed (C-04, C-38).
	ApplyClaim(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// DeleteClaim removes the volume's bound claim; only this path may (C-39).
	DeleteClaim(ctx context.Context, ref write.Ref) error
	// MarkOn writes marker keys on exactly the object the caller read.
	MarkOn(ctx context.Context, current *unstructured.Unstructured, set map[string]string, remove []string) (*unstructured.Unstructured, error)
	// ApplyStatus writes the status of one volume (C-03).
	ApplyStatus(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// SetFinalizers writes the finalizer list of one volume (C-39).
	SetFinalizers(ctx context.Context, ref write.Ref, finalizers []string) (*unstructured.Unstructured, error)
}

// WriterStore is the production Store: every write runs through the admitted
// write path, so the volume reconciler writes only what C-04 allocates.
type WriterStore struct {
	writer *write.Writer
}

// NewWriterStore builds a store over one write path.
func NewWriterStore(writer *write.Writer) *WriterStore {
	return &WriterStore{writer: writer}
}

// Get implements Store.
func (s *WriterStore) Get(ctx context.Context, ref write.Ref) (*unstructured.Unstructured, error) {
	return s.writer.Get(ctx, ref)
}

// GetResource implements Store.
func (s *WriterStore) GetResource(ctx context.Context, resource schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	var readable dynamic.ResourceInterface = s.writer.Dynamic.Resource(resource)
	if namespace != "" {
		readable = s.writer.Dynamic.Resource(resource).Namespace(namespace)
	}
	return readable.Get(ctx, name, metav1.GetOptions{})
}

// List implements Store.
func (s *WriterStore) List(ctx context.Context, resource schema.GroupVersionResource, namespace, selector string) ([]unstructured.Unstructured, error) {
	var readable dynamic.ResourceInterface = s.writer.Dynamic.Resource(resource)
	if namespace != "" {
		readable = s.writer.Dynamic.Resource(resource).Namespace(namespace)
	}
	list, err := readable.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// CreateClaim implements Store.
func (s *WriterStore) CreateClaim(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.writer.CreateClaim(ctx, write.SubjectVolumeReconciler, write.Session{}, write.ClaimBound, document)
}

// ApplyClaim implements Store.
func (s *WriterStore) ApplyClaim(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.writer.ApplyClaim(ctx, write.SubjectVolumeReconciler, write.Session{}, write.ClaimBound, document)
}

// DeleteClaim implements Store.
func (s *WriterStore) DeleteClaim(ctx context.Context, ref write.Ref) error {
	return s.writer.DeleteClaim(ctx, write.SubjectVolumeReconciler, write.Session{}, write.ClaimBound, ref)
}

// MarkOn implements Store.
func (s *WriterStore) MarkOn(ctx context.Context, current *unstructured.Unstructured, set map[string]string, remove []string) (*unstructured.Unstructured, error) {
	return s.writer.MarkOn(ctx, write.SubjectVolumeReconciler, write.Session{}, current, set, remove)
}

// ApplyStatus implements Store.
func (s *WriterStore) ApplyStatus(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.writer.Apply(ctx, write.SubjectVolumeReconciler, write.Session{}, write.PartStatus, document)
}

// SetFinalizers implements Store.
func (s *WriterStore) SetFinalizers(ctx context.Context, ref write.Ref, finalizers []string) (*unstructured.Unstructured, error) {
	return s.writer.SetFinalizers(ctx, write.SubjectVolumeReconciler, ref, finalizers)
}
