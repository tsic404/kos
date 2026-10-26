package tc19

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC19AttributionSurvivesRenders covers C-02's creator rule across a
// reconcile cycle: a render that omits managed-by must not delete the marker
// its own manager wrote at creation, or the next write is refused as unmanaged.
func TestTC19AttributionSurvivesRenders(t *testing.T) {
	cluster := harness.Start(t)
	ctx := context.Background()
	writer := cluster.Writer()
	writer.Resources[schema.GroupKind{Kind: "Service"}] = "services"
	writer.Scope = write.Scope{Namespaces: map[string]bool{harness.Namespace: true}}

	// A downstream object: the first merged apply creates it with both
	// attribution keys, two later renders omit them, and the write path keeps
	// them on the same manager's field ownership.
	service, err := writer.Apply(ctx, write.SubjectApplicationReconciler, write.Session{}, write.PartObject, serviceDocument(applicationUID))
	if err != nil {
		t.Fatalf("apply-create service: %v", err)
	}
	assertAttribution(t, service, "apply-create")
	rendered, err := writer.Apply(ctx, write.SubjectApplicationReconciler, write.Session{}, write.PartObject, serviceDocument(""))
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
	assertAttribution(t, rendered, "second render")
	if _, err := writer.Apply(ctx, write.SubjectApplicationReconciler, write.Session{}, write.PartObject, serviceDocument("")); err != nil {
		t.Fatalf("third render refused: %v", err)
	}

	// A claim: ApplyClaim creates and re-renders through the same keepalive.
	claim, err := writer.ApplyClaim(ctx, write.SubjectVolumeReconciler, write.Session{}, write.ClaimBound, claimDocument(volumeUID))
	if err != nil {
		t.Fatalf("apply-create claim: %v", err)
	}
	assertAttribution(t, claim, "claim apply-create")
	renderedClaim, err := writer.ApplyClaim(ctx, write.SubjectVolumeReconciler, write.Session{}, write.ClaimBound, claimDocument(""))
	if err != nil {
		t.Fatalf("second claim render: %v", err)
	}
	assertAttribution(t, renderedClaim, "second claim render")
	if _, err := writer.ApplyClaim(ctx, write.SubjectVolumeReconciler, write.Session{}, write.ClaimBound, claimDocument("")); err != nil {
		t.Fatalf("third claim render refused: %v", err)
	}
}

// assertAttribution refuses a render that lost a marker the creator wrote.
func assertAttribution(t *testing.T, object *unstructured.Unstructured, stage string) {
	t.Helper()
	for _, key := range []string{markers.ManagedBy, markers.OwnerUID} {
		if got := object.GetLabels()[key]; got == "" {
			t.Fatalf("%s dropped %s", stage, key)
		}
	}
}

// applicationUID and volumeUID are the owners the caller declares for its
// downstream object and claim (C-02): the write path requires the marker, it
// does not derive the value.
const (
	applicationUID = "00000000-0000-0000-0000-0000000000a1"
	volumeUID      = "00000000-0000-0000-0000-0000000000b1"
)

// serviceDocument is what the application reconciler renders for its Service.
// An empty ownerUID is a render that omits the marker, as a plain manifest does.
func serviceDocument(ownerUID string) *unstructured.Unstructured {
	metadata := map[string]any{"name": "web", "namespace": harness.Namespace}
	if ownerUID != "" {
		metadata["labels"] = map[string]any{markers.OwnerUID: ownerUID}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   metadata,
		"spec": map[string]any{"ports": []any{
			map[string]any{"port": int64(80), "protocol": "TCP"},
		}},
	}}
}

// claimDocument is what the volume reconciler renders for its bound claim. An
// empty ownerUID is a render that omits the marker.
func claimDocument(ownerUID string) *unstructured.Unstructured {
	metadata := map[string]any{"name": "data", "namespace": harness.Namespace}
	if ownerUID != "" {
		metadata["labels"] = map[string]any{markers.OwnerUID: ownerUID}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   metadata,
		"spec": map[string]any{
			"accessModes": []any{"ReadWriteOnce"},
			"resources":   map[string]any{"requests": map[string]any{"storage": "1Gi"}},
		},
	}}
}
