package volume

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
)

// Native kinds and the policy the volume resolves its binding from.
var (
	pvcResource          = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	pvResource           = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
	nodeResource         = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	storageClassResource = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	volumeResource       = schema.GroupVersionResource{Group: "kos.tsic.top", Version: "v1beta1", Resource: "volumes"}
)

// hostnameLabel is the node affinity key a node-local binding resolves to the
// node the volume is pinned to (C-38).
const hostnameLabel = "kubernetes.io/hostname"

// storageClass is the resolved backend policy of a claim (C-38).
type storageClass struct {
	// Name is the storage class name.
	Name string
	// Provisioner is the driver the class names.
	Provisioner string
	// AllowVolumeExpansion reports whether the backend expands online.
	AllowVolumeExpansion bool
	// ReclaimPolicy is the class's policy, used until a claim is bound.
	ReclaimPolicy string
	// Parameters are the class parameters, read for the filesystem type.
	Parameters map[string]string
}

// storageClass reads the named class; a missing class is nil, not an error,
// so the round can report it as a blocked reason (C-38).
func (c *Controller) storageClass(ctx context.Context, name string) (*storageClass, error) {
	if name == "" {
		return nil, nil
	}
	object, err := c.store.GetResource(ctx, storageClassResource, "", name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	class := &storageClass{Name: name}
	class.Provisioner, _, _ = unstructured.NestedString(object.Object, "provisioner")
	class.AllowVolumeExpansion, _, _ = unstructured.NestedBool(object.Object, "allowVolumeExpansion")
	class.ReclaimPolicy, _, _ = unstructured.NestedString(object.Object, "reclaimPolicy")
	class.Parameters, _, _ = unstructured.NestedStringMap(object.Object, "parameters")
	return class, nil
}

// sharedCapability resolves the shared-rwx capability a shared volume needs.
func (c *Controller) sharedCapability(ctx context.Context) (string, bool, error) {
	if c.capabilities == nil {
		return "", false, nil
	}
	version, registered, err := c.capabilities.Version(ctx, CapabilitySharedRWX)
	if err != nil {
		return "", false, err
	}
	if version == "" {
		version = CapabilityVersionV1
	}
	return version, registered, nil
}

// observe reads the binding C-38 makes observable: the claim's phase, the
// persistent volume it resolved to, and for a node-local volume the node it is
// pinned to.
func (c *Controller) observe(ctx context.Context, volume *v1beta1.Volume, claim *unstructured.Unstructured, class *storageClass, sharedVersion string) (v1beta1.VolumeStatus, reconcile.Result, error) {
	status := baseStatus(volume)
	status.BoundClaim = claim.GetName()
	phase, _, _ := unstructured.NestedString(claim.Object, "status", "phase")
	pvName, _, _ := unstructured.NestedString(claim.Object, "spec", "volumeName")
	if phase != "Bound" || pvName == "" {
		status.Phase = PhasePending
		status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
		status.Conditions = clearFailures(status.Conditions)
		status.Conditions = state.Set(status.Conditions, state.Blocked(
			volume.Generation, "Binding", fmt.Sprintf("claim %q is not bound yet", claim.GetName())))
		return status, reconcile.Result{RequeueAfter: claimResync}, nil
	}

	pv, err := c.store.GetResource(ctx, pvResource, "", pvName)
	if apierrors.IsNotFound(err) {
		return lostStatus(volume, volume.Status.Node, fmt.Sprintf("persistent volume %q is missing", pvName)), reconcile.Result{RequeueAfter: boundResync}, nil
	}
	if err != nil {
		return lostStatus(volume, volume.Status.Node, fmt.Sprintf("persistent volume %q cannot be read: %v", pvName, err)), reconcile.Result{RequeueAfter: boundResync}, nil
	}

	status.Phase = PhaseBound
	status.Health = v1beta1.Health{State: v1beta1.HealthStateHealthy}
	status.Provider = backendOf(pv, class)
	status.ReclaimPolicy = reclaimPolicyOf(pv, class)
	status.Capabilities = capabilitiesOf(volume, sharedVersion)
	if volume.Spec.Mobility == v1beta1.VolumeMobilitySharedRWX {
		// A shared read-write volume can only be made crash-consistent (C-38).
		status.SnapshotConsistency = SnapshotConsistencyCrash
	}
	if volume.Spec.Mobility == v1beta1.VolumeMobilityNodeLocal {
		node := bindingNodeOf(pv)
		if node == "" {
			// The claim is bound but the node it is pinned to cannot be
			// resolved, so the confirmation window cannot run: the binding is
			// observed as unknown, never as healthy (C-07, C-38).
			status.Health = v1beta1.Health{State: v1beta1.HealthStateUnknown}
			status.Conditions = clearFailures(status.Conditions)
			status.Conditions = state.Set(status.Conditions, state.Unobserved(volume.Generation,
				fmt.Sprintf("the binding of claim %q names no resolvable node", claim.GetName())))
			return status, reconcile.Result{RequeueAfter: boundResync}, nil
		}
		status.Node = node
		lost, err := c.nodeLost(ctx, node)
		if err != nil {
			return v1beta1.VolumeStatus{}, reconcile.Result{}, err
		}
		if lost {
			message := fmt.Sprintf("node %q of the node-local binding was lost beyond the confirmation window", node)
			return lostStatus(volume, node, message), reconcile.Result{RequeueAfter: boundResync}, nil
		}
	}
	status.Conditions = clearFailures(status.Conditions)
	status.Conditions = state.Set(status.Conditions, state.Applied(volume.Generation, "the claim is bound"))
	return status, reconcile.Result{RequeueAfter: boundResync}, nil
}

// lostStatus is the Lost state of C-38: the volume is observable and its data
// is not. node is the binding node this round knows, or the one the status
// already carried; losing the volume does not lose that fact.
func lostStatus(volume *v1beta1.Volume, node, message string) v1beta1.VolumeStatus {
	status := baseStatus(volume)
	status.Phase = PhaseLost
	status.Node = node
	status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
	status.Conditions = state.Set(status.Conditions, state.Blocked(volume.Generation, ConditionLost, message))
	status.Conditions = withFailure(status.Conditions, volume.Generation, ConditionLost, message)
	return status
}

// nodeLost reports a node-local binding whose node has been unobservable for
// longer than the confirmation window: the window is the heartbeat grace period
// plus five minutes (C-38).
func (c *Controller) nodeLost(ctx context.Context, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	node, err := c.store.GetResource(ctx, nodeResource, "", name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	ready, since, found := nodeReady(node)
	if !found || ready || since.IsZero() {
		return false, nil
	}
	return c.clock().Sub(since) > c.nodeGrace+ConfirmationWindow, nil
}

// nodeReady reads the Ready condition and the last sign of life of a node. A
// node without the condition has not been observed at all, so its loss cannot
// be dated.
func nodeReady(node *unstructured.Unstructured) (ready bool, since time.Time, found bool) {
	conditions, _, _ := unstructured.NestedSlice(node.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] != "Ready" {
			continue
		}
		found = true
		if condition["status"] == "True" {
			return true, time.Time{}, true
		}
		since = timestampOf(condition["lastHeartbeatTime"])
		if since.IsZero() {
			since = timestampOf(condition["lastTransitionTime"])
		}
		return false, since, true
	}
	return false, time.Time{}, false
}

// timestampOf parses one RFC3339 node condition timestamp.
func timestampOf(value any) time.Time {
	text, _ := value.(string)
	if text == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// bindingNodeOf resolves the node a node-local volume is pinned to from the
// persistent volume's node affinity (C-38).
func bindingNodeOf(pv *unstructured.Unstructured) string {
	terms, _, _ := unstructured.NestedSlice(pv.Object, "spec", "nodeAffinity", "required", "nodeSelectorTerms")
	for _, raw := range terms {
		term, _ := raw.(map[string]any)
		expressions, _ := term["matchExpressions"].([]any)
		for _, item := range expressions {
			expression, _ := item.(map[string]any)
			if expression["key"] != hostnameLabel || expression["operator"] != "In" {
				continue
			}
			values, _ := expression["values"].([]any)
			if len(values) == 0 {
				continue
			}
			if name, ok := values[0].(string); ok && name != "" {
				return name
			}
		}
	}
	return ""
}

// backendOf resolves the storage provider a bound volume landed on: the CSI
// driver of the persistent volume wins over the class provisioner (C-38).
func backendOf(pv *unstructured.Unstructured, class *storageClass) v1beta1.VolumeBackend {
	if driver, found, _ := unstructured.NestedString(pv.Object, "spec", "csi", "driver"); found && driver != "" {
		return v1beta1.VolumeBackend{Name: driver}
	}
	if class != nil && class.Provisioner != "" {
		return v1beta1.VolumeBackend{Name: class.Provisioner}
	}
	return v1beta1.VolumeBackend{}
}

// reclaimPolicyOf reads the policy the resolved volume carries; KOS never
// rewrites it (C-39).
func reclaimPolicyOf(pv *unstructured.Unstructured, class *storageClass) string {
	if policy, found, _ := unstructured.NestedString(pv.Object, "spec", "persistentVolumeReclaimPolicy"); found && policy != "" {
		return policy
	}
	if class != nil {
		return class.ReclaimPolicy
	}
	return ""
}

// capabilitiesOf is the capability set the backend resolved: the volume
// baseline always, plus shared-rwx for a shared volume (C-09, C-38).
func capabilitiesOf(volume *v1beta1.Volume, sharedVersion string) []v1beta1.VolumeCapability {
	capabilities := []v1beta1.VolumeCapability{{Name: CapabilityVolume, Version: CapabilityVersionV1}}
	if volume.Spec.Mobility == v1beta1.VolumeMobilitySharedRWX {
		capabilities = append(capabilities, v1beta1.VolumeCapability{Name: CapabilitySharedRWX, Version: sharedVersion})
	}
	return capabilities
}
