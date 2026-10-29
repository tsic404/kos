// Package tc82 verifies the two device-fact clocks: an unrefreshed fact is
// marked stale after the freshness bound, and the fact of a node that has been
// gone for the reclaim window is removed (AC-41, TC-82).
package tc82

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/device"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC82StaleAndReclaim stops collection past the freshness bound, then
// removes the node and waits out the reclaim window.
func TestTC82StaleAndReclaim(t *testing.T) {
	cluster := harness.Start(t)
	writer := cluster.Writer()
	ctx := context.Background()
	start := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	now := start
	clock := func() time.Time { return now }

	if _, err := cluster.Kube.CoreV1().Nodes().Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create node-a: %v", err)
	}
	if _, err := (&device.Collector{Writer: writer, Node: "node-a", Now: clock}).Collect(ctx, device.Report{
		Object: "node-a", Node: "node-a",
		Devices: []v1beta1.DeviceFact{{
			Name: "sda", Serial: "DISK-1", Model: "X", Kind: "disk",
			IsPresent: true, IsRecognized: true, IsDriverReady: true, IsPublished: true,
		}},
	}); err != nil {
		t.Fatalf("write the device fact: %v", err)
	}

	maintainer := device.NewMaintainer(writer, func(ctx context.Context, name string) (bool, error) {
		_, err := cluster.Kube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	})
	maintainer.Now = clock

	// Collection stops; after the freshness bound the fact is stale.
	now = start.Add(25 * time.Hour)
	if err := maintainer.Run(ctx); err != nil {
		t.Fatalf("maintain after the freshness bound: %v", err)
	}
	stale := cluster.Get(t, "v1beta1", "NodeDevice", "node-a")
	if got := stale.GetLabels()[markers.Stale]; got == "" {
		t.Fatalf("an unrefreshed fact carries no stale marker: %v", stale.GetLabels())
	}

	// Collection stopped long ago; the node object disappears eight days after
	// the last observation. The reclaim window starts at the round that sees
	// the absence, not at the stale observation (C-37).
	disappeared := start.Add(8 * 24 * time.Hour)
	now = disappeared
	if err := cluster.Kube.CoreV1().Nodes().Delete(ctx, "node-a", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete node-a: %v", err)
	}
	if err := maintainer.Run(ctx); err != nil {
		t.Fatalf("maintain after the node disappeared: %v", err)
	}
	if _, err := cluster.GetObject(ctx, "v1beta1", "NodeDevice", "", "node-a"); err != nil {
		t.Fatalf("the fact was reclaimed in the round the node disappeared: %v", err)
	}

	// One hour before the window ends the fact is still kept.
	now = disappeared.Add(device.ReclaimAfter - time.Hour)
	if err := maintainer.Run(ctx); err != nil {
		t.Fatalf("maintain before the reclaim window ended: %v", err)
	}
	if _, err := cluster.GetObject(ctx, "v1beta1", "NodeDevice", "", "node-a"); err != nil {
		t.Fatalf("the fact was reclaimed before the window: %v", err)
	}

	// After the window the fact is reclaimed.
	now = disappeared.Add(device.ReclaimAfter + time.Hour)
	if err := maintainer.Run(ctx); err != nil {
		t.Fatalf("maintain after the reclaim window: %v", err)
	}
	_, err := cluster.GetObject(ctx, "v1beta1", "NodeDevice", "", "node-a")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the fact of a vanished node was not reclaimed: %v", err)
	}
}
