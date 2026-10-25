package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
)

// writeRecorder is a cluster client that records what the write path submits and
// answers with one canned error.
type writeRecorder struct {
	dynamic.Interface
	current   *unstructured.Unstructured
	createErr error
	writeErr  error
	submitted *unstructured.Unstructured
	deleted   *metav1.DeleteOptions
}

// Resource implements dynamic.Interface.
func (r *writeRecorder) Resource(schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &recordedResource{recorder: r}
}

// recordedResource is the single resource the recorder serves.
type recordedResource struct {
	dynamic.NamespaceableResourceInterface
	recorder *writeRecorder
}

// Namespace implements dynamic.NamespaceableResourceInterface.
func (r *recordedResource) Namespace(string) dynamic.ResourceInterface { return r }

// Get implements dynamic.ResourceInterface.
func (r *recordedResource) Get(_ context.Context, name string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	if r.recorder.current == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: api.Group, Resource: "systemcomponents"}, name)
	}
	return r.recorder.current.DeepCopy(), nil
}

// Create implements dynamic.ResourceInterface.
func (r *recordedResource) Create(_ context.Context, _ *unstructured.Unstructured, _ metav1.CreateOptions, _ ...string) (*unstructured.Unstructured, error) {
	return nil, r.recorder.createErr
}

// Update implements dynamic.ResourceInterface.
func (r *recordedResource) Update(_ context.Context, object *unstructured.Unstructured, _ metav1.UpdateOptions, _ ...string) (*unstructured.Unstructured, error) {
	r.recorder.submitted = object.DeepCopy()
	if r.recorder.writeErr != nil {
		return nil, r.recorder.writeErr
	}
	return object, nil
}

// Delete implements dynamic.ResourceInterface.
func (r *recordedResource) Delete(_ context.Context, _ string, options metav1.DeleteOptions, _ ...string) error {
	r.recorder.deleted = &options
	return r.recorder.writeErr
}

// currentObject is one stored object with a resource version.
func currentObject(name, version string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "SystemComponent",
		"metadata":   map[string]any{"name": name, "resourceVersion": version},
	}}
}

// desiredVersion builds the desired state of that object.
func desiredVersion(name string) desiredObject {
	return desiredObject{
		Ref: kernelRef("SystemComponent", "", name),
		Object: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": api.Group + "/v1beta1",
			"kind":       "SystemComponent",
			"metadata":   map[string]any{"name": name},
		}},
	}
}

// TestUpdateSubmitsTheCheckedVersion keeps the write path from re-reading and
// overwriting: the version the caller's precondition passed must be the one the
// API server judges, so a write that landed in between is a conflict (C-05).
func TestUpdateSubmitsTheCheckedVersion(t *testing.T) {
	recorder := &writeRecorder{
		current:  currentObject("alpha", "9"),
		writeErr: apierrors.NewConflict(schema.GroupResource{Group: api.Group, Resource: "systemcomponents"}, "alpha", errors.New("the object has been modified")),
	}
	server := &Server{dynamic: recorder, source: &fakeSource{}, log: slog.New(slog.DiscardHandler)}

	failure := server.update(context.Background(), desiredVersion("alpha"), "7")
	if failure == nil || failure.Code != CodeConflict {
		t.Fatalf("failure = %v, want a conflict from the API server", failure)
	}
	if recorder.submitted == nil {
		t.Fatal("the update was never submitted")
	}
	if got := recorder.submitted.GetResourceVersion(); got != "7" {
		t.Fatalf("submitted resourceVersion = %q, want the checked version 7", got)
	}
}

// TestDeleteCarriesTheCheckedVersion keeps a delete from destroying an update
// that landed after the caller read the object (C-05).
func TestDeleteCarriesTheCheckedVersion(t *testing.T) {
	recorder := &writeRecorder{
		current:  currentObject("alpha", "9"),
		writeErr: apierrors.NewConflict(schema.GroupResource{Group: api.Group, Resource: "systemcomponents"}, "alpha", errors.New("the object has been modified")),
	}
	server := &Server{dynamic: recorder, source: &fakeSource{}, log: slog.New(slog.DiscardHandler)}
	ref := kernelRef("SystemComponent", "", "alpha")

	if failure := server.delete(context.Background(), ref, "7"); failure == nil || failure.Code != CodeConflict {
		t.Fatalf("failure = %v, want a conflict from the API server", failure)
	}
	if recorder.deleted == nil || recorder.deleted.Preconditions == nil || recorder.deleted.Preconditions.ResourceVersion == nil {
		t.Fatal("the delete carried no resource version precondition")
	}
	if got := *recorder.deleted.Preconditions.ResourceVersion; got != "7" {
		t.Fatalf("delete precondition resourceVersion = %q, want the checked version 7", got)
	}
}

