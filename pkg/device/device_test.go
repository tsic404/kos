package device_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/device"
)

// TestKeepLatestKeepsNewestSerial covers the duplicate rule of C-37: one entry
// per serial number, the one observed last.
func TestKeepLatestKeepsNewestSerial(t *testing.T) {
	older := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	kept := device.KeepLatest([]v1beta1.DeviceFact{
		{Name: "old", Serial: "S-1", Model: "A", ObservedAt: &metav1.Time{Time: older}},
		{Name: "new", Serial: "S-1", Model: "B", ObservedAt: &metav1.Time{Time: newer}},
		{Name: "other", Serial: "S-2", Model: "A", ObservedAt: &metav1.Time{Time: older}},
		{Name: "serial-less", ObservedAt: &metav1.Time{Time: older}},
	})
	if len(kept) != 3 {
		t.Fatalf("kept %d entries, want 3: %+v", len(kept), kept)
	}
	if kept[0].Name != "new" || kept[0].Model != "B" {
		t.Fatalf("duplicate serial kept %+v, want the newest entry", kept[0])
	}
	if kept[1].Name != "other" || kept[2].Name != "serial-less" {
		t.Fatalf("unrelated entries changed: %+v", kept)
	}
}

// TestViewDoesNotInferLayers covers the rule that one missing layer does not
// imply another and that such a device is not presented as free (C-37).
func TestViewDoesNotInferLayers(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "NodeDevice",
		"metadata":   map[string]any{"name": "node-a", "labels": map[string]any{}},
		"spec": map[string]any{
			"node":       "node-a",
			"observedAt": "2026-10-25T00:00:00Z",
			"devices": []any{map[string]any{
				"name": "gpu0", "present": true, "recognized": true, "driverReady": true, "published": false,
			}},
		},
	}}
	view := device.BuildView([]*unstructured.Unstructured{object}, time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	if len(view.Devices) != 1 {
		t.Fatalf("view shows %d devices, want 1", len(view.Devices))
	}
	presented := view.Devices[0]
	if presented.IsPublished {
		t.Fatal("published layer = true, want false")
	}
	if presented.IsAllocatable {
		t.Fatal("a device whose published layer does not hold is presented as allocatable")
	}
}
