package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
)

// Event reasons the application controller records on the application's event
// stream (C-34, C-35, C-64).
const (
	// EventUpgradeCompleted marks an upgrade that was applied and verified by
	// its hook (C-34).
	EventUpgradeCompleted = "UpgradeCompleted"
	// EventUpgradeRolledBack marks an upgrade that was reverted to the previous
	// manifest set (C-34).
	EventUpgradeRolledBack = "UpgradeRolledBack"
	// EventExternalChangeConfirmed marks an external change accepted as the new
	// baseline (C-35).
	EventExternalChangeConfirmed = "ExternalChangeConfirmed"
)

// steady drives an application that already reached Ready: it watches the
// downstream objects for writes made outside KOS and performs the upgrade the
// spec requests (C-34, C-35).
func (c *Controller) steady(ctx context.Context, application *v1beta1.Application, definition *v1beta1.ApplicationDefinition, resolved map[string]any, comparison CapabilityResult, now time.Time) (reconcile.Result, error) {
	next, err := c.buildTarget(application, definition, resolved)
	if err != nil {
		return c.fail(ctx, application, ConditionInvalidPackage, err.Error())
	}
	applied := application.Status.Applied
	if applied == nil {
		// An application that reached Ready before the baseline existed adopts
		// its current render, so the next spec write still has a starting point.
		return c.recordBaseline(ctx, application, next, comparison, now)
	}

	// Drift is judged against the manifest set the last apply wrote: the kept
	// render converged with the lifecycle facts that apply recorded (C-35, C-66,
	// C-67). Comparing the live objects with a set converged from the current
	// spec would read the difference KOS itself has not written yet as an
	// external write, and a stop or a scale would then be skipped forever under
	// the adopt and ignore policies.
	expected, err := convergeApplied(applied)
	if err != nil {
		return reconcile.Result{}, err
	}
	drift, err := c.driftEntries(ctx, expected)
	if err != nil {
		return reconcile.Result{}, err
	}
	// An external change to an application whose upgrade declares an
	// irreversible change suspends reconciliation: KOS may neither write the old
	// version back nor continue the upgrade (C-35). Every irreversible class
	// counts, so a shrunk volume pauses exactly like a data format change.
	irreversible, _, err := c.irreversibleItems(ctx, application, applied, next)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(irreversible) > 0 && len(drift) > 0 {
		return c.suspend(ctx, application, next, definition, resolved, drift, now)
	}
	if next.applied.SpecDigest != applied.SpecDigest {
		return c.upgrade(ctx, application, next, definition, resolved, drift, comparison, now)
	}
	return c.reconcileApplied(ctx, application, definition, resolved, applied, drift, comparison, now)
}

// recordBaseline writes the target manifests and records them as the baseline
// of a completed install (C-34).
func (c *Controller) recordBaseline(ctx context.Context, application *v1beta1.Application, next *target, comparison CapabilityResult, now time.Time) (reconcile.Result, error) {
	defaults := resourceDefaults(application, next.definition)
	observed, rendered, err := c.applyManifests(ctx, application, next.objects, nil)
	if err != nil {
		if permanent(err) {
			return c.fail(ctx, application, ConditionInvalidPackage, err.Error())
		}
		return c.recordFailure(ctx, application, now, err.Error())
	}
	status := baseStatus(application)
	status.Phase = PhaseReady
	status.Rendered = rendered
	status.Applied = next.applied
	status.Upgrade = nil
	status.ResourceDefaults = defaults
	resetBudget(&status)
	status.Health = v1beta1.Health{State: applicationHealth(observed, comparison, application.Spec.Paused)}
	status.Resources, err = c.resourceStatus(ctx, application, next.resources)
	if err != nil {
		return reconcile.Result{}, err
	}
	settlePhase(application, &status)
	status.Conditions = state.Set(status.Conditions, state.Applied(application.Generation, "desired state applied"))
	status.Conditions = clearFailures(status.Conditions)
	if len(comparison.Unsatisfied) > 0 {
		message := "guarantees not met: " + strings.Join(comparison.Unsatisfied, ", ")
		status.Conditions = withFailure(status.Conditions, application.Generation, ConditionCapabilityUnsatisfied, message)
	}
	return reconcile.Result{RequeueAfter: readyResync}, c.writeStatus(ctx, application, status)
}

