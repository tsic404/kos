package volume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
)

// Phases of a volume (C-38).
const (
	PhasePending = "Pending"
	PhaseBound   = "Bound"
	PhaseLost    = "Lost"
)

// Condition types and reasons the controller writes (C-07, C-38, C-39). A
// failure condition keeps its own type and never stands in for Reconciled.
const (
	ConditionInvalidSpec       = "InvalidSpec"
	ConditionCapabilityMissing = "CapabilityMissing"
	ConditionStorageMissing    = "StorageMissing"
	ConditionClaimBlocked      = "ClaimBlocked"
	ConditionLost              = "Lost"
	ConditionImported          = "Imported"
)

// Capability names a volume backend resolves (C-09, C-38). The baseline names
// start at v1.
const (
	CapabilityVolume         = "storage.volume"
	CapabilitySharedRWX      = "storage.shared-rwx"
	CapabilityVersionV1      = "v1"
	SnapshotConsistencyCrash = "crash-consistent"
)

// Finalizer keeps a deleting volume observable until its release marker is
// written or its purge has removed the claim (C-39).
const Finalizer = "kos.tsic.top/volume"

// ReleasedValue is what the release marker carries on an unbound claim (C-02,
// C-39). The marker's presence is the contract; the value is not.
const ReleasedValue = "true"

// DefaultNodeGrace is the kubelet heartbeat grace period the confirmation
// window adds five minutes to (C-38). It matches the kernel's node monitor
// default; a cluster with another value passes it in Options.
const DefaultNodeGrace = 40 * time.Second

// ConfirmationWindow is how long a node-local binding may be unobservable
// before the volume is Lost (C-38).
const ConfirmationWindow = 5 * time.Minute

// Resync intervals. The kernel has no watch source yet, so a settled object is
// re-observed by its own bounded requeue (C-06).
const (
	claimResync   = 10 * time.Second
	boundResync   = 30 * time.Second
	pendingResync = 30 * time.Second
)

// CapabilitySource reports the capabilities providers registered (C-09). The
// version is what the volume status records; ok is false when no provider
// registers the name.
type CapabilitySource interface {
	Version(ctx context.Context, name string) (version string, ok bool, err error)
}

// Registered is a fixed registration set of capabilities.
type Registered []v1beta1.VolumeCapability

// Version implements CapabilitySource.
func (r Registered) Version(_ context.Context, name string) (string, bool, error) {
	for _, capability := range r {
		if capability.Name == name {
			return capability.Version, true, nil
		}
	}
	return "", false, nil
}

// Options configures a Controller.
type Options struct {
	// Capabilities supplies the registered capabilities; nil registers none,
	// so a shared-rwx volume stays Pending with the missing capability named.
	Capabilities CapabilitySource
	// Clock supplies the round time; defaults to time.Now.
	Clock func() time.Time
	// NodeGrace is the heartbeat grace period the confirmation window adds
	// five minutes to; zero selects DefaultNodeGrace.
	NodeGrace time.Duration
}

// Controller reconciles volumes: it creates and expands the one claim a volume
// owns, follows the claim's binding and keeps the data-deletion boundary.
type Controller struct {
	store        Store
	capabilities CapabilitySource
	clock        func() time.Time
	nodeGrace    time.Duration
}

// NewController builds a controller over one store.
func NewController(store Store, options Options) *Controller {
	controller := &Controller{store: store, capabilities: options.Capabilities, clock: options.Clock, nodeGrace: options.NodeGrace}
	if controller.clock == nil {
		controller.clock = time.Now
	}
	if controller.nodeGrace <= 0 {
		controller.nodeGrace = DefaultNodeGrace
	}
	return controller
}

// Reconcile runs one round of one volume (C-06).
func (c *Controller) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	if request.Kind != "Volume" {
		return reconcile.Result{}, nil
	}
	ref := write.Ref{Group: api.Group, Version: "v1beta1", Kind: "Volume", Namespace: request.Namespace, Name: request.Name}
	stored, err := c.store.Get(ctx, ref)
	if apierrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	volume := &v1beta1.Volume{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, volume); err != nil {
		return reconcile.Result{}, fmt.Errorf("decode the volume: %w", err)
	}
	if volume.DeletionTimestamp != nil {
		return c.release(ctx, volume)
	}
	if !hasFinalizer(volume) {
		finalizers := append(append([]string{}, volume.Finalizers...), Finalizer)
		if _, err := c.store.SetFinalizers(ctx, ref, finalizers); err != nil {
			return reconcile.Result{}, err
		}
	}
	return c.converge(ctx, volume)
}

