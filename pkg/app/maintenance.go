package app

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/write"
)

// NodeSource reports whether the run kernel has taken a node out of scheduling
// (C-65). A controller without one reads the node through its store.
type NodeSource interface {
	IsUnschedulable(ctx context.Context, name string) (bool, error)
}

// storeNodes reads the maintenance state of a node through the store (C-65).
type storeNodes struct {
	store Store
}

// IsUnschedulable reports whether the run kernel took the node out of
// scheduling. A node the cluster does not have is not in maintenance: node loss
// belongs to the volume module (C-38).
func (n storeNodes) IsUnschedulable(ctx context.Context, name string) (bool, error) {
	stored, err := n.store.Get(ctx, write.Ref{Version: "v1", Kind: "Node", Name: name})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	unschedulable, _, _ := unstructured.NestedBool(stored.Object, "spec", "unschedulable")
	return unschedulable, nil
}

// maintenanceNode reports the node whose maintenance must stop this
// application: a declared node-local volume whose data sits on an unschedulable
// node (C-65).
func (c *Controller) maintenanceNode(ctx context.Context, application *v1beta1.Application) (string, error) {
	for _, name := range application.Spec.Volumes {
		facts, err := c.volumes.VolumeFacts(ctx, application.Spec.Namespace, name)
		if err != nil {
			return "", err
		}
		if facts.Mobility != v1beta1.VolumeMobilityNodeLocal || facts.Node == "" {
			continue
		}
		unschedulable, err := c.nodes.IsUnschedulable(ctx, facts.Node)
		if err != nil {
			return "", err
		}
		if unschedulable {
			return facts.Node, nil
		}
	}
	return "", nil
}

// stopForMaintenance drives the workloads to zero for a node the run kernel
// took out of scheduling, records why, and holds the phase (C-65, C-66).
func (c *Controller) stopForMaintenance(ctx context.Context, application *v1beta1.Application, node string, comparison CapabilityResult, now time.Time) (reconcile.Result, error) {
	return c.stop(ctx, application, stopCause{
		reason:  ReasonNodeMaintenance,
		message: fmt.Sprintf("the workloads are stopped because node %q is in maintenance", node),
	}, comparison, now)
}