// upgrade runs the three steps of C-34 for the target the spec requests: the
// preflight is reported on its own round, then the manifests are applied and the
// package's upgrade hook decides whether they stay. An external write found on
// the way is answered by the drift policy, so a refusal does not swallow it
// (C-35).
func (c *Controller) upgrade(ctx context.Context, application *v1beta1.Application, next *target, definition *v1beta1.ApplicationDefinition, resolved map[string]any, drift []v1beta1.DriftEntry, comparison CapabilityResult, now time.Time) (reconcile.Result, error) {
	if rolledBack(application, next) {
		// A target that was applied and reverted is not attempted again: the
		// package has to change, and only then is there a new request (C-34).
		// The wait is a refusal like any other, so a write made outside KOS
		// while it lasts is answered by the drift policy rather than dropped
		// (C-35).
		message := "the previous attempt at this target was rolled back; write a new target to retry"
		return c.refuseUpgrade(ctx, application, *application.Status.Upgrade,
			ConditionUpgradeFailed, message, definition, resolved, drift, comparison, now)
	}
	if application.Status.Phase != PhaseUpgrading || !upgradePending(application, next) {
		report, err := c.preflight(ctx, application, application.Status.Applied, next)
		if err != nil {
			return reconcile.Result{}, err
		}
		if len(report.Irreversible) > 0 {
			message := "the upgrade is refused because it is irreversible: " + describeIrreversible(report.Irreversible)
			return c.refuseUpgrade(ctx, application, report, ConditionIrreversibleUpgrade, message, definition, resolved, drift, comparison, now)
		}
		if len(report.Migrations) > 0 {
			message := "the storage class change requires a migration: " + describeRefs(report.Migrations)
			return c.refuseUpgrade(ctx, application, report, ConditionMigrationRequired, message, definition, resolved, drift, comparison, now)
		}
		status := baseStatus(application)
		status.Phase = PhaseUpgrading
		status.Upgrade = &report
		status.Conditions = state.Set(status.Conditions, state.Blocked(
			application.Generation, PhaseUpgrading, "the preflight passed; the new manifests are being applied"))
		return reconcile.Result{RequeueAfter: time.Second}, c.writeStatus(ctx, application, status)
	}

	observed, rendered, err := c.applyManifests(ctx, application, next.objects, nil)
	if err != nil {
		return c.rollback(ctx, application, fmt.Errorf("the upgrade could not write its manifests: %w", err), comparison, now)
	}
	if failure := c.runUpgradeHook(ctx, next); failure != nil {
		return c.rollback(ctx, application, failure, comparison, now)
	}
	status := baseStatus(application)
	status.Phase = PhaseReady
	status.Rendered = rendered
	status.Applied = next.applied
	status.Upgrade = nil
	status.Drift = nil
	resetBudget(&status)
	status.Health = v1beta1.Health{State: applicationHealth(observed, comparison, application.Spec.Paused)}
	status.Resources, err = c.resourceStatus(ctx, application, next.resources)
	if err != nil {
		return reconcile.Result{}, err
	}
	settlePhase(application, &status)
	status.Conditions = state.Set(status.Conditions, state.Applied(application.Generation, "the upgrade was applied"))
	status.Conditions = clearFailures(status.Conditions)
	if len(comparison.Unsatisfied) > 0 {
		message := "guarantees not met: " + strings.Join(comparison.Unsatisfied, ", ")
		status.Conditions = withFailure(status.Conditions, application.Generation, ConditionCapabilityUnsatisfied, message)
	}
	c.recordEvent(ctx, application, "Normal", EventUpgradeCompleted,
		fmt.Sprintf("the application was upgraded to %s", next.applied.DefinitionDigest), now)
	return reconcile.Result{RequeueAfter: readyResync}, c.writeStatus(ctx, application, status)
}