// converge creates or expands the claim, imports a released one and reports
// the observed binding state (C-38, C-39).
func (c *Controller) converge(ctx context.Context, volume *v1beta1.Volume) (reconcile.Result, error) {
	capacity, problem := validateSpec(volume.Spec)
	if problem != "" {
		return c.block(ctx, volume, ConditionInvalidSpec, problem)
	}
	sharedVersion := ""
	if volume.Spec.Mobility == v1beta1.VolumeMobilitySharedRWX {
		version, registered, err := c.sharedCapability(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}
		if !registered {
			return c.block(ctx, volume, ConditionCapabilityMissing,
				"shared-rwx needs the registered "+CapabilitySharedRWX+" capability")
		}
		sharedVersion = version
	}
	class, err := c.storageClass(ctx, volume.Spec.StorageClass)
	if err != nil {
		return reconcile.Result{}, err
	}
	if volume.Spec.StorageClass != "" && class == nil {
		return c.block(ctx, volume, ConditionStorageMissing,
			fmt.Sprintf("storage class %q does not exist", volume.Spec.StorageClass))
	}
	claimName := volume.Name
	if volume.Status.BoundClaim != "" {
		claimName = volume.Status.BoundClaim
	}
	claim, imported, err := c.ensureClaim(ctx, volume, claimName, capacity)
	if err != nil {
		var lost *lostClaimError
		if errors.As(err, &lost) {
			// The claim a bound volume owned is gone: recreating it would
			// produce an empty claim under the same name and hide the data
			// loss (C-38).
			status := lostStatus(volume, volume.Status.Node, lost.message)
			return reconcile.Result{RequeueAfter: boundResync}, c.writeStatus(ctx, volume, status)
		}
		if permanent(err) {
			return c.blockClaim(ctx, volume, fmt.Sprintf("claim %q cannot be maintained: %v", claimName, err))
		}
		return reconcile.Result{}, err
	}
	status, observed, err := c.observe(ctx, volume, claim, class, sharedVersion)
	if err != nil {
		return reconcile.Result{}, err
	}
	if imported {
		status.Conditions = state.Set(status.Conditions, state.Condition(
			ConditionImported, metav1.ConditionTrue, volume.Generation, ConditionImported,
			fmt.Sprintf("claim %q carried the release marker and was imported", claim.GetName())))
	}
	return observed, c.writeStatus(ctx, volume, status)
}

// ensureClaim brings the volume's one claim into existence and keeps its size
// at the requested capacity. imported reports a claim that carried the release
// marker and was taken back (C-39). A claim a bound volume owned is never
// recreated: its absence or unreadability makes the volume Lost (C-38).
func (c *Controller) ensureClaim(ctx context.Context, volume *v1beta1.Volume, name string, capacity resource.Quantity) (*unstructured.Unstructured, bool, error) {
	bound := volume.Status.Phase == PhaseBound || volume.Status.Phase == PhaseLost
	claim, err := c.store.GetResource(ctx, pvcResource, volume.Namespace, name)
	switch {
	case apierrors.IsNotFound(err):
		if bound {
			return nil, false, &lostClaimError{message: fmt.Sprintf("claim %q is missing", name)}
		}
		created, err := c.store.CreateClaim(ctx, desiredClaim(volume, name, capacity))
		if err != nil {
			return nil, false, fmt.Errorf("create claim %q: %w", name, err)
		}
		return created, false, nil
	case err != nil:
		if bound {
			return nil, false, &lostClaimError{message: fmt.Sprintf("claim %q cannot be read: %v", name, err)}
		}
		return nil, false, err
	}

	imported := false
	if released(claim) {
		updated, err := c.store.MarkOn(ctx, claim, nil, []string{markers.Released})
		if err != nil {
			return nil, false, err
		}
		claim = updated
		imported = true
	}
	expanded, changed, err := c.expand(ctx, volume, claim, capacity)
	if err != nil {
		return nil, false, err
	}
	if changed {
		claim = expanded
	}
	return claim, imported, nil
}

