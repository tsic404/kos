// Package device implements the node side of the device fact contract: one
// collector writes the fact object of its own node, the validator marks facts
// that fall outside the collection boundary, a maintainer refreshes the
// validity markers and reclaims a vanished node's fact, and the read side
// builds the device view.
package device

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

const (
	// StaleAfter is how long a device fact may go unrefreshed before KOS marks
	// it stale (C-37).
	StaleAfter = 24 * time.Hour
	// ReclaimAfter is how long the fact of a vanished node is kept before KOS
	// reclaims it (C-37).
	ReclaimAfter = 7 * 24 * time.Hour
	// markerValue is the value a validity marker carries.
	markerValue = "true"
)

// nodeDeviceResource is the class the facts live in.
var nodeDeviceResource = schema.GroupVersionResource{
	Group:    api.Group,
	Version:  v1beta1.GroupVersion.Version,
	Resource: "nodedevices",
}

// Collector writes the device fact object of one node (C-37). Node is the
// collector's hostname: the object it writes carries that name and must name
// that node in its node field. Identity is recorded in the object and defaults
// to Node.
type Collector struct {
	// Writer executes the writes.
	Writer *write.Writer
	// Node is the collector's hostname and the collection identity recorded on
	// the object.
	Node string
	// Now supplies the observation time; nil means the wall clock.
	Now func() time.Time
}

// Report is one collection round: the target object, the node the object names
// and the device entries observed. A legal round has Object and Node equal to
// the collector's hostname.
type Report struct {
	// Object is the name of the NodeDevice the round writes.
	Object string
	// Node is the value of the fact's node field.
	Node string
	// Devices are the hardware facts observed in this round.
	Devices []v1beta1.DeviceFact
}

// OutOfBounds reports a write outside the collector's own hostname (C-37).
type OutOfBounds struct {
	// Collector is the hostname the attempt was made under.
	Collector string
	// Object is the object the attempt addressed.
	Object string
	// Node is the node field the attempt carried.
	Node string
}

// Error names both sides of the boundary violation.
func (e *OutOfBounds) Error() string {
	return fmt.Sprintf("collector %q cannot write device fact %q for node %q", e.Collector, e.Object, e.Node)
}

// Collect writes one round of device facts. A round outside the collector's
// hostname is refused and the object it addressed, when present, is marked
// invalid (C-37); the facts of that object no longer take part in scheduling
// derivation.
func (c *Collector) Collect(ctx context.Context, report Report) (*unstructured.Unstructured, error) {
	if c.Writer == nil {
		return nil, errors.New("device: a writer is required")
	}
	if c.Node == "" {
		return nil, errors.New("device: the collector has no hostname")
	}
	if report.Object == "" {
		report.Object = report.Node
	}
	if report.Node == "" {
		report.Node = report.Object
	}
	if report.Object == "" {
		return nil, errors.New("device: the report names no object")
	}
	if report.Object != c.Node || report.Node != report.Object {
		outOfBounds := &OutOfBounds{Collector: c.Node, Object: report.Object, Node: report.Node}
		ref := factRef(report.Object)
		validator := &Validator{Writer: c.Writer}
		if err := validator.MarkInvalid(ctx, ref); err != nil && !apierrors.IsNotFound(err) {
			return nil, errors.Join(outOfBounds, err)
		}
		return nil, outOfBounds
	}

	now := c.now()
	devices := KeepLatest(report.Devices)
	for index := range devices {
		if devices[index].ObservedAt == nil {
			devices[index].ObservedAt = &metav1.Time{Time: now}
		}
	}
	spec := v1beta1.NodeDeviceSpec{
		Node:       c.Node,
		Collector:  c.Node,
		ObservedAt: &metav1.Time{Time: now},
		Devices:    devices,
	}
	value, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		return nil, fmt.Errorf("encode device fact of %s: %w", c.Node, err)
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "NodeDevice",
		"metadata": map[string]any{
			"name":   c.Node,
			"labels": map[string]any{markers.ManagedBy: write.ManagedByValue},
		},
		"spec": value,
	}}
	return c.Writer.Apply(ctx, write.SubjectDiscovery, write.Session{}, write.PartSpec, object)
}

