package kosdevicemanager

import (
	"context"
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/device"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/pages/cluster"
)

// Readings resolves the items the device page declares (C-53). Every item is
// read from the device facts the collector wrote; nothing here writes.
type Readings struct {
	// Dynamic reads the cluster.
	Dynamic dynamic.Interface
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
}

// Read implements pages.Readings.
func (r *Readings) Read(ctx context.Context, session identity.Session, item extension.PageItem) (pages.Reading, *controlplane.Error) {
	value, failure := r.value(ctx, item)
	if failure != nil {
		return pages.Reading{}, failure
	}
	return pages.Reading{Value: value, ObservedAt: r.reader().Clock()}, nil
}

// value resolves one item from the source it declares.
func (r *Readings) value(ctx context.Context, item extension.PageItem) (any, *controlplane.Error) {
	switch item.ID {
	case ItemHardware:
		return r.hardware(ctx)
	case ItemHealth:
		return r.health(ctx)
	case ItemMarkers:
		return r.markers(ctx)
	default:
		return nil, unknownItem(item.ID)
	}
}

// reader is the cluster read seam for this call.
func (r *Readings) reader() *cluster.Reader {
	return &cluster.Reader{Dynamic: r.Dynamic, Now: r.Now}
}

// hardwareDevice is one device fact entry as the page shows it.
type hardwareDevice struct {
	Node               string `json:"node"`
	Name               string `json:"name"`
	Serial             string `json:"serial,omitempty"`
	Model              string `json:"model,omitempty"`
	Kind               string `json:"kind,omitempty"`
	Present            bool   `json:"present"`
	Recognized         bool   `json:"recognized"`
	DriverReady        bool   `json:"driverReady"`
	Published          bool   `json:"published"`
	Occupied           bool   `json:"occupied"`
	OccupancySource    string `json:"occupancySource,omitempty"`
	OccupancyReference string `json:"occupancyReference,omitempty"`
	Stale              bool   `json:"stale"`
	Invalid            bool   `json:"invalid"`
	Allocatable        bool   `json:"allocatable"`
}

// hardware reads the node hardware and GPU facts (C-37, C-50).
func (r *Readings) hardware(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.facts(ctx)
	if failure != nil {
		return nil, failure
	}
	unstructuredObjects := make([]*unstructured.Unstructured, 0, len(objects))
	for _, object := range objects {
		unstructuredObjects = append(unstructuredObjects, &unstructured.Unstructured{Object: object})
	}
	view := device.BuildView(unstructuredObjects, r.reader().Clock())
	devices := make([]hardwareDevice, 0, len(view.Devices))
	for _, entry := range view.Devices {
		devices = append(devices, hardwareDevice{
			Node: entry.Node, Name: entry.Name, Serial: entry.Serial, Model: entry.Model, Kind: entry.Kind,
			Present: entry.IsPresent, Recognized: entry.IsRecognized, DriverReady: entry.IsDriverReady,
			Published: entry.IsPublished, Occupied: entry.IsOccupied, OccupancySource: entry.OccupancySource,
			OccupancyReference: entry.OccupancyReference, Stale: entry.IsStale, Invalid: entry.IsInvalid,
			Allocatable: entry.IsAllocatable,
		})
	}
	return map[string]any{"devices": devices, "warnings": view.Warnings}, nil
}

// healthAlert is one device health alert.
type healthAlert struct {
	Node   string `json:"node"`
	Device string `json:"device"`
	Kind   string `json:"kind,omitempty"`
	Detail string `json:"detail"`
}

// health computes the device health alerts C-50 names: the four fact layers of
// C-37 are compared per device and a missing layer is an alert on its own. The
// layers are never derived from one another.
func (r *Readings) health(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.facts(ctx)
	if failure != nil {
		return nil, failure
	}
	unstructuredObjects := make([]*unstructured.Unstructured, 0, len(objects))
	for _, object := range objects {
		unstructuredObjects = append(unstructuredObjects, &unstructured.Unstructured{Object: object})
	}
	view := device.BuildView(unstructuredObjects, r.reader().Clock())
	alerts := make([]healthAlert, 0)
	for _, entry := range view.Devices {
		switch {
		case !entry.IsPresent:
			alerts = append(alerts, healthAlert{Node: entry.Node, Device: entry.Name, Kind: entry.Kind, Detail: "硬件未上报存在"})
		case !entry.IsRecognized:
			alerts = append(alerts, healthAlert{Node: entry.Node, Device: entry.Name, Kind: entry.Kind, Detail: "Linux 未识别该硬件"})
		case !entry.IsDriverReady:
			alerts = append(alerts, healthAlert{Node: entry.Node, Device: entry.Name, Kind: entry.Kind, Detail: "驱动未就绪"})
		case !entry.IsPublished:
			alerts = append(alerts, healthAlert{Node: entry.Node, Device: entry.Name, Kind: entry.Kind, Detail: "尚未发布为可调度资源"})
		}
	}
	sort.Slice(alerts, func(left, right int) bool {
		if alerts[left].Node != alerts[right].Node {
			return alerts[left].Node < alerts[right].Node
		}
		return alerts[left].Device < alerts[right].Device
	})
	return map[string]any{
		"rule":   "设备事实的存在、识别、驱动与发布四层缺失逐台列出，各层不互相推导（C-37、C-50）",
		"alerts": alerts,
	}, nil
}

// markerState is one fact's stale and invalid state.
type markerState struct {
	Node       string `json:"node"`
	Collector  string `json:"collector,omitempty"`
	ObservedAt string `json:"observedAt,omitempty"`
	Stale      bool   `json:"stale"`
	Invalid    bool   `json:"invalid"`
	Reason     string `json:"reason,omitempty"`
}

// markers reads the stale and invalid markers C-37 defines; a marker that has
// not been written yet is derived from the fact's age and boundary.
func (r *Readings) markers(ctx context.Context) (any, *controlplane.Error) {
	objects, failure := r.facts(ctx)
	if failure != nil {
		return nil, failure
	}
	now := r.reader().Clock()
	states := make([]markerState, 0, len(objects))
	for _, object := range objects {
		var fact v1beta1.NodeDevice
		if failure := cluster.Decode(object, &fact); failure != nil {
			return nil, failure
		}
		invalid, stale := device.FactMarkers(&unstructured.Unstructured{Object: object}, &fact, now)
		state := markerState{
			Node: fact.Name, Collector: fact.Spec.Collector,
			Stale: stale, Invalid: invalid,
		}
		if fact.Spec.ObservedAt != nil {
			state.ObservedAt = fact.Spec.ObservedAt.UTC().Format(time.RFC3339)
		}
		switch {
		case invalid:
			state.Reason = fmt.Sprintf("对象名字与节点字段不一致或已被标记无效：node=%s", fact.Spec.Node)
		case stale:
			state.Reason = fmt.Sprintf("设备事实超过 %s 未更新", device.StaleAfter)
		}
		states = append(states, state)
	}
	sort.Slice(states, func(left, right int) bool {
		return states[left].Node < states[right].Node
	})
	return map[string]any{"facts": states}, nil
}

// facts lists the stored device facts.
func (r *Readings) facts(ctx context.Context) ([]map[string]any, *controlplane.Error) {
	return r.reader().List(ctx, "NodeDevice", "")
}

// unknownItem marks an item this product does not declare.
func unknownItem(id string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeCapabilityMissing,
		Message:     "kos-devicemanager 没有声明页面条目 " + id,
		Stage:       controlplane.StageCapability,
		SideEffects: controlplane.SideEffectsNone,
	}
}
