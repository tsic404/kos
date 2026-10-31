package backup

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/pkg/write"
)

// Store is the object access one backup or restore round needs: it reads kernel
// objects, writes status as the round's declared writer, and creates the
// downstream objects C-04 allocates.
type Store interface {
	Get(ctx context.Context, ref write.Ref) (*unstructured.Unstructured, error)
	List(ctx context.Context, resource schema.GroupVersionResource, namespace, selector string) ([]unstructured.Unstructured, error)
	GetResource(ctx context.Context, resource schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error)
	ApplyStatus(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
	ApplyDownstream(ctx context.Context, namespace string, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
	DeleteDownstream(ctx context.Context, namespace string, ref write.Ref) error
	CreateObject(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
}

// WriterStore is the production Store: every write runs through the admitted
// write path, so a round writes only what C-03 and C-04 allocate.
type WriterStore struct {
	writer  *write.Writer
	subject write.Subject
}

// NewWriterStore builds a store over one write path for one declared subject.
func NewWriterStore(writer *write.Writer, subject write.Subject) *WriterStore {
	return &WriterStore{writer: writer, subject: subject}
}

// Get implements Store.
func (s *WriterStore) Get(ctx context.Context, ref write.Ref) (*unstructured.Unstructured, error) {
	return s.writer.Get(ctx, ref)
}

// List implements Store.
func (s *WriterStore) List(ctx context.Context, resource schema.GroupVersionResource, namespace, selector string) ([]unstructured.Unstructured, error) {
	readable := listClient(s.writer.Dynamic.Resource(resource), namespace)
	list, err := readable.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GetResource implements Store.
func (s *WriterStore) GetResource(ctx context.Context, resource schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	readable := listClient(s.writer.Dynamic.Resource(resource), namespace)
	return readable.Get(ctx, name, metav1.GetOptions{})
}

// ApplyStatus implements Store.
func (s *WriterStore) ApplyStatus(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.writer.Apply(ctx, s.subject, write.Session{}, write.PartStatus, document)
}

// ApplyDownstream implements Store for the downstream objects C-04 allocates.
func (s *WriterStore) ApplyDownstream(ctx context.Context, namespace string, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.scoped(namespace).Apply(ctx, s.subject, write.Session{}, write.PartObject, document)
}

// DeleteDownstream removes one downstream object.
func (s *WriterStore) DeleteDownstream(ctx context.Context, namespace string, ref write.Ref) error {
	return s.scoped(namespace).Delete(ctx, s.subject, write.Session{}, ref)
}

// CreateObject creates one new kernel object; admission restricts it to the
// restore's new instance (C-41).
func (s *WriterStore) CreateObject(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.writer.Create(ctx, s.subject, write.Session{}, document)
}

// scoped narrows the writer to one namespace, so admission sees the namespace
// the round declares.
func (s *WriterStore) scoped(namespace string) *write.Writer {
	scoped := *s.writer
	scoped.Scope = write.Scope{
		Groups:     s.writer.Scope.Groups,
		Namespaces: map[string]bool{namespace: true},
	}
	return &scoped
}

// listClient namespaces a dynamic resource unless the namespace is empty.
func listClient(resource dynamic.NamespaceableResourceInterface, namespace string) dynamic.ResourceInterface {
	if namespace == "" {
		return resource
	}
	return resource.Namespace(namespace)
}
