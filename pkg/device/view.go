package device

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
)

// View is the read-side device view: one entry per device fact entry and the
// warnings the view must show with them (C-37, C-50).
type View struct {
	// Devices lists the observed devices in fact order.
	Devices []Device
	// Warnings lists the invalid and stale facts the view must surface.
	Warnings []string
}

// Device is one device as the view presents it.
type Device struct {
	// Node is the node the fact belongs to.
	Node string
	// Name is the device name on the node.
	Name string
	// Serial is the device serial number.
	Serial string
	// Model is the device model.
	Model string
	// Kind is the device class.
	Kind string
	// The four layers are independent: none is derived from another (C-37).
	IsPresent     bool
	IsRecognized  bool
	IsDriverReady bool
	IsPublished   bool
	// IsOccupied tells that a storage, device or network system holds it.
	IsOccupied bool
	// OccupancySource names the holding system.
	OccupancySource string
	// OccupancyReference names the holding object inside that system.
	OccupancyReference string
	// IsStale tells that the fact is no longer refreshed.
	IsStale bool
	// IsInvalid tells that the fact is outside the collection boundary.
	IsInvalid bool
	// IsAllocatable tells that the device may be presented as free.
	IsAllocatable bool
}

// FactMarkers reports the stale and invalid state of one stored device fact: a
// fact is invalid when the marker exists or its node field differs from the
// object name, and stale when the marker exists or its observation is older than
// StaleAfter (C-37).
func FactMarkers(object *unstructured.Unstructured, fact *v1beta1.NodeDevice, now time.Time) (invalid, stale bool) {
	return object.GetLabels()[markers.Invalid] != "" || fact.Spec.Node != fact.Name,
		object.GetLabels()[markers.Stale] != "" || isOlderThan(fact.Spec.ObservedAt, StaleAfter, now)
}

// BuildView turns stored device facts into the device view. A fact whose node
// field differs from its name is invalid even when the marker has not been
// written yet (C-37).
func BuildView(facts []*unstructured.Unstructured, now time.Time) View {
	var view View
	for _, object := range facts {
		node := &v1beta1.NodeDevice{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, node); err != nil {
			view.Warnings = append(view.Warnings, fmt.Sprintf("无法读取设备事实 %s", object.GetName()))
			continue
		}
		invalid, stale := FactMarkers(object, node, now)
		if invalid {
			view.Warnings = append(view.Warnings, fmt.Sprintf("节点 %s 的设备事实被标记为无效", node.Name))
		}
		if stale {
			view.Warnings = append(view.Warnings, fmt.Sprintf("节点 %s 的设备事实已陈旧", node.Name))
		}
		for _, fact := range node.Spec.Devices {
			occupied := fact.Occupancy != nil
			presented := Device{
				Node:          node.Name,
				Name:          fact.Name,
				Serial:        fact.Serial,
				Model:         fact.Model,
				Kind:          fact.Kind,
				IsPresent:     fact.IsPresent,
				IsRecognized:  fact.IsRecognized,
				IsDriverReady: fact.IsDriverReady,
				IsPublished:   fact.IsPublished,
				IsOccupied:    occupied,
				IsStale:       stale,
				IsInvalid:     invalid,
			}
			if occupied {
				presented.OccupancySource = fact.Occupancy.Source
				presented.OccupancyReference = fact.Occupancy.Reference
			}
			presented.IsAllocatable = allocatable(presented)
			view.Devices = append(view.Devices, presented)
		}
	}
	return view
}

// Allocatable returns the devices the view may present as free. An invalid
// fact takes no part in scheduling derivation (C-37); staleness is a freshness
// mark of the read projection and does not by itself change the fact.
func (v View) Allocatable() []Device {
	free := make([]Device, 0, len(v.Devices))
	for _, device := range v.Devices {
		if device.IsAllocatable {
			free = append(free, device)
		}
	}
	return free
}

// allocatable applies C-37 to one presented device: every layer holds, no
// system holds the device, and its fact is valid.
func allocatable(device Device) bool {
	if device.IsInvalid || device.IsOccupied {
		return false
	}
	return device.IsPresent && device.IsRecognized && device.IsDriverReady && device.IsPublished
}

// isOlderThan reports whether one observation is older than the bound.
func isOlderThan(observed *metav1.Time, bound time.Duration, now time.Time) bool {
	if observed == nil {
		return false
	}
	return now.Sub(observed.Time) > bound
}