// refuseUpgrade keeps the application on the baseline it already applied and
// reports why the upgrade cannot run. No confirmation bypasses the refusal
// (C-34). An external write seen in the same round is answered by the drift
// policy rather than dropped: the reconcile policy writes the baseline back,
// adopt records what the config can express, and ignore only reports (C-35).
func (c *Controller) refuseUpgrade(ctx context.Context, application *v1beta1.Application, report v1beta1.UpgradeStatus, conditionType, message string, definition *v1beta1.ApplicationDefinition, resolved map[string]any, drift []v1beta1.DriftEntry, comparison CapabilityResult, now time.Time) (reconcile.Result, error) {
	status := baseStatus(application)
	status.Phase = application.Status.Phase
	if status.Phase == PhaseUpgrading {
		status.Phase = PhaseReady
	}
	status.Applied = application.Status.Applied
	status.Upgrade = &report
	status.Health = application.Status.Health
	status.Conditions = state.Set(status.Conditions, state.Blocked(application.Generation, conditionType, message))
	status.Conditions = withFailure(status.Conditions, application.Generation, conditionType, message)
	// The drift record is a fact of this round: entries a previous round left
	// do not survive a round that found nothing to answer (C-35).
	status.Drift = nil
	if len(drift) > 0 {
		if driftPolicyOf(application) == v1beta1.DriftPolicyReconcile && application.Status.Applied != nil {
			previous, err := decodeManifests(application.Status.Applied.Render)
			if err != nil {
				return reconcile.Result{}, err
			}
			resources := convergeManifests(application, frozenDefaults(application), previous)
			observed, rendered, err := c.applyManifests(ctx, application, previous, nil)
			if err != nil {
				return c.recordFailure(ctx, application, now, err.Error())
			}
			status.Applied = withLifecycle(application.Status.Applied, application, resources)
			status.Rendered = rendered
			status.Health = v1beta1.Health{State: applicationHealth(observed, comparison, application.Spec.Paused)}
			status.Resources, err = c.resourceStatus(ctx, application, resources)
			if err != nil {
				return reconcile.Result{}, err
			}
		}
		if err := c.driftState(application, definition, resolved, drift, false, &status); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{RequeueAfter: readyResync}, c.writeStatus(ctx, application, status)
}

// driftState writes what one drift policy makes observable on a round that keeps
// the baseline. Adopt records the values the config structure can carry, ignore
// leaves them and only reports, and reconcile says whether it wrote the desired
// values back or is still waiting for a confirmation (C-35). The record is a
// status fact, so it does not clear the failure condition of the round.
func (c *Controller) driftState(application *v1beta1.Application, definition *v1beta1.ApplicationDefinition, resolved map[string]any, drift []v1beta1.DriftEntry, pending bool, status *v1beta1.ApplicationStatus) error {
	status.Drift = nil
	report := func(text string) {
		status.Conditions = state.Set(status.Conditions,
			state.Failure(ConditionDrifted, application.Generation, ConditionDrifted, text))
	}
	switch driftPolicyOf(application) {
	case v1beta1.DriftPolicyAdopt:
		expressible, err := configPaths(definition.Spec, renderInput(application, resolved), resolved)
		if err != nil {
			return err
		}
		entries, recordable := recordableDrift(drift, expressible)
		if !recordable {
			report("the external values cannot be expressed by the config structure: " + describeDrift(drift))
			return nil
		}
		status.Drift = entries
		report("downstream values written outside KOS are recorded: " + describeDrift(drift))
	case v1beta1.DriftPolicyIgnore:
		report("downstream values written outside KOS are left in place: " + describeDrift(drift))
	default:
		if pending {
			report("the reconcile policy owns these fields; the write-back waits for the confirmation: " + describeDrift(drift))
			return nil
		}
		report("downstream values written outside KOS are written back: " + describeDrift(drift))
	}
	return nil
}

// rollback re-applies the previous manifest set after a failed upgrade and
// removes what the failed attempt added, so the manifest set the cluster holds is
// the previous one rather than the union of both (C-34). It restores the object
// shapes only: the data of the previous version is what the backup point holds,
// not something this step promises.
func (c *Controller) rollback(ctx context.Context, application *v1beta1.Application, cause error, comparison CapabilityResult, now time.Time) (reconcile.Result, error) {
	applied := application.Status.Applied
	previous, err := decodeManifests(applied.Render)
	if err != nil {
		return c.fail(ctx, application, ConditionUpgradeFailed, cause.Error()+"; the previous manifests cannot be read: "+err.Error())
	}
	resources := convergeManifests(application, frozenDefaults(application), previous)
	observed, rendered, err := c.applyManifests(ctx, application, previous, nil)
	if err != nil {
		return c.fail(ctx, application, ConditionUpgradeFailed, cause.Error()+"; the previous manifests cannot be re-applied: "+err.Error())
	}
	status := baseStatus(application)
	status.Phase = PhaseReady
	status.Rendered = rendered
	status.Applied = withLifecycle(applied, application, resources)
	status.Upgrade = rolledBackReport(application)
	status.Health = v1beta1.Health{State: applicationHealth(observed, comparison, application.Spec.Paused)}
	status.Resources, err = c.resourceStatus(ctx, application, resources)
	if err != nil {
		return reconcile.Result{}, err
	}
	settlePhase(application, &status)
	status.Conditions = state.Set(status.Conditions, state.Applied(application.Generation, "the previous manifests were re-applied"))
	status.Conditions = withFailure(status.Conditions, application.Generation, ConditionUpgradeFailed, cause.Error())
	c.recordEvent(ctx, application, "Warning", EventUpgradeRolledBack, cause.Error(), now)
	return reconcile.Result{RequeueAfter: readyResync}, c.writeStatus(ctx, application, status)
}

// suspend holds an application whose upgrade declares an irreversible change
// after an external write: nothing is written back and no upgrade runs until the
// user confirms the target (C-35). The drift policy still decides what the pause
// makes observable, so the three policies stay distinguishable while KOS waits.
func (c *Controller) suspend(ctx context.Context, application *v1beta1.Application, next *target, definition *v1beta1.ApplicationDefinition, resolved map[string]any, drift []v1beta1.DriftEntry, now time.Time) (reconcile.Result, error) {
	report, err := c.preflight(ctx, application, application.Status.Applied, next)
	if err != nil {
		return reconcile.Result{}, err
	}
	status := baseStatus(application)
	status.Phase = PhaseReady
	status.Applied = application.Status.Applied
	status.Upgrade = &report
	status.Health = v1beta1.Health{State: v1beta1.HealthStateDegraded}
	message := "external changes await confirmation: " + describeDrift(drift)
	status.Conditions = state.Set(status.Conditions, state.Blocked(application.Generation, ReasonExternalChangePending, message))
	status.Conditions = withFailure(status.Conditions, application.Generation, ConditionIrreversibleUpgrade, describeIrreversible(report.Irreversible))
	if err := c.driftState(application, definition, resolved, drift, true, &status); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: pendingResync}, c.writeStatus(ctx, application, status)
}

