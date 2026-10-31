// Package migration implements the C-40 volume move as a six-phase state
// machine: Preflight, Quiesce, Copy, Verify, Cutover and Cleanup. The four
// phases after Preflight each write a rollback point on entry and roll that
// point back when they fail; a Cleanup failure retries in place because the
// volume has already been cut over by then.
//
// The flow writes only what C-04 and ADR-015 allocate to it: the migration
// target claim, its own step Jobs, the two binding fields of the volume status
// and the release marker of the source claim. It never writes a volume spec,
// never deletes the source claim (F6) and never deletes a claim a volume is
// bound to.
package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
)

// Phases of a migration (C-40). The first six are the flow stages; Succeeded
// and Failed are the two terminal states.
const (
	PhasePreflight = "Preflight"
	PhaseQuiesce   = "Quiesce"
	PhaseCopy      = "Copy"
	PhaseVerify    = "Verify"
	PhaseCutover   = "Cutover"
	PhaseCleanup   = "Cleanup"
	PhaseSucceeded = "Succeeded"
	PhaseFailed    = "Failed"
)

// rollbackPhases are the phases whose entry writes the attempt's rollback
// point: a failure there rolls back what the phase did (C-40). Preflight has
// changed nothing, and a Cleanup failure retries in place.
var rollbackPhases = map[string]bool{
	PhaseQuiesce: true,
	PhaseCopy:    true,
	PhaseVerify:  true,
	PhaseCutover: true,
}

// Condition types the controller writes (C-07). InvalidSpec reports a request
// the flow cannot attempt; Failed reports an attempt that rolled back.
const (
	ConditionInvalidSpec = "InvalidSpec"
	ConditionFailed      = "Failed"
)

// Finalizer keeps a deleting migration observable until the objects it created
// are gone, so a delete mid-flight leaves no target claim behind (C-40).
const Finalizer = "kos.tsic.top/volumemigration"

// Steps a step Job runs; the flow passes the step and the two claims as the
// Job's arguments (C-40).
const (
	StepCopy   = "copy"
	StepVerify = "verify"
)

// ReleasedValue is what the release marker carries on the source claim; the
// marker's presence is the contract, not its value (C-02, C-39).
const ReleasedValue = "true"

// EventSourceReleased is the reason of the event Cleanup records when it marks
// the source claim released (C-40, C-64).
const EventSourceReleased = "SourceClaimReleased"

// phaseResync re-observes a migration that waits for an external fact (C-06).
// The kernel has no watch source yet, so a settled object requeues itself.
const phaseResync = 5 * time.Second

// Native resources the flow reads or writes directly.
var (
	pvcResource          = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	jobResource          = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	storageClassResource = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	applicationResource  = schema.GroupVersionResource{Group: api.Group, Version: "v1beta1", Resource: "applications"}
)

// scalableKinds are the rendered kinds a stop drives to zero (C-66): the same
// set the application reconciler converges, so the flow waits for exactly the
// workloads an application stops.
var scalableKinds = map[string]bool{
	"Deployment":  true,
	"StatefulSet": true,
	"ReplicaSet":  true,
}

// Options configures a Controller.
type Options struct {
	// RunnerImage names the image that runs one migration step. The flow
	// writes the step and the two claims as the Job's arguments; without an
	// image the request fails at Preflight instead of creating a Job that can
	// never run (C-40).
	RunnerImage string
	// Clock supplies the round time; defaults to time.Now.
	Clock func() time.Time
}

// Controller reconciles volume migrations: it drives the six phases, keeps the
// target claim the flow owns, and rolls an attempt back to the source claim
// when a phase fails (C-40).
type Controller struct {
	store Store
	image string
	clock func() time.Time
}

// NewController builds a controller over one store.
func NewController(store Store, options Options) *Controller {
	controller := &Controller{store: store, image: options.RunnerImage, clock: options.Clock}
	if controller.clock == nil {
		controller.clock = time.Now
	}
	return controller
}

// Reconcile runs one round of one migration (C-06).
func (c *Controller) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	if request.Kind != "VolumeMigration" {
		return reconcile.Result{}, nil
	}
	ref := write.Ref{Group: api.Group, Version: "v1beta1", Kind: "VolumeMigration", Namespace: request.Namespace, Name: request.Name}
	stored, err := c.store.Get(ctx, ref)
	if apierrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	migration := &v1beta1.VolumeMigration{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, migration); err != nil {
		return reconcile.Result{}, fmt.Errorf("decode the migration: %w", err)
	}
	if migration.DeletionTimestamp != nil {
		return c.remove(ctx, migration)
	}
	if !hasFinalizer(migration) {
		finalizers := append(append([]string{}, migration.Finalizers...), Finalizer)
		if _, err := c.store.SetFinalizers(ctx, ref, finalizers); err != nil {
			return reconcile.Result{}, err
		}
	}
	return c.converge(ctx, migration)
}