// expand raises the claim's requested size to the volume's capacity; a smaller
// capacity never shrinks a claim, because that change is irreversible (C-34,
// C-38).
func (c *Controller) expand(ctx context.Context, volume *v1beta1.Volume, claim *unstructured.Unstructured, capacity resource.Quantity) (*unstructured.Unstructured, bool, error) {
	requested, found, err := unstructured.NestedString(claim.Object, "spec", "resources", "requests", "storage")
	if err != nil {
		return claim, false, fmt.Errorf("read the claim size: %w", err)
	}
	if !found {
		return claim, false, nil
	}
	current, err := resource.ParseQuantity(requested)
	if err != nil {
		return claim, false, fmt.Errorf("the claim size %q is not a quantity: %w", requested, err)
	}
	if current.Cmp(capacity) >= 0 {
		return claim, false, nil
	}
	// Apply only the size the volume owns: a whole-object apply would claim
	// fields other writers set, such as the binding the controller records
	// (C-04, C-38).
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      claim.GetName(),
			"namespace": claim.GetNamespace(),
		},
		"spec": map[string]any{
			"resources": map[string]any{
				"requests": map[string]any{"storage": capacity.String()},
			},
		},
	}}
	stored, err := c.store.ApplyClaim(ctx, document)
	return stored, true, err
}