// confirm consumes a confirmation record. The pause clears only when the record
// names the target the stored spec requests, was written against that same spec,
// and the target is what the cluster actually holds (C-35). The third condition
// is an observation of the downstream objects, never a second render of the
// same spec: otherwise a confirmation would stand in for an upgrade the
// preflight refused to run. Any other record keeps the pause and is consumed
// without effect.
func (c *Controller) confirm(ctx context.Context, application *v1beta1.Application, record *unstructured.Unstructured, now time.Time) (reconcile.Result, error) {
	target, _, _ := unstructured.NestedString(record.Object, "data", ActionTargetDigestKey)
	stored, _, _ := unstructured.NestedString(record.Object, "data", ActionSpecDigestKey)
	status := baseStatus(application)
	status.Phase = application.Status.Phase
	if !paused(application) {
		// A confirmation resumes a pause. Outside one it has nothing to accept,
		// so it is consumed and the next round runs the normal path (C-35).
		return c.consumeRecord(ctx, record)
	}
	next, failure := c.currentTarget(ctx, application)
	observed := ""
	if failure == nil {
		observed, failure = c.observedRenderDigest(ctx, next.objects)
	}
	agreed := failure == nil && target != "" && target == next.convergedDigest &&
		target == observed && stored == next.applied.SpecDigest
	if agreed {
		if status.Phase != PhaseUpgrading {
			status.Phase = PhaseReady
		}
		status.Rendered = mergeRefs(application.Status.Rendered, refsOf(next.objects))
		status.Applied = next.applied
		status.Upgrade = nil
		status.Drift = nil
		status.Health = application.Status.Health
		status.Conditions = state.Set(status.Conditions,
			state.Applied(application.Generation, "the external change is the new baseline"))
		status.Conditions = clearFailures(status.Conditions)
		c.recordEvent(ctx, application, "Normal", EventExternalChangeConfirmed,
			"the external change was confirmed as the new baseline", now)
	} else {
		status.Conditions = state.Set(status.Conditions,
			state.Blocked(application.Generation, ReasonExternalChangePending, confirmFailure(failure, target, stored, next)))
	}
	if err := c.writeStatus(ctx, application, status); err != nil {
		return reconcile.Result{}, err
	}
	if err := c.deleteRecord(ctx, record); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: readyResync}, nil
}