// converge advances the flow by one phase, or reports why the phase cannot
// finish yet (C-40).
func (c *Controller) converge(ctx context.Context, migration *v1beta1.VolumeMigration) (reconcile.Result, error) {
	switch migration.Status.Phase {
	case "", PhasePreflight:
		return c.preflight(ctx, migration)
	case PhaseQuiesce:
		return c.quiesce(ctx, migration)
	case PhaseCopy:
		return c.copy(ctx, migration)
	case PhaseVerify:
		return c.verify(ctx, migration)
	case PhaseCutover:
		return c.cutover(ctx, migration)
	case PhaseCleanup:
		return c.cleanup(ctx, migration)
	case PhaseSucceeded, PhaseFailed:
		return reconcile.Result{}, nil
	default:
		return c.invalid(ctx, migration,
			fmt.Sprintf("phase %q is not one of the six stages", migration.Status.Phase))
	}
}

// attempt is what one round resolved about the move the request asks for. Every
// round reads it again from current facts, so a phase never depends on what a
// previous round recorded (C-06).
type attempt struct {
	sourceClaim string
	sourceClass string
	targetClaim string
	capacity    string
	accessModes []any
	duration    time.Duration
	start       time.Time
}

// preflight refuses a request the flow cannot carry through, waits for the
// window to open, and otherwise records the binding the attempt returns to
// (C-40).
func (c *Controller) preflight(ctx context.Context, migration *v1beta1.VolumeMigration) (reconcile.Result, error) {
	now := c.clock()
	next, problem, err := c.resolve(ctx, migration)
	if err != nil {
		return reconcile.Result{}, err
	}
	if problem != "" {
		return c.invalid(ctx, migration, problem)
	}
	if now.Before(next.start) {
		return c.wait(ctx, migration, PhasePreflight,
			fmt.Sprintf("the window opens at %s", next.start.UTC().Format(time.RFC3339)))
	}
	if !now.Before(next.start.Add(next.duration)) {
		return c.invalid(ctx, migration,
			"the window closed before the migration could quiesce; nothing was changed")
	}
	// The binding is the fact a rollback returns to, so the attempt records it
	// before it touches anything else (C-40).
	if err := c.writeBinding(ctx, migration, next.sourceClaim, next.sourceClass); err != nil {
		return reconcile.Result{}, err
	}
	status := baseStatus(migration)
	status.Phase = PhaseQuiesce
	status.SourceClaim = next.sourceClaim
	status.TargetClaim = next.targetClaim
	status.RollbackPoint = PhaseQuiesce
	status.WindowStartedAt = &metav1.Time{Time: now}
	status.Health = v1beta1.Health{State: v1beta1.HealthStateHealthy}
	status.Conditions = state.Set(status.Conditions, state.Blocked(migration.Generation, PhaseQuiesce,
		"waiting for the workloads that use the volume to exit"))
	return reconcile.Result{RequeueAfter: phaseResync}, c.writeStatus(ctx, migration, status)
}

// quiesce waits for the workloads of every application that declares the
// volume to exit. The application reconciler stops them; the flow only waits
// and never writes an application spec (C-40).
func (c *Controller) quiesce(ctx context.Context, migration *v1beta1.VolumeMigration) (reconcile.Result, error) {
	if expired(migration, c.clock()) {
		return c.fail(ctx, migration, PhaseQuiesce, "the downtime window expired before the workloads exited")
	}
	stopped, err := c.workloadsStopped(ctx, migration)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !stopped {
		return c.wait(ctx, migration, PhaseQuiesce, "waiting for the workloads that use the volume to exit")
	}
	return c.enter(ctx, migration, PhaseCopy, "the workloads exited; the target claim and the copy Job are created")
}

