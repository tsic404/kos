package volume

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/write"
)

// ExpansionPlan answers the three questions C-38 requires before a claim is
// expanded. A question the kernel cannot answer from the backend makes
// DowntimeRequired true: an unanswerable preflight never promises online
// growth.
type ExpansionPlan struct {
	// BackendOnline reports whether the storage backend expands online.
	BackendOnline bool
	// FilesystemOnline reports whether the filesystem grows online.
	FilesystemOnline bool
	// EstimatedDuration is how long the expansion is expected to take.
	EstimatedDuration time.Duration
	// DowntimeRequired is true when any of the three answers is unavailable.
	DowntimeRequired bool
}

// The estimate is the planning number the kernel reports, not a promise: an
// expansion moves about expansionRate per second on the backends the kernel
// targets, with a floor for the round trip itself.
const (
	expansionRate    = 100 << 20
	minimumExpansion = 30 * time.Second
)

// onlineFilesystems are the filesystems that grow while mounted (C-38).
var onlineFilesystems = map[string]bool{"ext4": true, "xfs": true}

// ExpansionPreflight answers C-38's three questions for one volume. It reads
// the claim's current size, so the estimate covers the size actually added.
func (c *Controller) ExpansionPreflight(ctx context.Context, namespace, name string) (ExpansionPlan, error) {
	ref := write.Ref{Group: api.Group, Version: "v1beta1", Kind: "Volume", Namespace: namespace, Name: name}
	stored, err := c.store.Get(ctx, ref)
	if err != nil {
		return ExpansionPlan{}, err
	}
	volume := &v1beta1.Volume{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, volume); err != nil {
		return ExpansionPlan{}, fmt.Errorf("decode the volume: %w", err)
	}
	capacity, problem := validateSpec(volume.Spec)
	if problem != "" {
		return ExpansionPlan{DowntimeRequired: true}, nil
	}
	class, err := c.storageClass(ctx, volume.Spec.StorageClass)
	if err != nil {
		return ExpansionPlan{}, err
	}
	delta, known := requestDelta(ctx, c.store, volume, capacity)
	plan := planFor(delta, class)
	if !known {
		// The claim or its size could not be read, so the added size and thus
		// the estimate are unknown (C-38).
		plan.DowntimeRequired = true
		plan.EstimatedDuration = 0
	}
	return plan, nil
}

// requestDelta is the size the expansion would add; a claim already at or above
// the requested size has nothing to add. known is false when the claim or its
// requested size cannot be read.
func requestDelta(ctx context.Context, store Store, volume *v1beta1.Volume, capacity resource.Quantity) (resource.Quantity, bool) {
	claimName := volume.Name
	if volume.Status.BoundClaim != "" {
		claimName = volume.Status.BoundClaim
	}
	claim, err := store.GetResource(ctx, pvcResource, volume.Namespace, claimName)
	if err != nil {
		return resource.Quantity{}, false
	}
	requested, found, err := unstructured.NestedString(claim.Object, "spec", "resources", "requests", "storage")
	if err != nil || !found {
		return resource.Quantity{}, false
	}
	current, err := resource.ParseQuantity(requested)
	if err != nil {
		return resource.Quantity{}, false
	}
	delta := capacity.DeepCopy()
	delta.Sub(current)
	if delta.Sign() < 0 {
		return resource.Quantity{}, true
	}
	return delta, true
}

// planFor turns the backend policy and the added size into the three answers.
// A missing class answers nothing, so the plan requires downtime.
func planFor(delta resource.Quantity, class *storageClass) ExpansionPlan {
	plan := ExpansionPlan{}
	if class != nil {
		plan.BackendOnline = class.AllowVolumeExpansion
		plan.FilesystemOnline = onlineFilesystems[class.Parameters["fsType"]]
	}
	if plan.BackendOnline && plan.FilesystemOnline {
		bytes := delta.Value()
		estimate := time.Duration(bytes) * time.Second / expansionRate
		if estimate < minimumExpansion {
			estimate = minimumExpansion
		}
		plan.EstimatedDuration = estimate
	}
	plan.DowntimeRequired = !plan.BackendOnline || !plan.FilesystemOnline
	return plan
}
