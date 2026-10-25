// Package tc17 verifies that adopting, releasing and declining an object is
// observable, reversible and stops KOS writes to a released object (AC-8,
// TC-17).
package tc17

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/adoption"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// requestTimeout bounds every API call so a wedged server fails the case.
const requestTimeout = 30 * time.Second

// TestTC17AdoptReleaseDecline takes an unmarked object over, edits it, releases
// it, and declines a second one.
func TestTC17AdoptReleaseDecline(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	writer := cluster.Writer()
	actions := adoption.New(writer)
	session := write.Session{Username: "alice", Role: write.RoleOwner}
	ctx := context.Background()

	adoptedRef := createUnmanaged(t, cluster, "adopted", "alice")
	declinedRef := createUnmanaged(t, cluster, "declined", "bob")

	before := cluster.Get(t, "v1beta1", "Application", adoptedRef.Name)
	if got := attribution(t, before); got != markers.AttributionUnmanaged {
		t.Fatalf("fresh object is %s, want unmanaged", got)
	}
	if adoptable, err := adoption.Adoptable(before.GetLabels()); err != nil || !adoptable {
		t.Fatalf("unmarked object is not offered for adoption: %v", err)
	}

	// Adoption records the attribution and the pre-adoption spec.
	adopted, err := actions.Adopt(ctx, session, adoptedRef, "pre-existing")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	assertAdopted(t, adopted, session.Username, "pre-existing")
	snapshot, err := actions.SnapshotOf(ctx, adoptedRef)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if !reflect.DeepEqual(snapshot.Spec, specOf(t, before)) {
		t.Fatalf("snapshot spec %v != pre-adoption spec %v", snapshot.Spec, specOf(t, before))
	}

	// The snapshot is immutable, and a second adoption is refused.
	record := snapshotRecord(t, cluster, adoptedRef)
	if _, managed := record.GetLabels()[markers.ManagedBy]; !managed {
		t.Fatal("the snapshot record carries no managed-by marker")
	}
	data := record.Object["data"].(map[string]any)
	data["snapshot"] = `{"spec":{"visibility":{"members":["mallory"]}}}`
	if _, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).Update(ctx, record, metav1.UpdateOptions{}); err == nil {
		t.Fatal("the snapshot record accepted a modification")
	}
	if _, err := actions.Adopt(ctx, session, adoptedRef, "again"); err == nil {
		t.Fatal("an adopted object was adopted twice")
	}
	if _, err := actions.Adopt(ctx, session, declinedRef, "two words"); err == nil {
		t.Fatal("an adoption reason a label cannot carry was accepted")
	}

	// Editing the spec after adoption leaves the snapshot alone. The edit goes
	// the way the object was created, as kubectl: its field ownership still sits
	// with that writer, so a merged apply would be a 409 (C-05).
	current := cluster.Get(t, "v1beta1", "Application", adoptedRef.Name)
	if err := unstructured.SetNestedStringSlice(current.Object, []string{"alice", "bob"}, "spec", "visibility", "members"); err != nil {
		t.Fatalf("set members: %v", err)
	}
	modified, err := cluster.Direct(t, api.Group, "v1beta1", "Application", harness.Namespace).Update(ctx, current, metav1.UpdateOptions{FieldManager: "kubectl"})
	if err != nil {
		t.Fatalf("edit adopted spec: %v", err)
	}
	unchanged, err := actions.SnapshotOf(ctx, adoptedRef)
	if err != nil {
		t.Fatalf("read snapshot after the edit: %v", err)
	}
	if !reflect.DeepEqual(unchanged.Spec, snapshot.Spec) {
		t.Fatalf("snapshot changed with the object: %v != %v", unchanged.Spec, snapshot.Spec)
	}

	// Release gives the object back in the state it had.
	released, err := actions.Release(ctx, session, adoptedRef)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := attribution(t, released); got != markers.AttributionUnmanaged {
		t.Fatalf("released object is %s, want unmanaged", got)
	}
	if !reflect.DeepEqual(specOf(t, released), specOf(t, modified)) {
		t.Fatalf("release changed the spec: %v != %v", specOf(t, released), specOf(t, modified))
	}

	// KOS does not write a released object any more.
	_, err = writer.Apply(ctx, write.SubjectApplicationReconciler, write.Session{}, write.PartStatus, statusDocument(adoptedRef.Name, "Ready"))
	var denied *write.Denied
	if !errors.As(err, &denied) {
		t.Fatalf("a released object was written by KOS: %v", err)
	}
	if !strings.Contains(denied.Error(), string(write.SubjectApplicationReconciler)) || !strings.Contains(denied.Error(), "Application") {
		t.Fatalf("refusal does not name the subject and the object: %v", denied)
	}
	after := cluster.Get(t, "v1beta1", "Application", adoptedRef.Name)
	if after.GetResourceVersion() != released.GetResourceVersion() {
		t.Fatalf("released object changed: resourceVersion %s -> %s", released.GetResourceVersion(), after.GetResourceVersion())
	}

	// Declining keeps the second object unmanaged and stops the offer.
	declined, err := actions.Decline(ctx, session, declinedRef)
	if err != nil {
		t.Fatalf("decline: %v", err)
	}
	if got := attribution(t, declined); got != markers.AttributionUnmanaged {
		t.Fatalf("declined object is %s, want unmanaged", got)
	}
	if declined.GetLabels()[markers.Declined] == "" {
		t.Fatal("declined object carries no declined marker")
	}
	if adoptable, err := adoption.Adoptable(declined.GetLabels()); err != nil || adoptable {
		t.Fatalf("declined object is still offered for adoption: %v", err)
	}
}

