package app

import (
	"context"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/migration"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
)

// migrationResource is the migration the application observes to learn that a
// volume it mounts is being moved (C-40).
var migrationResource = schema.GroupVersionResource{Group: api.Group, Version: "v1beta1", Resource: "volumemigrations"}

// ConditionQuiesced reports the stop a migration or a node in maintenance
// forces on the application (C-40, C-65). It is a state condition, not a
// failure: the workloads are stopped on purpose, so the phase is held and
// health follows the stop rules of C-66.
const ConditionQuiesced = "Quiesced"

// ReasonMigrationInProgress is the reason of a quiesce the migration flow
// waits for (C-40).
const ReasonMigrationInProgress = "MigrationInProgress"

// ReasonNodeMaintenance is the reason of a stop a node the run kernel took out
// of scheduling forces (C-65).
const ReasonNodeMaintenance = "NodeMaintenance"

// migrationStopSpan reports whether a migration phase requires the workloads of
// the applications that mount the volume to stay stopped: from Quiesce until the
// binding is cut over. Cleanup runs after the cutover, so the application may
// start again without waiting for it (C-40).
func migrationStopSpan(phase string) bool {
	switch phase {
	case migration.PhaseQuiesce, migration.PhaseCopy, migration.PhaseVerify, migration.PhaseCutover:
		return true
	}
	return false
}

// quiesceFor returns the migration the application must stop for: the one an
// application that mounts the volume is waiting on. An application that
// declares no volume never quiesces (C-40).
func (c *Controller) quiesceFor(ctx context.Context, application *v1beta1.Application) (string, error) {
	if len(application.Spec.Volumes) == 0 {
		return "", nil
	}
	migrations, err := c.store.List(ctx, migrationResource, application.Spec.Namespace, "")
	if err != nil {
		return "", err
	}
	for index := range migrations {
		stored := &migrations[index]
		volume, _, err := unstructured.NestedString(stored.Object, "spec", "volume")
		if err != nil {
			return "", err
		}
		if !slices.Contains(application.Spec.Volumes, volume) {
			continue
		}
		phase, _, err := unstructured.NestedString(stored.Object, "status", "phase")
		if err != nil {
			return "", err
		}
		if migrationStopSpan(phase) {
			return stored.GetName(), nil
		}
	}
	return "", nil
}

// stopCause is one reason the controller drives the workloads of an
// application to zero: a migration moving a volume (C-40) or a node in
// maintenance (C-65).
type stopCause struct {
	// reason is what the Quiesced condition records.
	reason string
	// message is what the Quiesced condition says the stop is for.
	message string
}

// quiesce drives the workloads of an application to zero for the migration the
// flow is running, records why, and holds the phase (C-40, C-66).
func (c *Controller) quiesce(ctx context.Context, application *v1beta1.Application, migrationName string, comparison CapabilityResult, now time.Time) (reconcile.Result, error) {
	return c.stop(ctx, application, stopCause{
		reason:  ReasonMigrationInProgress,
		message: fmt.Sprintf("the workloads are stopped because migration %q is moving the volume", migrationName),
	}, comparison, now)
}

// stop drives the workloads of an application to zero for one cause, records
// why, and holds the phase: the application spec is never written, so the
// volumes, the replicas and the entries survive the stop (C-40, C-65, C-66).
func (c *Controller) stop(ctx context.Context, application *v1beta1.Application, cause stopCause, comparison CapabilityResult, now time.Time) (reconcile.Result, error) {
	status := baseStatus(application)
	status.Phase = application.Status.Phase
	if status.Phase == "" {
		status.Phase = PhasePending
	}
	applied := application.Status.Applied
	if applied == nil {
		// Nothing was applied yet, so there is no workload to stop: the round
		// records the stop and waits for the cause to clear (C-06).
		status.Health = v1beta1.Health{State: c.applicationHealth(ctx, application, nil, comparison, true)}
		status.Conditions = state.Set(status.Conditions, quiescedCondition(application, cause))
		return reconcile.Result{RequeueAfter: readyResync}, c.writeStatus(ctx, application, status)
	}
	objects, err := decodeManifests(applied.Render)
	if err != nil {
		return reconcile.Result{}, err
	}
	resources := convergeManifests(application, frozenDefaults(application), objects)
	Converge(objects, WorkloadTarget{Paused: true, Resources: resources})
	observed, rendered, err := c.applyManifests(ctx, application, objects, nil)
	if err != nil {
		if permanent(err) {
			return c.fail(ctx, application, ConditionInvalidPackage, err.Error())
		}
		return c.recordFailure(ctx, application, now, err.Error())
	}
	status = baseStatus(application)
	status.Phase = application.Status.Phase
	if status.Phase == "" {
		status.Phase = PhasePending
	}
	// The baseline records the stopped shape it just applied, so the next round
	// does not read KOS's own stop as a write made outside KOS (C-35, C-40, C-65).
	status.Applied = stoppedBaseline(applied)
	status.Rendered = rendered
	status.Health = v1beta1.Health{State: c.applicationHealth(ctx, application, observed, comparison, true)}
	status.Resources, err = c.resourceStatus(ctx, application, resources)
	if err != nil {
		return reconcile.Result{}, err
	}
	status.Conditions = state.Set(status.Conditions, quiescedCondition(application, cause))
	return reconcile.Result{RequeueAfter: readyResync}, c.writeStatus(ctx, application, status)
}

// quiescedCondition is the stop reason an application records while a migration
// moves one of its volumes or a node is in maintenance (C-40, C-65).
func quiescedCondition(application *v1beta1.Application, cause stopCause) metav1.Condition {
	return state.Condition(ConditionQuiesced, metav1.ConditionTrue, application.Generation, cause.reason, cause.message)
}

// stoppedBaseline is the baseline of an application whose workloads were driven
// to zero for a stop: the pause fact is what the round wrote, so a later round
// compares the live objects with the shape KOS applied (C-35, C-40, C-65).
func stoppedBaseline(applied *v1beta1.AppliedState) *v1beta1.AppliedState {
	updated := applied.DeepCopy()
	facts := &v1beta1.LifecycleState{Paused: true}
	if applied.Lifecycle != nil {
		facts.Replicas = applied.Lifecycle.Replicas
		facts.Resources = applied.Lifecycle.Resources
	}
	updated.Lifecycle = facts
	return updated
}

// sameClaims reports whether two recorded binding sets agree. A nil set and an
// empty one agree: an application with no volumes records nothing either way.
func sameClaims(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, claim := range left {
		if right[name] != claim {
			return false
		}
	}
	return true
}