// release runs the deletion path of C-39: the claim survives unless the purge
// token names this volume; a retained claim carries the release marker. The
// decision reads the object field, so the kubectl and panel paths agree. A
// claim KOS does not own is neither marked nor deleted (C-02, C-04).
func (c *Controller) release(ctx context.Context, volume *v1beta1.Volume) (reconcile.Result, error) {
	claimName := volume.Name
	if volume.Status.BoundClaim != "" {
		claimName = volume.Status.BoundClaim
	}
	ref := claimRef(volume.Namespace, claimName)
	claim, err := c.store.GetResource(ctx, pvcResource, volume.Namespace, claimName)
	if apierrors.IsNotFound(err) {
		return reconcile.Result{}, c.clearFinalizer(ctx, volume)
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	if !claimOwned(claim) {
		return c.releaseForeignClaim(ctx, volume, claimName)
	}
	if volume.Spec.PurgeToken == volume.Name {
		if err := c.store.DeleteClaim(ctx, ref); err != nil && !apierrors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
		if _, err := c.store.GetResource(ctx, pvcResource, volume.Namespace, claimName); err != nil {
			if apierrors.IsNotFound(err) {
				return reconcile.Result{}, c.clearFinalizer(ctx, volume)
			}
			return reconcile.Result{}, err
		}
		// The claim has a finalizer of its own; keep the gate until it is gone.
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
	if !released(claim) {
		if _, err := c.store.MarkOn(ctx, claim, map[string]string{markers.Released: ReleasedValue}, nil); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, c.clearFinalizer(ctx, volume)
}

// releaseForeignClaim ends the deletion of a volume whose claim belongs to
// another writer: the claim stays in place untouched, and the volume records
// why it was not marked before its gate is released (C-02, C-39).
func (c *Controller) releaseForeignClaim(ctx context.Context, volume *v1beta1.Volume, claimName string) (reconcile.Result, error) {
	status := baseStatus(volume)
	status.Phase = volume.Status.Phase
	if status.Phase == "" {
		status.Phase = PhasePending
	}
	status.Health = volume.Status.Health
	if status.Health.State == "" {
		status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
	}
	message := fmt.Sprintf("claim %q is not managed by KOS; it is left in place without a release marker", claimName)
	status.Conditions = withFailure(status.Conditions, volume.Generation, ConditionClaimBlocked, message)
	if err := c.writeStatus(ctx, volume, status); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, c.clearFinalizer(ctx, volume)
}

// claimOwned reports whether the claim is an object KOS manages or adopted; a
// claim without an attribution marker is not KOS's to mark or delete (C-02).
func claimOwned(claim *unstructured.Unstructured) bool {
	attribution, err := markers.AttributionOf(claim.GetLabels())
	return err == nil && attribution != markers.AttributionUnmanaged
}

// released reports whether the claim carries the release marker: the marker's
// presence is the contract, its value is not (C-39).
func released(claim *unstructured.Unstructured) bool {
	_, marked := claim.GetLabels()[markers.Released]
	return marked
}

// lostClaimError reports a bound volume's claim that is gone or unreadable: the
// volume is Lost and the claim is never recreated (C-38).
type lostClaimError struct {
	message string
}

// Error implements error.
func (e *lostClaimError) Error() string { return e.message }

// block writes a Pending state with the reason the spec or a prerequisite is
// not usable (C-07, C-38).
func (c *Controller) block(ctx context.Context, volume *v1beta1.Volume, conditionType, message string) (reconcile.Result, error) {
	status := baseStatus(volume)
	status.Phase = PhasePending
	status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
	status.Conditions = state.Set(status.Conditions, state.Blocked(volume.Generation, conditionType, message))
	status.Conditions = withFailure(status.Conditions, volume.Generation, conditionType, message)
	return reconcile.Result{RequeueAfter: pendingResync}, c.writeStatus(ctx, volume, status)
}

// blockClaim reports a claim write the same spec cannot make succeed, keeping
// the phase the volume already reached: a failed expansion does not unbind a
// bound volume (C-38).
func (c *Controller) blockClaim(ctx context.Context, volume *v1beta1.Volume, message string) (reconcile.Result, error) {
	status := baseStatus(volume)
	status.Phase = volume.Status.Phase
	if status.Phase == "" {
		status.Phase = PhasePending
	}
	status.Health = volume.Status.Health
	if status.Health.State == "" {
		status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
	}
	status.Conditions = state.Set(status.Conditions, state.Blocked(volume.Generation, ConditionClaimBlocked, message))
	status.Conditions = withFailure(status.Conditions, volume.Generation, ConditionClaimBlocked, message)
	return reconcile.Result{RequeueAfter: pendingResync}, c.writeStatus(ctx, volume, status)
}

// clearFinalizer releases the deletion gate so the API server can remove the
// object (C-39).
func (c *Controller) clearFinalizer(ctx context.Context, volume *v1beta1.Volume) error {
	ref := write.Ref{Group: api.Group, Version: "v1beta1", Kind: "Volume", Namespace: volume.Namespace, Name: volume.Name}
	remaining := make([]string, 0, len(volume.Finalizers))
	for _, finalizer := range volume.Finalizers {
		if finalizer != Finalizer {
			remaining = append(remaining, finalizer)
		}
	}
	_, err := c.store.SetFinalizers(ctx, ref, remaining)
	return err
}

// writeStatus writes the status only when it changed, so a repeat observation
// produces no write (C-06). The binding claim and the policy it resolved are
// written by the migration flow (ADR-015, C-40): this round reads them and
// leaves them out of its document, so the two writers merge by field (C-05).
func (c *Controller) writeStatus(ctx context.Context, volume *v1beta1.Volume, status v1beta1.VolumeStatus) error {
	if sameStatus(volume.Status, status) {
		return nil
	}
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return fmt.Errorf("encode the volume status: %w", err)
	}
	delete(encoded, "boundClaim")
	delete(encoded, "storageClass")
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Volume",
		"metadata": map[string]any{
			"name":      volume.Name,
			"namespace": volume.Namespace,
		},
		"status": encoded,
	}}
	_, err = c.store.ApplyStatus(ctx, document)
	return err
}

// baseStatus carries the conditions a round starts from; every observed fact
// is filled in by the round itself. The binding claim and its policy are read
// back so the round judges the claim the migration bound the volume to and does
// not report a change it never wrote (C-40).
func baseStatus(volume *v1beta1.Volume) v1beta1.VolumeStatus {
	return v1beta1.VolumeStatus{
		ObjectStatus: v1beta1.ObjectStatus{
			Conditions: append([]metav1.Condition(nil), volume.Status.Conditions...),
			Health:     v1beta1.Health{State: v1beta1.HealthStateUnavailable},
		},
		BoundClaim:   volume.Status.BoundClaim,
		StorageClass: volume.Status.StorageClass,
	}
}

