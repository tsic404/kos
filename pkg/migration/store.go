package migration

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/pkg/write"
)

// Store is the object access one migration round needs. NewWriterStore is the
// production implementation; a test substitutes its own to inject a failure.
type Store interface {
	// Get reads one kernel object.
	Get(ctx context.Context, ref write.Ref) (*unstructured.Unstructured, error)
	// GetResource reads one object of an explicit resource: the claims, the
	// storage classes and the Jobs the flow observes are native objects the
	// write path only reaches where C-04 allocates a write.
	GetResource(ctx context.Context, resource schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error)
	// List reads one resource in one namespace with a label selector.
	List(ctx context.Context, resource schema.GroupVersionResource, namespace, selector string) ([]unstructured.Unstructured, error)
	// CreateClaim creates the migration target (C-04).
	CreateClaim(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// DeleteClaim removes the migration target; the flow may not delete the
	// claim a volume is bound to (C-04, F6).
	DeleteClaim(ctx context.Context, ref write.Ref) error
	// CreateJob creates one step Job of the flow (C-04, C-40).
	CreateJob(ctx context.Context, namespace string, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// DeleteJob removes one step Job of the flow (C-04, C-40).
	DeleteJob(ctx context.Context, namespace string, ref write.Ref) error
	// MarkOn writes marker keys on exactly the object the caller read.
	MarkOn(ctx context.Context, current *unstructured.Unstructured, set map[string]string, remove []string) (*unstructured.Unstructured, error)
	// ApplyStatus writes the status of one migration (C-03).
	ApplyStatus(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// ApplyVolumeStatus writes the two volume status fields C-40 and ADR-015
	// allocate to the migration flow.
	ApplyVolumeStatus(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// SetFinalizers writes the finalizer list of one migration.
	SetFinalizers(ctx context.Context, ref write.Ref, finalizers []string) (*unstructured.Unstructured, error)
	// RecordEvent appends one event about the object the flow acted on (C-64).
	RecordEvent(ctx context.Context, involved *unstructured.Unstructured, eventType, reason, message string, now time.Time) error
}

// WriterStore is the production Store: every write runs through the admitted
// write path, so the migration flow writes only what C-04 allocates.
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
	return s.writer.CreateClaim(ctx, write.SubjectMigrationReconciler, write.Session{}, write.ClaimMigrationTarget, document)
}

// DeleteClaim implements Store.
func (s *WriterStore) DeleteClaim(ctx context.Context, ref write.Ref) error {
	return s.writer.DeleteClaim(ctx, write.SubjectMigrationReconciler, write.Session{}, write.ClaimMigrationTarget, ref)
}

// CreateJob implements Store.
func (s *WriterStore) CreateJob(ctx context.Context, namespace string, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.scoped(namespace).Create(ctx, write.SubjectMigrationReconciler, write.Session{}, document)
}

// DeleteJob implements Store.
func (s *WriterStore) DeleteJob(ctx context.Context, namespace string, ref write.Ref) error {
	return s.scoped(namespace).Delete(ctx, write.SubjectMigrationReconciler, write.Session{}, ref)
}

// MarkOn implements Store.
func (s *WriterStore) MarkOn(ctx context.Context, current *unstructured.Unstructured, set map[string]string, remove []string) (*unstructured.Unstructured, error) {
	return s.writer.MarkOn(ctx, write.SubjectMigrationReconciler, write.Session{}, current, set, remove)
}

// ApplyStatus implements Store.
func (s *WriterStore) ApplyStatus(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.writer.Apply(ctx, write.SubjectMigrationReconciler, write.Session{}, write.PartStatus, document)
}

// ApplyVolumeStatus implements Store. Only the binding fields are in the
// document, so the volume reconciler's fields keep their writer (C-05, C-40).
func (s *WriterStore) ApplyVolumeStatus(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.writer.Apply(ctx, write.SubjectMigrationReconciler, write.Session{}, write.PartStatus, document)
}

// SetFinalizers implements Store.
func (s *WriterStore) SetFinalizers(ctx context.Context, ref write.Ref, finalizers []string) (*unstructured.Unstructured, error) {
	return s.writer.SetFinalizers(ctx, write.SubjectMigrationReconciler, ref, finalizers)
}

// RecordEvent implements Store. Events are the cluster's event stream rather
// than kernel or downstream objects, so they take no part in the C-04 writer
// allocation.
func (s *WriterStore) RecordEvent(ctx context.Context, involved *unstructured.Unstructured, eventType, reason, message string, now time.Time) error {
	occurred := metav1.NewTime(now)
	kind := involved.GroupVersionKind()
	event := &corev1.Event{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Event"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%d", involved.GetName(), now.UnixNano()),
			Namespace: involved.GetNamespace(),
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: kind.GroupVersion().String(),
			Kind:       kind.Kind,
			Namespace:  involved.GetNamespace(),
			Name:       involved.GetName(),
			UID:        involved.GetUID(),
		},
		Reason:              reason,
		Message:             message,
		Type:                eventType,
		Action:              reason,
		ReportingController: "kos.tsic.top/migration-reconciler",
		ReportingInstance:   "kos-shell",
		FirstTimestamp:      occurred,
		LastTimestamp:       occurred,
		Count:               1,
		EventTime:           metav1.NewMicroTime(now),
		Source:              corev1.EventSource{Component: "kos-migration-reconciler"},
	}
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(event)
	if err != nil {
		return fmt.Errorf("encode the event: %w", err)
	}
	resource := s.writer.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "events"}).Namespace(involved.GetNamespace())
	_, err = resource.Create(ctx, &unstructured.Unstructured{Object: encoded}, metav1.CreateOptions{})
	return err
}

// scoped returns a writer whose downstream scope is the namespace the flow
// writes in: the one the volume lives in (C-04).
func (s *WriterStore) scoped(namespace string) *write.Writer {
	scoped := *s.writer
	scoped.Scope = write.Scope{
		Groups:     s.writer.Scope.Groups,
		Namespaces: map[string]bool{namespace: true},
	}
	return &scoped
}
