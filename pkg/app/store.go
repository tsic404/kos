package app

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
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
// The write is forced: C-04 makes this reconciler the writer of the object, so
// the reconcile policy of C-35 writes the desired value back over a write made
// outside KOS.
func (s *WriterStore) Apply(ctx context.Context, namespace string, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return s.scoped(namespace).ApplyForce(ctx, write.SubjectApplicationReconciler, write.Session{}, write.PartObject, document)
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

// RecordEvent appends one event to the application's stream (C-64). Events are
// the cluster's event stream rather than kernel or downstream objects, so they
// take no part in the C-04 writer allocation.
func (s *WriterStore) RecordEvent(ctx context.Context, application *v1beta1.Application, eventType, reason, message string, now time.Time) error {
	occurred := metav1.NewTime(now)
	event := &corev1.Event{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Event"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%d", application.Name, now.UnixNano()),
			Namespace: application.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: api.Group + "/v1beta1",
			Kind:       "Application",
			Namespace:  application.Namespace,
			Name:       application.Name,
			UID:        application.UID,
		},
		Reason:              reason,
		Message:             message,
		Type:                eventType,
		Action:              reason,
		ReportingController: "kos.tsic.top/application-reconciler",
		ReportingInstance:   "kos-shell",
		FirstTimestamp:      occurred,
		LastTimestamp:       occurred,
		Count:               1,
		EventTime:           metav1.NewMicroTime(now),
		Source:              corev1.EventSource{Component: "kos-application-reconciler"},
	}
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(event)
	if err != nil {
		return fmt.Errorf("encode the event: %w", err)
	}
	resource := s.writer.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "events"}).Namespace(application.Namespace)
	_, err = resource.Create(ctx, &unstructured.Unstructured{Object: encoded}, metav1.CreateOptions{})
	return err
}

// storeVolumes reads the volume facts the upgrade preflight needs from the
// objects that hold them (C-34, C-38). The Volume object itself carries no size
// and no policy: the claim the volume creates under its own name holds the size
// and class it asks for, and the PersistentVolume it binds holds the class the
// storage backend actually gave it.
type storeVolumes struct {
	store Store
}

// VolumeFacts reads the desired and bound size and storage policy of one
// volume. A volume whose claim the cluster does not have yields no facts: the
// volume module has not bound it yet, so no size and no policy can change.
func (v storeVolumes) VolumeFacts(ctx context.Context, namespace, name string) (VolumeFacts, error) {
	claimRef := write.Ref{Version: "v1", Kind: "PersistentVolumeClaim", Namespace: namespace, Name: name}
	claim, err := v.store.Get(ctx, claimRef)
	if apierrors.IsNotFound(err) {
		return VolumeFacts{}, nil
	}
	if err != nil {
		return VolumeFacts{}, err
	}
	facts := VolumeFacts{
		Found:         true,
		DesiredSize:   nestedString(claim.Object, "spec", "resources", "requests", "storage"),
		DesiredPolicy: nestedString(claim.Object, "spec", "storageClassName"),
		ActualSize:    nestedString(claim.Object, "status", "capacity", "storage"),
		Bound:         nestedString(claim.Object, "status", "phase") == "Bound",
	}
	if !facts.Bound {
		return facts, nil
	}
	bound := nestedString(claim.Object, "spec", "volumeName")
	if bound == "" {
		return facts, nil
	}
	stored, err := v.store.Get(ctx, write.Ref{Version: "v1", Kind: "PersistentVolume", Name: bound})
	if apierrors.IsNotFound(err) {
		return facts, nil
	}
	if err != nil {
		return VolumeFacts{}, err
	}
	facts.ActualPolicy = nestedString(stored.Object, "spec", "storageClassName")
	return facts, nil
}

// nestedString reads one string field, treating an absent or non-string value
// as empty: the caller decides whether an empty fact is acceptable.
func nestedString(object map[string]any, fields ...string) string {
	value, _, _ := unstructured.NestedString(object, fields...)
	return value
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