// confirmFailure says which of the three conditions of C-35 the record did not
// meet, so a user can tell a stale record from a target the cluster does not
// hold.
func confirmFailure(failure error, target, stored string, next *target) string {
	if failure != nil {
		return "the confirmation cannot be judged: " + failure.Error()
	}
	switch {
	case target == "":
		return "the confirmation names no target"
	case stored != next.applied.SpecDigest:
		return "the spec changed after the confirmation was written"
	case target != next.convergedDigest:
		return "the confirmation names a target the spec does not request"
	default:
		return "the downstream objects do not hold the confirmed target"
	}
}

// consumeRecord deletes a record that has no effect and lets a later round run
// the normal path (C-35).
func (c *Controller) consumeRecord(ctx context.Context, record *unstructured.Unstructured) (reconcile.Result, error) {
	if err := c.deleteRecord(ctx, record); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: readyResync}, nil
}

// reconcileApplied keeps a settled application on its baseline: the desired
// manifests are re-applied under the reconcile policy, while the adopt and
// ignore policies leave externally written values in place and report them
// (C-35).
func (c *Controller) reconcileApplied(ctx context.Context, application *v1beta1.Application, definition *v1beta1.ApplicationDefinition, resolved map[string]any, applied *v1beta1.AppliedState, drift []v1beta1.DriftEntry, comparison CapabilityResult, now time.Time) (reconcile.Result, error) {
	status := baseStatus(application)
	status.Applied = applied
	skip := map[string]bool{}
	kept := false
	if len(drift) > 0 {
		switch driftPolicyOf(application) {
		case v1beta1.DriftPolicyAdopt:
			expressible, err := configPaths(definition.Spec, renderInput(application, resolved), resolved)
			if err != nil {
				return reconcile.Result{}, err
			}
			entries, recordable := recordableDrift(drift, expressible)
			status.Drift = nil
			if recordable {
				status.Drift = entries
			}
			kept = true
		case v1beta1.DriftPolicyIgnore:
			status.Drift = nil
			kept = true
		default:
			// reconcile writes the desired value back in the apply below.
			status.Drift = nil
		}
		if kept {
			skip = driftedObjects(drift)
			status.Conditions = withFailure(status.Conditions, application.Generation, ConditionDrifted,
				"downstream objects were written outside KOS: "+describeDrift(drift))
		}
	} else {
		status.Drift = nil
	}
	if !kept {
		status.Conditions = clearFailures(status.Conditions)
	}

	objects, err := decodeManifests(applied.Render)
	if err != nil {
		return reconcile.Result{}, err
	}
	resources := convergeManifests(application, frozenDefaults(application), objects)
	observed, rendered, err := c.applyManifests(ctx, application, objects, skip)
	if err != nil {
		if permanent(err) {
			return c.fail(ctx, application, ConditionInvalidPackage, err.Error())
		}
		return c.recordFailure(ctx, application, now, err.Error())
	}
	status.Applied = withLifecycle(applied, application, resources)
	status.Rendered = rendered
	health := applicationHealth(observed, comparison, application.Spec.Paused)
	if kept {
		health = v1beta1.HealthStateDegraded
	}
	status.Health = v1beta1.Health{State: health}
	status.Resources, err = c.resourceStatus(ctx, application, resources)
	if err != nil {
		return reconcile.Result{}, err
	}
	status.Phase = PhaseReady
	settlePhase(application, &status)
	status.Conditions = state.Set(status.Conditions, state.Applied(application.Generation, "desired state applied"))
	return reconcile.Result{RequeueAfter: readyResync}, c.writeStatus(ctx, application, status)
}