// copy creates the migration target and the copy Job, then waits for the Job
// (C-40).
func (c *Controller) copy(ctx context.Context, migration *v1beta1.VolumeMigration) (reconcile.Result, error) {
	if expired(migration, c.clock()) {
		return c.fail(ctx, migration, PhaseCopy, "the downtime window expired during the copy")
	}
	next, problem, err := c.resolve(ctx, migration)
	if err != nil {
		return reconcile.Result{}, err
	}
	if problem != "" {
		return c.fail(ctx, migration, PhaseCopy, problem)
	}
	problem, err = c.ensureTarget(ctx, migration, next)
	if err != nil {
		return reconcile.Result{}, err
	}
	if problem != "" {
		return c.fail(ctx, migration, PhaseCopy, problem)
	}
	job, problem, err := c.ensureJob(ctx, migration, StepCopy, next)
	if err != nil {
		return reconcile.Result{}, err
	}
	if problem != "" {
		return c.fail(ctx, migration, PhaseCopy, problem)
	}
	switch outcome, message := jobState(job); outcome {
	case jobFailed:
		return c.fail(ctx, migration, PhaseCopy, "the copy Job failed: "+message)
	case jobComplete:
		return c.enter(ctx, migration, PhaseVerify, "the copy Job completed; the target is verified")
	default:
		return c.wait(ctx, migration, PhaseCopy, "the copy Job is running")
	}
}

// verify waits for the Job that compares the target with the source, so an
// extra or missing file fails the attempt instead of reaching the cutover
// (C-40).
func (c *Controller) verify(ctx context.Context, migration *v1beta1.VolumeMigration) (reconcile.Result, error) {
	if expired(migration, c.clock()) {
		return c.fail(ctx, migration, PhaseVerify, "the downtime window expired during verification")
	}
	next, problem, err := c.resolve(ctx, migration)
	if err != nil {
		return reconcile.Result{}, err
	}
	if problem != "" {
		return c.fail(ctx, migration, PhaseVerify, problem)
	}
	job, problem, err := c.ensureJob(ctx, migration, StepVerify, next)
	if err != nil {
		return reconcile.Result{}, err
	}
	if problem != "" {
		return c.fail(ctx, migration, PhaseVerify, problem)
	}
	switch outcome, message := jobState(job); outcome {
	case jobFailed:
		return c.fail(ctx, migration, PhaseVerify, "the verification Job failed: "+message)
	case jobComplete:
		return c.enter(ctx, migration, PhaseCutover, "the target matches the source; the binding is cut over")
	default:
		return c.wait(ctx, migration, PhaseVerify, "the verification Job is running")
	}
}