// createUnmanaged writes an object the way kubectl does: no attribution marker,
// a member in its visibility and nothing else.
func createUnmanaged(t *testing.T, cluster *harness.Cluster, name, member string) write.Ref {
	t.Helper()
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      name,
			"namespace": harness.Namespace,
		},
		"spec": map[string]any{
			"visibility": map[string]any{"members": []any{member}},
		},
	}}
	cluster.Create(t, object)
	return write.Ref{Group: api.Group, Version: "v1beta1", Kind: "Application", Namespace: harness.Namespace, Name: name}
}

// statusDocument is the apply document a reconciler writes on the subresource.
func statusDocument(name, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"status":     map[string]any{"phase": phase},
	}}
}

// snapshotRecord reads the ConfigMap that holds the snapshot of ref.
func snapshotRecord(t *testing.T, cluster *harness.Cluster, ref write.Ref) *unstructured.Unstructured {
	t.Helper()
	object := cluster.Get(t, ref.Version, ref.Kind, ref.Name)
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	list, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "kos.tsic.top/record=adoption-snapshot",
	})
	if err != nil {
		t.Fatalf("list snapshot records: %v", err)
	}
	for index := range list.Items {
		if list.Items[index].GetLabels()["kos.tsic.top/adopted-uid"] == string(object.GetUID()) {
			return &list.Items[index]
		}
	}
	t.Fatalf("no snapshot record for %s", ref)
	return nil
}

// assertAdopted checks the attribution markers adoption writes.
func assertAdopted(t *testing.T, object *unstructured.Unstructured, by, reason string) {
	t.Helper()
	if got := attribution(t, object); got != markers.AttributionAdopted {
		t.Fatalf("adopted object is %s, want adopted", got)
	}
	if got := object.GetLabels()[markers.AdoptedBy]; got != by {
		t.Errorf("adopted-by = %q, want %q", got, by)
	}
	if got := object.GetLabels()[markers.AdoptionReason]; got != reason {
		t.Errorf("adoption-reason = %q, want %q", got, reason)
	}
	if object.GetAnnotations()[markers.AdoptedAt] == "" {
		t.Error("adopted-at annotation is missing")
	}
	if err := markers.Validate(object.GetLabels(), object.GetAnnotations()); err != nil {
		t.Errorf("adopted object violates the marker contract: %v", err)
	}
}

// attribution derives the state the projection would show.
func attribution(t *testing.T, object *unstructured.Unstructured) markers.Attribution {
	t.Helper()
	state, err := markers.AttributionOf(object.GetLabels())
	if err != nil {
		t.Fatalf("attribution of %s: %v", object.GetName(), err)
	}
	return state
}

// specOf returns a copy of the object's spec.
func specOf(t *testing.T, object *unstructured.Unstructured) map[string]any {
	t.Helper()
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil || !found {
		t.Fatalf("read spec of %s: found=%v err=%v", object.GetName(), found, err)
	}
	return spec
}
