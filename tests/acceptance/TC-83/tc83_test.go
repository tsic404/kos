// Package tc83 verifies that an occupied device is presented with its holding
// system, an unused device of the same model is presented as free, both appear
// in the device view and the free entry carries no occupancy structure (AC-41,
// TC-83).
package tc83

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/device"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC83Occupancy puts one disk into a storage pool and leaves its twin
// unused.
func TestTC83Occupancy(t *testing.T) {
	cluster := harness.Start(t)
	writer := cluster.Writer()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := (&device.Collector{Writer: writer, Node: "node-a", Now: func() time.Time { return now }}).Collect(ctx, device.Report{
		Object: "node-a", Node: "node-a",
		Devices: []v1beta1.DeviceFact{
			{
				Name: "sda", Serial: "DISK-1", Model: "X", Kind: "disk",
				IsPresent: true, IsRecognized: true, IsDriverReady: true, IsPublished: true,
				Occupancy: &v1beta1.DeviceOccupancy{Source: "storage-pool", Reference: "pool-1"},
			},
			{
				Name: "sdb", Serial: "DISK-2", Model: "X", Kind: "disk",
				IsPresent: true, IsRecognized: true, IsDriverReady: true, IsPublished: true,
			},
		},
	}); err != nil {
		t.Fatalf("write the device facts: %v", err)
	}

	view := device.BuildView(cluster.List(t, "v1beta1", "NodeDevice"), now)
	if len(view.Devices) != 2 {
		t.Fatalf("the device view shows %d devices, want 2", len(view.Devices))
	}
	occupied := deviceNamed(t, view, "sda")
	if !occupied.IsOccupied || occupied.OccupancySource != "storage-pool" {
		t.Fatalf("the pooled disk is not presented as occupied by its source: %+v", occupied)
	}
	if occupied.IsAllocatable {
		t.Fatalf("an occupied disk is presented as freely allocatable: %+v", occupied)
	}
	free := deviceNamed(t, view, "sdb")
	if free.IsOccupied || !free.IsAllocatable {
		t.Fatalf("the unused disk is not presented as free: %+v", free)
	}
	allocatable := view.Allocatable()
	if len(allocatable) != 1 || allocatable[0].Name != "sdb" {
		t.Fatalf("scheduling derivation = %+v, want only sdb", allocatable)
	}

	// The wire format agrees with the view: the free entry carries no occupancy
	// structure, the occupied one does (C-70).
	stored := cluster.Get(t, "v1beta1", "NodeDevice", "node-a")
	entries, found, err := unstructured.NestedSlice(stored.Object, "spec", "devices")
	if err != nil || !found {
		t.Fatalf("read the stored entries: found=%v err=%v", found, err)
	}
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		name, _ := entry["name"].(string)
		_, hasOccupancy := entry["occupancy"]
		switch name {
		case "sda":
			if !hasOccupancy {
				t.Fatalf("the pooled disk entry carries no occupancy: %v", entry)
			}
		case "sdb":
			if hasOccupancy {
				t.Fatalf("the unused disk entry carries an occupancy structure: %v", entry)
			}
		}
	}
}

// deviceNamed finds one presented device by name.
func deviceNamed(t *testing.T, view device.View, name string) device.Device {
	t.Helper()
	for _, presented := range view.Devices {
		if presented.Name == name {
			return presented
		}
	}
	t.Fatalf("the device view does not show %s: %+v", name, view.Devices)
	return device.Device{}
}