// cutover waits for the target claim to bind and then writes the volume's
// binding and actual policy; the application re-renders from the new claim and
// its own object does not change (C-40).
func (c *Controller) cutover(ctx context.Context, migration *v1beta1.VolumeMigration) (reconcile.Result, error) {
	if expired(migration, c.clock()) {
		return c.fail(ctx, migration, PhaseCutover, "the downtime window expired before the target claim was bound")
	}
	claim, err := c.store.GetResource(ctx, pvcResource, migration.Namespace, migration.Status.TargetClaim)
	if apierrors.IsNotFound(err) {
		return c.fail(ctx, migration, PhaseCutover, "the target claim is missing")
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	phase, _, _ := unstructured.NestedString(claim.Object, "status", "phase")
	boundVolume, _, _ := unstructured.NestedString(claim.Object, "spec", "volumeName")
	if phase != "Bound" || boundVolume == "" {
		return c.wait(ctx, migration, PhaseCutover, "the target claim is not bound yet")
	}
	class, _, _ := unstructured.NestedString(claim.Object, "spec", "storageClassName")
	if err := c.writeBinding(ctx, migration, claim.GetName(), class); err != nil {
		return reconcile.Result{}, err
	}
	return c.enter(ctx, migration, PhaseCleanup, "the binding points at the target; the source is released")
}

// cleanup marks the source claim released and deletes the step Jobs. It never
// deletes the source claim and never rolls back: a failed Cleanup retries in
// place (C-40, F6).
func (c *Controller) cleanup(ctx context.Context, migration *v1beta1.VolumeMigration) (reconcile.Result, error) {
	source, err := c.store.GetResource(ctx, pvcResource, migration.Namespace, migration.Status.SourceClaim)
	if apierrors.IsNotFound(err) {
		return c.wait(ctx, migration, PhaseCleanup,
			fmt.Sprintf("the source claim %q is missing; it cannot be marked released", migration.Status.SourceClaim))
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	if _, marked := source.GetLabels()[markers.Released]; !marked {
		if _, err := c.store.MarkOn(ctx, source, map[string]string{markers.Released: ReleasedValue}, nil); err != nil {
			return reconcile.Result{}, err
		}
		message := fmt.Sprintf("the source claim is released; the volume is bound to %q", migration.Status.TargetClaim)
		if err := c.store.RecordEvent(ctx, source, "Normal", EventSourceReleased, message, c.clock()); err != nil {
			return reconcile.Result{}, err
		}
	}
	if err := c.deleteJobs(ctx, migration); err != nil {
		return reconcile.Result{}, err
	}
	status := baseStatus(migration)
	status.Phase = PhaseSucceeded
	status.RollbackPoint = ""
	status.Health = v1beta1.Health{State: v1beta1.HealthStateHealthy}
	status.Conditions = state.Set(status.Conditions, state.Applied(migration.Generation,
		fmt.Sprintf("the volume is bound to %q and the source claim %q is released", migration.Status.TargetClaim, migration.Status.SourceClaim)))
	status.Conditions = clearFailures(status.Conditions)
	return reconcile.Result{}, c.writeStatus(ctx, migration, status)
}

// resolve reads what the move needs from current facts: the volume, the claim
// it is bound to, the policy that claim resolved, the target claim name and the
// window. A non-empty problem is a request the flow cannot attempt.
func (c *Controller) resolve(ctx context.Context, migration *v1beta1.VolumeMigration) (*attempt, string, error) {
	spec := migration.Spec
	if spec.Volume == "" {
		return nil, "volume is required", nil
	}
	if spec.TargetStorageClass == "" {
		return nil, "targetStorageClass is required", nil
	}
	duration, err := time.ParseDuration(spec.Window.Duration)
	if err != nil || duration <= 0 {
		return nil, fmt.Sprintf("window duration %q is not a positive duration", spec.Window.Duration), nil
	}
	if c.image == "" {
		return nil, "no migration runner image is configured, so no step can run", nil
	}
	volume := &v1beta1.Volume{}
	stored, err := c.store.Get(ctx, write.Ref{Group: api.Group, Version: "v1beta1", Kind: "Volume", Namespace: migration.Namespace, Name: spec.Volume})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Sprintf("volume %q does not exist", spec.Volume), nil
	}
	if err != nil {
		return nil, "", err
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, volume); err != nil {
		return nil, fmt.Sprintf("volume %q cannot be decoded: %v", spec.Volume, err), nil
	}
	if volume.Spec.StorageClass != spec.TargetStorageClass {
		return nil, fmt.Sprintf("the target policy %q is not the policy the volume declares (%q)",
			spec.TargetStorageClass, volume.Spec.StorageClass), nil
	}
	if volume.Status.Phase != "Bound" {
		return nil, fmt.Sprintf("volume %q is not bound", spec.Volume), nil
	}
	sourceClaim := volume.Status.BoundClaim
	if sourceClaim == "" {
		sourceClaim = volume.Name
	}
	claim, err := c.store.GetResource(ctx, pvcResource, migration.Namespace, sourceClaim)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Sprintf("the claim %q the volume is bound to is missing", sourceClaim), nil
	}
	if err != nil {
		return nil, "", err
	}
	sourceClass, _, _ := unstructured.NestedString(claim.Object, "spec", "storageClassName")
	if sourceClass == spec.TargetStorageClass {
		return nil, fmt.Sprintf("the volume is already bound with the target policy %q", sourceClass), nil
	}
	capacity, _, _ := unstructured.NestedString(claim.Object, "spec", "resources", "requests", "storage")
	if capacity == "" {
		return nil, fmt.Sprintf("the claim %q reports no size", sourceClaim), nil
	}
	if _, err := c.store.GetResource(ctx, storageClassResource, "", spec.TargetStorageClass); apierrors.IsNotFound(err) {
		return nil, fmt.Sprintf("storage class %q does not exist", spec.TargetStorageClass), nil
	} else if err != nil {
		return nil, "", err
	}
	accessModes, _, _ := unstructured.NestedSlice(claim.Object, "spec", "accessModes")
	if len(accessModes) == 0 {
		accessModes = []any{"ReadWriteOnce"}
	}
	start := c.clock()
	if spec.Window.Start != nil {
		start = spec.Window.Start.Time
	}
	return &attempt{
		sourceClaim: sourceClaim,
		sourceClass: sourceClass,
		targetClaim: targetClaimName(migration),
		capacity:    capacity,
		accessModes: accessModes,
		duration:    duration,
		start:       start,
	}, "", nil
}

