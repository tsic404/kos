package device

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
)

// TestFactMarkersFollowTheBoundaryAndTheRefreshBound pins C-37's two markers:
// invalid when the node field differs from the object name or the marker exists,
// stale when the observation is older than StaleAfter or the marker exists.
func TestFactMarkersFollowTheBoundaryAndTheRefreshBound(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		labels     map[string]string
		node       string
		objectName string
		observedAt *metav1.Time
		invalid    bool
		stale      bool
	}{
		{name: "fresh", node: "node-a", objectName: "node-a", observedAt: moment(now.Add(-time.Hour))},
		{name: "node field differs", node: "node-b", objectName: "node-a", observedAt: moment(now), invalid: true},
		{name: "older than the refresh bound", node: "node-a", objectName: "node-a",
			observedAt: moment(now.Add(-StaleAfter - time.Second)), stale: true},
		{name: "exactly at the refresh bound", node: "node-a", objectName: "node-a", observedAt: moment(now.Add(-StaleAfter))},
		{name: "invalid marker", labels: map[string]string{markers.Invalid: "true"},
			node: "node-a", objectName: "node-a", observedAt: moment(now), invalid: true},
		{name: "stale marker", labels: map[string]string{markers.Stale: "true"},
			node: "node-a", objectName: "node-a", observedAt: moment(now), stale: true},
		{name: "no observation", node: "node-a", objectName: "node-a"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			labels := make(map[string]any, len(testCase.labels))
			for key, value := range testCase.labels {
				labels[key] = value
			}
			object := &unstructured.Unstructured{Object: map[string]any{
				"metadata": map[string]any{"name": testCase.objectName, "labels": labels},
			}}
			fact := &v1beta1.NodeDevice{
				ObjectMeta: metav1.ObjectMeta{Name: testCase.objectName},
				Spec:       v1beta1.NodeDeviceSpec{Node: testCase.node, ObservedAt: testCase.observedAt},
			}
			invalid, stale := FactMarkers(object, fact, now)
			if invalid != testCase.invalid || stale != testCase.stale {
				t.Fatalf("FactMarkers = (%v, %v), want (%v, %v)", invalid, stale, testCase.invalid, testCase.stale)
			}
		})
	}
}

// moment wraps one instant for the observation field.
func moment(at time.Time) *metav1.Time {
	moment := metav1.NewTime(at)
	return &moment
}
