// Package tc81 verifies that a collector cannot write another node's device
// fact: the object is marked invalid, its facts leave scheduling derivation and
// the device view warns (AC-41, TC-81).
package tc81

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/device"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC81OutOfBoundsWrite writes another node's object under one node's
// collection identity.
func TestTC81OutOfBoundsWrite(t *testing.T) {
	cluster := harness.Start(t)
	writer := cluster.Writer()
	ctx := context.Background()
	now := time.Now().UTC()
	clock := func() time.Time { return now }

	gpu := v1beta1.DeviceFact{
		Name: "gpu0", Serial: "GPU-1", Model: "A100", Kind: "gpu",
		IsPresent: true, IsRecognized: true, IsDriverReady: true, IsPublished: true,
	}
	if _, err := (&device.Collector{Writer: writer, Node: "node-b", Now: clock}).Collect(ctx, device.Report{
		Object: "node-b", Node: "node-b", Devices: []v1beta1.DeviceFact{gpu},
	}); err != nil {
		t.Fatalf("write the legal fact of node-b: %v", err)
	}
	before := cluster.Get(t, "v1beta1", "NodeDevice", "node-b")

	// node-a tries to rewrite node-b's object.
	intruder := &device.Collector{Writer: writer, Node: "node-a", Now: clock}
	_, err := intruder.Collect(ctx, device.Report{
		Object: "node-b", Node: "node-b",
		Devices: []v1beta1.DeviceFact{{
			Name: "gpu0", Serial: "GPU-1", Model: "A100", Kind: "gpu", IsPresent: true,
		}},
	})
	var outOfBounds *device.OutOfBounds
	if !errors.As(err, &outOfBounds) {
		t.Fatalf("a write outside the collector hostname was accepted: %v", err)
	}

	after := cluster.Get(t, "v1beta1", "NodeDevice", "node-b")
	if got := after.GetLabels()[markers.Invalid]; got == "" {
		t.Fatalf("the out-of-bounds object carries no invalid marker: %v", after.GetLabels())
	}
	if got, want := specOf(t, after), specOf(t, before); !reflect.DeepEqual(got, want) {
		t.Fatalf("the refused write changed the facts: %v != %v", got, want)
	}

	view := device.BuildView(cluster.List(t, "v1beta1", "NodeDevice"), now)
	if len(view.Warnings) == 0 {
		t.Fatal("the device view shows no warning for an invalid fact")
	}
	if !strings.Contains(strings.Join(view.Warnings, " "), "无效") {
		t.Fatalf("the device view warning does not name the invalid fact: %v", view.Warnings)
	}
	if len(view.Devices) != 1 || !view.Devices[0].IsInvalid {
		t.Fatalf("the invalid fact is not presented as invalid: %+v", view.Devices)
	}
	if free := view.Allocatable(); len(free) != 0 {
		t.Fatalf("an invalid fact took part in scheduling derivation: %+v", free)
	}
}

// specOf returns a copy of the object's spec.
func specOf(t *testing.T, object *unstructured.Unstructured) map[string]any {
	t.Helper()
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil || !found {
		t.Fatalf("read spec of %s: %v", object.GetName(), err)
	}
	return spec
}