// workloadsStopped reports whether every workload that mounts the volume has
// exited: a scalable workload counts as exited when it asks for zero replicas
// and reports none ready (C-40, C-66).
func (c *Controller) workloadsStopped(ctx context.Context, migration *v1beta1.VolumeMigration) (bool, error) {
	// The application object need not live in the namespace it declares (C-32),
	// so the applications are listed across namespaces and filtered by the
	// namespace the volume lives in.
	applications, err := c.store.List(ctx, applicationResource, "", "")
	if err != nil {
		return false, err
	}
	for index := range applications {
		application := &applications[index]
		namespace, _, err := unstructured.NestedString(application.Object, "spec", "namespace")
		if err != nil {
			return false, err
		}
		if namespace != migration.Namespace {
			continue
		}
		declares, _, err := unstructured.NestedStringSlice(application.Object, "spec", "volumes")
		if err != nil {
			return false, err
		}
		if !contains(declares, migration.Spec.Volume) {
			continue
		}
		rendered, _, err := unstructured.NestedSlice(application.Object, "status", "rendered")
		if err != nil {
			return false, err
		}
		for _, raw := range rendered {
			reference, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			kind, _, _ := unstructured.NestedString(reference, "kind")
			if !scalableKinds[kind] {
				continue
			}
			name, _, _ := unstructured.NestedString(reference, "name")
			namespace, _, _ := unstructured.NestedString(reference, "namespace")
			group, _, _ := unstructured.NestedString(reference, "group")
			version, _, _ := unstructured.NestedString(reference, "version")
			workload, err := c.store.Get(ctx, write.Ref{Group: group, Version: version, Kind: kind, Namespace: namespace, Name: name})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			if !exited(workload) {
				return false, nil
			}
		}
	}
	return true, nil
}

// exited reports whether one scalable workload asks for zero replicas and has
// none ready.
func exited(workload *unstructured.Unstructured) bool {
	replicas, found, err := unstructured.NestedInt64(workload.Object, "spec", "replicas")
	if err != nil || !found {
		// An unset count means the count the package rendered, which is not
		// zero: the workload has not been stopped (C-66).
		return false
	}
	if replicas != 0 {
		return false
	}
	ready, _, _ := unstructured.NestedInt64(workload.Object, "status", "readyReplicas")
	return ready == 0
}

// ensureTarget creates the migration target claim or confirms the one this
// migration owns. A claim another writer owns under the same name is a problem
// the attempt cannot fix (C-40).
func (c *Controller) ensureTarget(ctx context.Context, migration *v1beta1.VolumeMigration, next *attempt) (string, error) {
	claim, err := c.store.GetResource(ctx, pvcResource, migration.Namespace, next.targetClaim)
	switch {
	case apierrors.IsNotFound(err):
		if _, err := c.store.CreateClaim(ctx, desiredClaim(migration, next)); err != nil {
			return "", err
		}
		return "", nil
	case err != nil:
		return "", err
	}
	if claim.GetLabels()[markers.OwnerUID] != string(migration.UID) {
		return fmt.Sprintf("the claim %q exists and is not this migration's target", next.targetClaim), nil
	}
	return "", nil
}

// ensureJob creates one step Job or returns the one this migration owns, so a
// repeated round observes the Job instead of creating a second one (C-06).
func (c *Controller) ensureJob(ctx context.Context, migration *v1beta1.VolumeMigration, step string, next *attempt) (*unstructured.Unstructured, string, error) {
	name := jobName(next.targetClaim, step)
	job, err := c.store.GetResource(ctx, jobResource, migration.Namespace, name)
	switch {
	case apierrors.IsNotFound(err):
		created, err := c.store.CreateJob(ctx, migration.Namespace, desiredJob(migration, name, step, c.image, next))
		if err != nil {
			return nil, "", err
		}
		return created, "", nil
	case err != nil:
		return nil, "", err
	}
	if job.GetLabels()[markers.OwnerUID] != string(migration.UID) {
		return nil, fmt.Sprintf("the Job %q exists and does not belong to this migration", name), nil
	}
	return job, "", nil
}