// KeepLatest keeps one entry per serial number: the one observed last. Entries
// without a serial number cannot be compared and are kept as they are (C-37).
func KeepLatest(devices []v1beta1.DeviceFact) []v1beta1.DeviceFact {
	positions := map[string]int{}
	kept := make([]v1beta1.DeviceFact, 0, len(devices))
	for _, fact := range devices {
		if fact.Serial == "" {
			kept = append(kept, fact)
			continue
		}
		index, seen := positions[fact.Serial]
		if !seen {
			positions[fact.Serial] = len(kept)
			kept = append(kept, fact)
			continue
		}
		if factObservedAt(fact).After(factObservedAt(kept[index])) {
			kept[index] = fact
		}
	}
	return kept
}

// Validator writes the validity markers of C-37. It is the subject the write
// allocation grants the validity marker class to.
type Validator struct {
	// Writer executes the marker writes.
	Writer *write.Writer
}

// MarkInvalid marks a device fact invalid. The mark is sticky: a fact that was
// written outside the boundary does not become trusted again by a later write.
func (v *Validator) MarkInvalid(ctx context.Context, ref write.Ref) error {
	return v.mark(ctx, ref, markers.Invalid, true)
}

// MarkStale marks a device fact as no longer refreshed.
func (v *Validator) MarkStale(ctx context.Context, ref write.Ref) error {
	return v.mark(ctx, ref, markers.Stale, true)
}

// ClearStale removes the stale mark of a fact that is refreshed again.
func (v *Validator) ClearStale(ctx context.Context, ref write.Ref) error {
	return v.mark(ctx, ref, markers.Stale, false)
}

// CheckBoundary checks the stored boundary rule: the object's node field must
// equal its name. A violation marks the object invalid (C-37); the boolean says
// whether one was found.
func (v *Validator) CheckBoundary(ctx context.Context, object *unstructured.Unstructured) (bool, error) {
	node, _, err := unstructured.NestedString(object.Object, "spec", "node")
	if err != nil {
		return false, fmt.Errorf("read node field of %s: %w", object.GetName(), err)
	}
	if node == object.GetName() {
		return false, nil
	}
	return true, v.MarkInvalid(ctx, write.RefOf(object))
}

// mark sets or clears one validity marker, writing only when the stored value
// differs so repeated rounds produce no write (C-06).
func (v *Validator) mark(ctx context.Context, ref write.Ref, key string, present bool) error {
	if v.Writer == nil {
		return errors.New("device: a writer is required")
	}
	current, err := v.Writer.Get(ctx, ref)
	if err != nil {
		return err
	}
	_, hasMarker := current.GetLabels()[key]
	if hasMarker == present {
		return nil
	}
	set := map[string]string{}
	var remove []string
	if present {
		set[key] = markerValue
	} else {
		remove = []string{key}
	}
	_, err = v.Writer.MarkOn(ctx, write.SubjectValidator, write.Session{}, current, set, remove)
	return err
}

// now returns the observation time of this round.
func (c *Collector) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now().UTC()
}

// factRef builds the reference of one device fact object.
func factRef(name string) write.Ref {
	return write.Ref{
		Group:   api.Group,
		Version: v1beta1.GroupVersion.Version,
		Kind:    "NodeDevice",
		Name:    name,
	}
}

// factObservedAt reads one entry's observation time; a missing value is the
// zero time, so it loses to every stamped entry.
func factObservedAt(fact v1beta1.DeviceFact) time.Time {
	if fact.ObservedAt == nil {
		return time.Time{}
	}
	return fact.ObservedAt.Time
}