// failureConditions are the condition types the controller owns, so a failure
// that no longer applies is removed instead of staying True forever.
var failureConditions = []string{
	ConditionInvalidSpec,
	ConditionCapabilityMissing,
	ConditionStorageMissing,
	ConditionClaimBlocked,
	ConditionLost,
}

// withFailure sets one failure condition and removes the ones that no longer
// apply (C-07).
func withFailure(conditions []metav1.Condition, generation int64, conditionType, message string) []metav1.Condition {
	for _, other := range failureConditions {
		if other != conditionType {
			meta.RemoveStatusCondition(&conditions, other)
		}
	}
	return state.Set(conditions, state.Failure(conditionType, generation, conditionType, message))
}

// clearFailures removes every failure condition the controller owns, so a
// round that observes a healthy state stops reporting an earlier failure
// (C-07).
func clearFailures(conditions []metav1.Condition) []metav1.Condition {
	for _, conditionType := range failureConditions {
		meta.RemoveStatusCondition(&conditions, conditionType)
	}
	return conditions
}

// sameStatus compares two statuses by their encoding.
func sameStatus(left, right v1beta1.VolumeStatus) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

// permanent reports a write the same spec cannot make succeed by retrying.
func permanent(err error) bool {
	var denied *write.Denied
	if errors.As(err, &denied) {
		return true
	}
	var unknown *write.UnknownResource
	if errors.As(err, &unknown) {
		return true
	}
	return apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsForbidden(err) || apierrors.IsNotFound(err)
}

// validateSpec applies C-38's required shape: mobility without a default and a
// readable capacity. It returns the capacity and the reason a spec is invalid.
func validateSpec(spec v1beta1.VolumeSpec) (resource.Quantity, string) {
	switch spec.Mobility {
	case v1beta1.VolumeMobilityNodeLocal, v1beta1.VolumeMobilityClusterMovable, v1beta1.VolumeMobilitySharedRWX:
	case "":
		return resource.Quantity{}, "mobility is required: node-local, cluster-movable or shared-rwx"
	default:
		return resource.Quantity{}, fmt.Sprintf("mobility %q is not one of node-local, cluster-movable or shared-rwx", spec.Mobility)
	}
	if spec.Capacity == "" {
		return resource.Quantity{}, "capacity is required"
	}
	capacity, err := resource.ParseQuantity(spec.Capacity)
	if err != nil {
		return resource.Quantity{}, fmt.Sprintf("capacity %q is not a quantity: %v", spec.Capacity, err)
	}
	if capacity.Sign() <= 0 {
		return resource.Quantity{}, fmt.Sprintf("capacity %q must be positive", spec.Capacity)
	}
	return capacity, ""
}

// hasFinalizer reports whether the volume carries the deletion gate.
func hasFinalizer(volume *v1beta1.Volume) bool {
	for _, finalizer := range volume.Finalizers {
		if finalizer == Finalizer {
			return true
		}
	}
	return false
}

// claimRef is the fixed claim name of a volume (C-38).
func claimRef(namespace, name string) write.Ref {
	return write.Ref{Version: "v1", Kind: "PersistentVolumeClaim", Namespace: namespace, Name: name}
}

// desiredClaim is the claim the volume object creates (C-32, C-38). The name is
// the volume name, the policy is the volume's, and the attribution names the
// volume as the owner.
func desiredClaim(volume *v1beta1.Volume, name string, capacity resource.Quantity) *unstructured.Unstructured {
	accessMode := "ReadWriteOnce"
	if volume.Spec.Mobility == v1beta1.VolumeMobilitySharedRWX {
		accessMode = "ReadWriteMany"
	}
	spec := map[string]any{
		"accessModes": []any{accessMode},
		"resources":   map[string]any{"requests": map[string]any{"storage": capacity.String()}},
	}
	if volume.Spec.StorageClass != "" {
		spec["storageClassName"] = volume.Spec.StorageClass
	}
	labels := map[string]any{markers.ManagedBy: write.ManagedByValue}
	if volume.UID != "" {
		labels[markers.OwnerUID] = string(volume.UID)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      name,
			"namespace": volume.Namespace,
			"labels":    labels,
		},
		"spec": spec,
	}}
}
