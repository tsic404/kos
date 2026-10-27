// Package tc131 verifies that duplicate records of one serial number keep the
// newest entry in the stored object and in the device view (AC-41, TC-131).
package tc131

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/device"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC131DuplicateSerialKeepsNewest writes two entries with one serial and
// different observation times.
func TestTC131DuplicateSerialKeepsNewest(t *testing.T) {
	cluster := harness.Start(t)
	writer := cluster.Writer()
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	older := now.Add(-time.Hour)
	newer := now.Add(-time.Minute)
	if _, err := (&device.Collector{Writer: writer, Node: "node-a", Now: func() time.Time { return now }}).Collect(ctx, device.Report{
		Object: "node-a", Node: "node-a",
		Devices: []v1beta1.DeviceFact{
			{
				Name: "old", Serial: "DISK-1", Model: "X", Kind: "disk",
				IsPresent: true, ObservedAt: &metav1.Time{Time: older},
			},
			{
				Name: "new", Serial: "DISK-1", Model: "X", Kind: "disk",
				IsPresent: true, ObservedAt: &metav1.Time{Time: newer},
			},
		},
	}); err != nil {
		t.Fatalf("write the duplicate records: %v", err)
	}

	stored := cluster.Get(t, "v1beta1", "NodeDevice", "node-a")
	entries, found, err := unstructured.NestedSlice(stored.Object, "spec", "devices")
	if err != nil || !found {
		t.Fatalf("read the stored entries: found=%v err=%v", found, err)
	}
	if len(entries) != 1 {
		t.Fatalf("the object keeps %d entries, want only the newest one", len(entries))
	}
	entry, _ := entries[0].(map[string]any)
	if name, _ := entry["name"].(string); name != "new" {
		t.Fatalf("the object kept %q, want the newer entry", name)
	}

	view := device.BuildView(cluster.List(t, "v1beta1", "NodeDevice"), now)
	if len(view.Devices) != 1 {
		t.Fatalf("the device view shows %d devices, want only the newest one", len(view.Devices))
	}
	if view.Devices[0].Name != "new" {
		t.Fatalf("the device view shows %q, want the newer entry", view.Devices[0].Name)
	}
}