// deleteJobs removes the step Jobs of this migration; a missing Job is already
// the wanted state.
func (c *Controller) deleteJobs(ctx context.Context, migration *v1beta1.VolumeMigration) error {
	for _, step := range []string{StepVerify, StepCopy} {
		name := jobName(targetClaimName(migration), step)
		ref := write.Ref{Group: "batch", Version: "v1", Kind: "Job", Namespace: migration.Namespace, Name: name}
		if err := c.store.DeleteJob(ctx, migration.Namespace, ref); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// enter writes the phase a completed phase moves into, with the rollback point
// the new phase would return to (C-40).
func (c *Controller) enter(ctx context.Context, migration *v1beta1.VolumeMigration, phase, message string) (reconcile.Result, error) {
	status := baseStatus(migration)
	status.Phase = phase
	status.RollbackPoint = ""
	if rollbackPhases[phase] {
		status.RollbackPoint = phase
	}
	status.Health = v1beta1.Health{State: v1beta1.HealthStateHealthy}
	status.Conditions = state.Set(status.Conditions, state.Blocked(migration.Generation, phase, message))
	return reconcile.Result{RequeueAfter: phaseResync}, c.writeStatus(ctx, migration, status)
}

// wait keeps the phase the object is in and reports what it waits for; a
// repeated round with the same reason writes nothing (C-06).
func (c *Controller) wait(ctx context.Context, migration *v1beta1.VolumeMigration, phase, message string) (reconcile.Result, error) {
	status := baseStatus(migration)
	status.Phase = phase
	status.Health = v1beta1.Health{State: v1beta1.HealthStateHealthy}
	status.Conditions = state.Set(status.Conditions, state.Blocked(migration.Generation, phase, message))
	return reconcile.Result{RequeueAfter: phaseResync}, c.writeStatus(ctx, migration, status)
}

// invalid records a request the flow cannot attempt at all: no phase ran and
// nothing outside the migration object was written (C-40).
func (c *Controller) invalid(ctx context.Context, migration *v1beta1.VolumeMigration, message string) (reconcile.Result, error) {
	status := baseStatus(migration)
	status.Phase = PhaseFailed
	status.RollbackPoint = ""
	status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
	status.Failure = &v1beta1.MigrationFailure{Phase: PhasePreflight, Message: message}
	status.Conditions = state.Set(status.Conditions, state.Blocked(migration.Generation, PhasePreflight, message))
	status.Conditions = state.Set(status.Conditions, state.Failure(ConditionInvalidSpec, migration.Generation, PhasePreflight, message))
	return reconcile.Result{}, c.writeStatus(ctx, migration, status)
}

// fail rolls the attempt back and records the phase and reason it failed in.
// The rollback returns the volume to the claim the attempt started from and
// removes everything the attempt created (C-40).
func (c *Controller) fail(ctx context.Context, migration *v1beta1.VolumeMigration, phase, message string) (reconcile.Result, error) {
	if err := c.rollback(ctx, migration, phase); err != nil {
		return reconcile.Result{}, err
	}
	status := baseStatus(migration)
	status.Phase = PhaseFailed
	status.RollbackPoint = ""
	status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
	status.Failure = &v1beta1.MigrationFailure{Phase: phase, Message: message}
	status.Conditions = state.Set(status.Conditions, state.Blocked(migration.Generation, phase, message))
	status.Conditions = state.Set(status.Conditions, state.Failure(ConditionFailed, migration.Generation, phase, message))
	return reconcile.Result{}, c.writeStatus(ctx, migration, status)
}

// rollback undoes what the failed phase created: the step Jobs, the target
// claim, and the binding when the attempt had replaced it. A Preflight failure
// changed nothing, so it has nothing to undo (C-40).
func (c *Controller) rollback(ctx context.Context, migration *v1beta1.VolumeMigration, phase string) error {
	if phase == PhasePreflight {
		return nil
	}
	if err := c.deleteJobs(ctx, migration); err != nil {
		return err
	}
	if err := c.deleteTarget(ctx, migration); err != nil {
		return err
	}
	return c.writeBinding(ctx, migration, migration.Status.SourceClaim, c.sourceClass(ctx, migration))
}

// remove runs the deletion path: the objects the flow created go away, and the
// claim the volume is bound to is left in place (C-04, C-40).
func (c *Controller) remove(ctx context.Context, migration *v1beta1.VolumeMigration) (reconcile.Result, error) {
	if err := c.deleteJobs(ctx, migration); err != nil {
		return reconcile.Result{}, err
	}
	if err := c.deleteTarget(ctx, migration); err != nil {
		return reconcile.Result{}, err
	}
	ref := write.Ref{Group: api.Group, Version: "v1beta1", Kind: "VolumeMigration", Namespace: migration.Namespace, Name: migration.Name}
	remaining := make([]string, 0, len(migration.Finalizers))
	for _, finalizer := range migration.Finalizers {
		if finalizer != Finalizer {
			remaining = append(remaining, finalizer)
		}
	}
	_, err := c.store.SetFinalizers(ctx, ref, remaining)
	return reconcile.Result{}, err
}

// deleteTarget removes the target claim unless the volume is now bound to it:
// after the cutover the claim belongs to the volume (C-40).
func (c *Controller) deleteTarget(ctx context.Context, migration *v1beta1.VolumeMigration) error {
	if migration.Status.TargetClaim == "" {
		return nil
	}
	// The binding the volume holds is the fact that decides ownership, not the
	// phase the migration recorded: the cutover writes the binding and the
	// phase in two writes, so a failed second write leaves the object in
	// Cutover with the volume already bound to the target (C-40).
	bound, found, err := c.volumeBinding(ctx, migration)
	if err != nil {
		return err
	}
	if found && bound == migration.Status.TargetClaim {
		return nil
	}
	if !found && (migration.Status.Phase == PhaseCleanup || migration.Status.Phase == PhaseSucceeded) {
		// The volume is gone after the cutover: the claim the flow created is
		// now the volume's data, and its release is the volume's to write. The
		// flow never deletes data it cannot prove it still owns (C-40, F6).
		return nil
	}
	ref := write.Ref{Version: "v1", Kind: "PersistentVolumeClaim", Namespace: migration.Namespace, Name: migration.Status.TargetClaim}
	if err := c.store.DeleteClaim(ctx, ref); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// volumeBinding reads the claim the volume is bound to. found is false when the
// volume object cannot be read, which the caller tells apart from a volume that
// is bound to something else.
func (c *Controller) volumeBinding(ctx context.Context, migration *v1beta1.VolumeMigration) (string, bool, error) {
	if migration.Spec.Volume == "" {
		return "", false, nil
	}
	volume, err := c.store.Get(ctx, write.Ref{Group: api.Group, Version: "v1beta1", Kind: "Volume", Namespace: migration.Namespace, Name: migration.Spec.Volume})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	bound, _, _ := unstructured.NestedString(volume.Object, "status", "boundClaim")
	return bound, true, nil
}

// sourceClass reads the policy the source claim carries, which is what a
// rollback restores as the volume's actual policy.
func (c *Controller) sourceClass(ctx context.Context, migration *v1beta1.VolumeMigration) string {
	claim, err := c.store.GetResource(ctx, pvcResource, migration.Namespace, migration.Status.SourceClaim)
	if err != nil {
		return ""
	}
	class, _, _ := unstructured.NestedString(claim.Object, "spec", "storageClassName")
	return class
}

// writeBinding writes the two volume status fields C-40 allocates to the flow:
// the claim the volume is bound to and the policy that claim resolved.
func (c *Controller) writeBinding(ctx context.Context, migration *v1beta1.VolumeMigration, claim, class string) error {
	if claim == "" {
		return nil
	}
	status := map[string]any{"boundClaim": claim}
	if class != "" {
		status["storageClass"] = class
	}
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Volume",
		"metadata": map[string]any{
			"name":      migration.Spec.Volume,
			"namespace": migration.Namespace,
		},
		"status": status,
	}}
	_, err := c.store.ApplyVolumeStatus(ctx, document)
	return err
}

// writeStatus writes the status only when it changed, so a repeat observation
// produces no write (C-06).
func (c *Controller) writeStatus(ctx context.Context, migration *v1beta1.VolumeMigration, status v1beta1.VolumeMigrationStatus) error {
	if sameStatus(migration.Status, status) {
		return nil
	}
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return fmt.Errorf("encode the migration status: %w", err)
	}
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "VolumeMigration",
		"metadata": map[string]any{
			"name":      migration.Name,
			"namespace": migration.Namespace,
		},
		"status": encoded,
	}}
	_, err = c.store.ApplyStatus(ctx, document)
	return err
}