// applyManifests brings the downstream objects to exactly one manifest set: every
// object of the set is written, and every object the previous round recorded that
// the set no longer carries is deleted, so the cluster holds one version rather
// than the union of two (C-34). The returned references are what a later
// uninstall deletes, so they name the set that is actually stored; an object
// whose deletion failed stays recorded for the next attempt. A failed write
// records the objects written so far, so a later uninstall still sees them
// (C-33).
func (c *Controller) applyManifests(ctx context.Context, application *v1beta1.Application, objects []*unstructured.Unstructured, skip map[string]bool) ([]*unstructured.Unstructured, []v1beta1.ObjectRef, error) {
	kept := make(map[string]bool, len(objects))
	records := make([]v1beta1.ObjectRef, 0, len(objects))
	for _, object := range objects {
		kept[identityOfRef(objectRefOf(object))] = true
		records = append(records, objectRefOf(object))
	}
	observed := make([]*unstructured.Unstructured, 0, len(objects))
	for _, object := range objects {
		if skip[identityOfRef(objectRefOf(object))] {
			continue
		}
		stampOwner(object, string(application.UID))
		stored, err := c.store.Apply(ctx, application.Spec.Namespace, object)
		if err != nil {
			application.Status.Rendered = mergeRefs(application.Status.Rendered, records)
			return nil, application.Status.Rendered, err
		}
		observed = append(observed, stored)
	}
	remaining := records
	for _, ref := range application.Status.Rendered {
		if kept[identityOfRef(ref)] {
			continue
		}
		namespace := ref.Namespace
		if namespace == "" {
			namespace = application.Spec.Namespace
		}
		if err := c.store.Delete(ctx, namespace, referenceOf(ref)); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			remaining = append(remaining, ref)
			application.Status.Rendered = remaining
			return nil, remaining, err
		}
	}
	application.Status.Rendered = remaining
	return observed, remaining, nil
}

// refsOf lists the references of one manifest set.
func refsOf(objects []*unstructured.Unstructured) []v1beta1.ObjectRef {
	refs := make([]v1beta1.ObjectRef, 0, len(objects))
	for _, object := range objects {
		refs = append(refs, objectRefOf(object))
	}
	return refs
}

// mergeRefs unions two reference lists, keeping the first sighting of each
// object.
func mergeRefs(left, right []v1beta1.ObjectRef) []v1beta1.ObjectRef {
	merged := append([]v1beta1.ObjectRef(nil), left...)
	for _, ref := range right {
		merged = appendUniqueRef(merged, ref)
	}
	return merged
}

// paused reports the external-change pause of C-35: reconciliation is on hold
// until the user confirms what was written outside KOS.
func paused(application *v1beta1.Application) bool {
	condition, present := state.Find(application.Status.Conditions, api.ConditionReconciled)
	return present && condition.Status == metav1.ConditionFalse && condition.Reason == ReasonExternalChangePending
}

// currentTarget renders the desired state the stored spec names.
func (c *Controller) currentTarget(ctx context.Context, application *v1beta1.Application) (*target, error) {
	definition, err := c.definition(ctx, application.Spec.DefinitionRef)
	if err != nil {
		return nil, err
	}
	if definition == nil {
		return nil, fmt.Errorf("definition %q does not exist", application.Spec.DefinitionRef)
	}
	if err := ValidateDefinition(definition.Spec); err != nil {
		return nil, err
	}
	config, err := decodeConfig(application.Spec.Config)
	if err != nil {
		return nil, err
	}
	resolved, err := Resolve(definition.Spec.Config, config)
	if err != nil {
		return nil, err
	}
	return c.buildTarget(application, definition, resolved)
}

