// Package tc18 verifies that an application's reconciler never writes the spec
// of the volume it references, even while the application is reconciled and
// upgraded (AC-8, TC-18).
package tc18

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC18ParentDoesNotWriteChildSpec writes the child spec as the application
// reconciler would and checks the volume keeps the user's value.
func TestTC18ParentDoesNotWriteChildSpec(t *testing.T) {
	cluster := harness.Start(t)
	writer := cluster.Writer()
	alice := write.Session{Username: "alice", Role: write.RoleMember}
	ctx := context.Background()

	application, err := writer.Create(ctx, write.SubjectUser, alice, object("Application", "web"))
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	cascade, err := writer.Create(ctx, write.SubjectUser, alice, object("Volume", "data"))
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	for _, created := range []*unstructured.Unstructured{application, cascade} {
		owner, _, err := unstructured.NestedString(created.Object, "spec", "owner")
		if err != nil || owner != alice.Username {
			t.Fatalf("%s owner = %q (err %v), want %q", created.GetKind(), owner, err, alice.Username)
		}
	}

	before := cluster.Get(t, "v1beta1", "Volume", "data")

	// The parent's reconciler tries to take the child's spec over.
	attempt := specDocument("Volume", "data", map[string]any{"owner": "mallory"})
	_, err = writer.Apply(ctx, write.SubjectApplicationReconciler, write.Session{}, write.PartSpec, attempt)
	var denied *write.Denied
	if !errors.As(err, &denied) {
		t.Fatalf("the application reconciler wrote a volume spec: %v", err)
	}
	if !strings.Contains(denied.Error(), string(write.SubjectApplicationReconciler)) || !strings.Contains(denied.Error(), "Volume") {
		t.Fatalf("refusal does not name the subject and the object: %v", denied)
	}
	if denied.Code() != 403 {
		t.Fatalf("refusal code = %d, want 403", denied.Code())
	}

	afterAttempt := cluster.Get(t, "v1beta1", "Volume", "data")
	if !reflect.DeepEqual(specOf(t, afterAttempt), specOf(t, before)) {
		t.Fatalf("volume spec changed to %v, want %v", specOf(t, afterAttempt), specOf(t, before))
	}

	// Two reconcile rounds — an install and an upgrade — write only the
	// application's status.
	for _, phase := range []string{"Installing", "Ready"} {
		if _, err := writer.Apply(ctx, write.SubjectApplicationReconciler, write.Session{}, write.PartStatus, statusDocument("web", phase)); err != nil {
			t.Fatalf("write application status %q: %v", phase, err)
		}
	}

	final := cluster.Get(t, "v1beta1", "Volume", "data")
	if !reflect.DeepEqual(specOf(t, final), specOf(t, before)) {
		t.Fatalf("volume spec changed to %v during reconcile, want %v", specOf(t, final), specOf(t, before))
	}
	applicationStatus := cluster.Get(t, "v1beta1", "Application", "web")
	phase, _, err := unstructured.NestedString(applicationStatus.Object, "status", "phase")
	if err != nil || phase != "Ready" {
		t.Fatalf("application phase = %q (err %v), want Ready", phase, err)
	}
}

// object builds an owned kernel object with its attribution marker, as the
// creating user writes it.
func object(kind, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": harness.Namespace,
			"labels":    map[string]any{markers.ManagedBy: "kos-shell"},
		},
	}}
}

// specDocument is the apply document a reconciler sends for a spec.
func specDocument(kind, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec":       spec,
	}}
}

// statusDocument is the apply document a reconciler sends for a status.
func statusDocument(name, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"status":     map[string]any{"phase": phase},
	}}
}

// specOf returns a copy of an object's spec.
func specOf(t *testing.T, object *unstructured.Unstructured) map[string]any {
	t.Helper()
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil || !found {
		t.Fatalf("read spec of %s: found=%v err=%v", object.GetName(), found, err)
	}
	return spec
}