// TestDeleteWithoutPreconditionUsesTheReadVersion checks the apply path, which
// has no caller-supplied version: the delete still carries the version it read.
func TestDeleteWithoutPreconditionUsesTheReadVersion(t *testing.T) {
	recorder := &writeRecorder{current: currentObject("alpha", "9")}
	server := &Server{dynamic: recorder, source: &fakeSource{}, log: slog.New(slog.DiscardHandler)}

	if failure := server.delete(context.Background(), kernelRef("SystemComponent", "", "alpha"), ""); failure != nil {
		t.Fatalf("delete: %v", failure)
	}
	if recorder.deleted == nil || recorder.deleted.Preconditions == nil || recorder.deleted.Preconditions.ResourceVersion == nil {
		t.Fatal("the delete carried no resource version precondition")
	}
	if got := *recorder.deleted.Preconditions.ResourceVersion; got != "9" {
		t.Fatalf("delete precondition resourceVersion = %q, want the read version 9", got)
	}
}

// TestDeleteOfAGoneObjectSucceeds checks the idempotent delete path.
func TestDeleteOfAGoneObjectSucceeds(t *testing.T) {
	recorder := &writeRecorder{}
	server := &Server{dynamic: recorder, source: &fakeSource{}, log: slog.New(slog.DiscardHandler)}
	if failure := server.delete(context.Background(), kernelRef("SystemComponent", "", "alpha"), "7"); failure != nil {
		t.Fatalf("delete of a gone object: %v", failure)
	}
	if recorder.deleted != nil {
		t.Fatal("a delete was issued for an object that does not exist")
	}
}

// TestCreateAcceptsWhenTheExistingObjectCannotBeRead keeps a repeat submission
// from turning into a failure when the source lags the write it just saw (C-12).
func TestCreateAcceptsWhenTheExistingObjectCannotBeRead(t *testing.T) {
	recorder := &writeRecorder{createErr: apierrors.NewAlreadyExists(
		schema.GroupResource{Group: api.Group, Resource: "systemcomponents"}, "alpha")}
	server := &Server{dynamic: recorder, source: &fakeSource{}, log: slog.New(slog.DiscardHandler)}

	if failure := server.create(context.Background(), desiredVersion("alpha")); failure != nil {
		t.Fatalf("create = %v, want acceptance: the identity already exists", failure)
	}
}

// TestFailureMessagesKeepTheClusterDetailOut checks that a user-facing message
// carries no endpoint or subject from the cluster's own error (C-12).
func TestFailureMessagesKeepTheClusterDetailOut(t *testing.T) {
	var logged strings.Builder
	server := &Server{
		source: &fakeSource{},
		log:    slog.New(slog.NewTextHandler(&logged, nil)),
	}
	detail := "dial tcp 10.0.0.7:6443: connect: connection refused"
	ref := kernelRef("Application", "default", "web")

	readFailure := server.readFailure(errors.New(detail), ref)
	if strings.Contains(readFailure.Message, "10.0.0.7") {
		t.Fatalf("read message leaks the cluster detail: %q", readFailure.Message)
	}
	invalid := server.writeFailure(context.Background(), apierrors.NewInvalid(
		schema.GroupKind{Group: api.Group, Kind: "Application"}, "web",
		field.ErrorList{field.Invalid(field.NewPath("spec"), "x", detail)}), ref)
	if strings.Contains(invalid.Message, "10.0.0.7") {
		t.Fatalf("write message leaks the cluster detail: %q", invalid.Message)
	}
	if !strings.Contains(logged.String(), "10.0.0.7") {
		t.Fatalf("the cluster detail never reached the log: %q", logged.String())
	}
}

// TestConflictCarriesTheCurrentObject checks the conflict answer C-05 requires.
func TestConflictCarriesTheCurrentObject(t *testing.T) {
	current := map[string]any{"metadata": map[string]any{"name": "web", "resourceVersion": "9"}}
	failure := conflict(kernelRef("Application", "default", "web"), "已被其他写入修改", current)
	if failure.Current == nil || failure.Current["metadata"] == nil {
		t.Fatalf("the conflict carries no current object: %v", failure)
	}
	if failure.Code != CodeConflict || failure.Retryable != true {
		t.Fatalf("conflict = %+v", failure)
	}
}