// runUpgradeHook runs the package hook the upgrade depends on (C-32, C-34).
func (c *Controller) runUpgradeHook(ctx context.Context, next *target) error {
	for _, hook := range next.definition.Spec.Hooks {
		if hook.Name != HookUpgrade {
			continue
		}
		if err := RunHook(ctx, hook); err != nil {
			return err
		}
	}
	return nil
}

// recordEvent appends one event to the application's stream (C-64). An event is
// evidence rather than desired state, so a failed write does not fail the round
// that produced it.
func (c *Controller) recordEvent(ctx context.Context, application *v1beta1.Application, eventType, reason, message string, now time.Time) {
	_ = c.store.RecordEvent(ctx, application, eventType, reason, message, now)
}

// upgradePending reports that the preflight already ran for exactly this
// target, so the next round of the upgrade may apply it.
func upgradePending(application *v1beta1.Application, next *target) bool {
	report := application.Status.Upgrade
	if report == nil || report.RolledBack {
		return false
	}
	return report.Target == next.applied.DefinitionDigest &&
		report.SpecDigest == next.applied.SpecDigest &&
		report.RenderDigest == next.convergedDigest
}

// rolledBack reports that the current target was already attempted and reverted
// (C-34).
func rolledBack(application *v1beta1.Application, next *target) bool {
	report := application.Status.Upgrade
	if report == nil || !report.RolledBack {
		return false
	}
	return report.Target == next.applied.DefinitionDigest && report.SpecDigest == next.applied.SpecDigest
}

// rolledBackReport marks the preflight report of a reverted target, so the same
// target is not applied again (C-34).
func rolledBackReport(application *v1beta1.Application) *v1beta1.UpgradeStatus {
	report := application.Status.Upgrade
	if report == nil {
		return nil
	}
	marked := *report
	marked.Irreversible = append([]v1beta1.IrreversibleItem(nil), report.Irreversible...)
	marked.Migrations = append([]v1beta1.ObjectRef(nil), report.Migrations...)
	marked.ConfigChanges = append([]v1beta1.ConfigChange(nil), report.ConfigChanges...)
	marked.RenderChanges = append([]v1beta1.RenderChange(nil), report.RenderChanges...)
	marked.RolledBack = true
	return &marked
}

// driftPolicyOf reads the policy of an application; an empty value is the
// reconcile default of C-35.
func driftPolicyOf(application *v1beta1.Application) v1beta1.DriftPolicy {
	if application.Spec.DriftPolicy == "" {
		return v1beta1.DriftPolicyReconcile
	}
	return application.Spec.DriftPolicy
}

// renderInput is what a translation template reads for one application (C-32).
func renderInput(application *v1beta1.Application, config map[string]any) RenderInput {
	return RenderInput{
		Name:      application.Name,
		Namespace: application.Spec.Namespace,
		Config:    config,
		Volumes:   volumeClaims(application.Spec.Volumes),
	}
}

// describeIrreversible renders the refused items for a condition message.
func describeIrreversible(items []v1beta1.IrreversibleItem) string {
	described := make([]string, 0, len(items))
	for _, item := range items {
		text := item.Kind
		if item.Object != nil {
			text += " " + item.Object.Kind + " " + item.Object.Name
		}
		described = append(described, text)
	}
	return strings.Join(described, ", ")
}

// describeRefs renders object references for a condition message.
func describeRefs(refs []v1beta1.ObjectRef) string {
	described := make([]string, 0, len(refs))
	for _, ref := range refs {
		described = append(described, ref.Kind+" "+ref.Name)
	}
	sort.Strings(described)
	return strings.Join(described, ", ")
}

// describeDrift renders the externally written fields for a condition message.
func describeDrift(entries []v1beta1.DriftEntry) string {
	described := make([]string, 0, len(entries))
	for _, entry := range entries {
		object := entry.Object.Kind + " " + entry.Object.Name
		if entry.Path == "" {
			described = append(described, object+" is missing")
			continue
		}
		described = append(described, object+" "+entry.Path)
	}
	sort.Strings(described)
	return strings.Join(described, ", ")
}