// baseStatus carries the facts a round starts from: the conditions, the claims
// the attempt owns and the window it counts from.
func baseStatus(migration *v1beta1.VolumeMigration) v1beta1.VolumeMigrationStatus {
	return v1beta1.VolumeMigrationStatus{
		ObjectStatus: v1beta1.ObjectStatus{
			Conditions: append([]metav1.Condition(nil), migration.Status.Conditions...),
			Health:     v1beta1.Health{State: v1beta1.HealthStateUnavailable},
		},
		SourceClaim:     migration.Status.SourceClaim,
		RollbackPoint:   migration.Status.RollbackPoint,
		TargetClaim:     migration.Status.TargetClaim,
		WindowStartedAt: migration.Status.WindowStartedAt,
		Failure:         migration.Status.Failure,
	}
}

// failureConditions are the condition types the controller owns, so a failure
// that no longer applies is removed instead of staying True forever (C-07).
var failureConditions = []string{ConditionInvalidSpec, ConditionFailed}

// clearFailures removes every failure condition the controller owns.
func clearFailures(conditions []metav1.Condition) []metav1.Condition {
	for _, conditionType := range failureConditions {
		meta.RemoveStatusCondition(&conditions, conditionType)
	}
	return conditions
}

// sameStatus compares two statuses by their encoding.
func sameStatus(left, right v1beta1.VolumeMigrationStatus) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

// expired reports whether the downtime window, counted from Quiesce, is over.
func expired(migration *v1beta1.VolumeMigration, now time.Time) bool {
	if migration.Status.WindowStartedAt == nil {
		return false
	}
	duration, err := time.ParseDuration(migration.Spec.Window.Duration)
	if err != nil || duration <= 0 {
		return false
	}
	return !now.Before(migration.Status.WindowStartedAt.Add(duration))
}

// targetClaimName is the claim the flow creates for one migration. It is
// derived from the migration name, so a retry of the same object addresses the
// same claim (C-06).
func targetClaimName(migration *v1beta1.VolumeMigration) string {
	return migration.Name + "-target"
}

// jobName is the step Job of one migration.
func jobName(targetClaim, step string) string {
	return targetClaim + "-" + step
}

// desiredClaim is the claim the flow creates for the copy: the source claim's
// size and access mode under the target policy, marked as this migration's
// (C-04, C-40).
func desiredClaim(migration *v1beta1.VolumeMigration, next *attempt) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      next.targetClaim,
			"namespace": migration.Namespace,
			"labels": map[string]any{
				markers.ManagedBy: write.ManagedByValue,
				markers.OwnerUID:  string(migration.UID),
			},
		},
		"spec": map[string]any{
			"accessModes":      next.accessModes,
			"storageClassName": migration.Spec.TargetStorageClass,
			"resources":        map[string]any{"requests": map[string]any{"storage": next.capacity}},
		},
	}}
}

// desiredJob is one step Job of the flow. The step and the two claims are the
// Job's arguments, so the runner needs no other input (C-40).
func desiredJob(migration *v1beta1.VolumeMigration, name, step, image string, next *attempt) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      name,
			"namespace": migration.Namespace,
			"labels": map[string]any{
				markers.ManagedBy: write.ManagedByValue,
				markers.OwnerUID:  string(migration.UID),
			},
		},
		"spec": map[string]any{
			"backoffLimit": int64(0),
			"template": map[string]any{
				"spec": map[string]any{
					"restartPolicy": "Never",
					// The runner reads and writes the two claims and nothing
					// else: it needs no API access and no privileges, and the
					// source is mounted read-only (C-40).
					"automountServiceAccountToken": false,
					"securityContext": map[string]any{
						"runAsNonRoot": true,
						"seccompProfile": map[string]any{
							"type": "RuntimeDefault",
						},
					},
					"volumes": []any{
						map[string]any{
							"name": "source",
							"persistentVolumeClaim": map[string]any{
								"claimName": next.sourceClaim,
								"readOnly":  true,
							},
						},
						map[string]any{
							"name": "target",
							"persistentVolumeClaim": map[string]any{
								"claimName": next.targetClaim,
							},
						},
					},
					"containers": []any{map[string]any{
						"name":  "migration",
						"image": image,
						// The step and the two mount paths are the whole input:
						// the claims are reachable only through these mounts.
						"args": []any{step, sourceMountPath, targetMountPath},
						"volumeMounts": []any{
							map[string]any{
								"name":      "source",
								"mountPath": sourceMountPath,
								"readOnly":  true,
							},
							map[string]any{
								"name":      "target",
								"mountPath": targetMountPath,
							},
						},
						"securityContext": map[string]any{
							"allowPrivilegeEscalation": false,
							"readOnlyRootFilesystem":   true,
							"capabilities": map[string]any{
								"drop": []any{"ALL"},
							},
						},
						"resources": map[string]any{
							"requests": map[string]any{"cpu": "100m", "memory": "128Mi"},
							"limits":   map[string]any{"cpu": "1", "memory": "512Mi"},
						},
					}},
				},
			},
		},
	}}
}

// Mount paths the step Job exposes to the runner: the source claim read-only
// and the target claim writable (C-40).
const (
	sourceMountPath = "/mnt/source"
	targetMountPath = "/mnt/target"
)

// Step Job outcomes (C-40).
const (
	jobRunning  = "running"
	jobComplete = "complete"
	jobFailed   = "failed"
)

// jobState reads a step Job's outcome from its conditions. A Job with neither
// condition is still running (C-11).
func jobState(job *unstructured.Unstructured) (string, string) {
	conditions, _, _ := unstructured.NestedSlice(job.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		conditionType, _, _ := unstructured.NestedString(condition, "type")
		status, _, _ := unstructured.NestedString(condition, "status")
		if status != "True" {
			continue
		}
		switch conditionType {
		case "Complete":
			return jobComplete, ""
		case "Failed":
			message, _, _ := unstructured.NestedString(condition, "message")
			if message == "" {
				message, _, _ = unstructured.NestedString(condition, "reason")
			}
			return jobFailed, message
		}
	}
	return jobRunning, ""
}

// contains reports whether names holds name.
func contains(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

// hasFinalizer reports whether the migration carries the deletion gate.
func hasFinalizer(migration *v1beta1.VolumeMigration) bool {
	for _, finalizer := range migration.Finalizers {
		if finalizer == Finalizer {
			return true
		}
	}
	return false
}
