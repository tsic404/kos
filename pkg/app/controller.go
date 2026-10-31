package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
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

// Phases of an application (C-33).
const (
	PhasePending      = "Pending"
	PhaseInstalling   = "Installing"
	PhaseReady        = "Ready"
	PhaseUpgrading    = "Upgrading"
	PhaseFailed       = "Failed"
	PhaseUninstalling = "Uninstalling"
)

// Condition types and reasons the controller writes (C-07, C-33). A failure
// condition carries its own type and never stands in for Reconciled.
const (
	ConditionCapabilityMissing     = "CapabilityMissing"
	ConditionCapabilityUnsatisfied = "CapabilityUnsatisfied"
	ConditionDefinitionMissing     = "DefinitionMissing"
	ConditionInvalidConfig         = "InvalidConfig"
	ConditionInvalidPackage        = "InvalidPackage"
	ConditionNamespaceMissing      = "NamespaceMissing"
	ConditionRetrying              = "Retrying"
	ConditionFailed                = "Failed"
	ConditionUninstallBlocked      = "UninstallBlocked"
	ConditionIrreversibleUpgrade   = "IrreversibleUpgrade"
	ConditionMigrationRequired     = "MigrationRequired"
	ConditionDrifted               = "Drifted"
	ConditionUpgradeFailed         = "UpgradeFailed"
)

// Reasons of the Reconciled condition the application controller writes itself
// (C-34, C-35).
const (
	// ReasonExternalChangePending is the pause an external write forces on an
	// application whose upgrade declares an irreversible change (C-35).
	ReasonExternalChangePending = "ExternalChangePending"
)

// Finalizer is what keeps a deleting application observable until its
// downstream objects are gone or abandoned (C-33, C-54).
const Finalizer = "kos.tsic.top/application"

// Resync intervals. The kernel has no watch source yet, so a settled object is
// re-observed only by its own bounded requeue (C-06). A blocked application
// rechecks the prerequisites C-33 waits for; a ready one re-reads its rendered
// workloads so health follows the observed replicas (C-08).
const (
	pendingResync = 30 * time.Second
	readyResync   = 30 * time.Second
)

// failureConditions are the condition types the controller owns, so a failure
// that no longer applies is removed instead of staying True forever.
var failureConditions = []string{
	ConditionCapabilityMissing,
	ConditionCapabilityUnsatisfied,
	ConditionDefinitionMissing,
	ConditionInvalidConfig,
	ConditionInvalidPackage,
	ConditionNamespaceMissing,
	ConditionRetrying,
	ConditionFailed,
	ConditionUninstallBlocked,
	ConditionIrreversibleUpgrade,
	ConditionMigrationRequired,
	ConditionDrifted,
	ConditionUpgradeFailed,
}

// Store is the object access one application round needs. NewWriterStore is the
// production implementation; a test substitutes its own to inject a failure.
type Store interface {
	// Get reads one object.
	Get(ctx context.Context, ref write.Ref) (*unstructured.Unstructured, error)
	// List reads the objects of one resource matching a label selector.
	List(ctx context.Context, resource schema.GroupVersionResource, namespace, selector string) ([]unstructured.Unstructured, error)
	// Apply writes one rendered object into its namespace (C-04). The
	// reconciler is the writer of its downstream objects, so the desired value
	// wins against a write made outside KOS (C-35).
	Apply(ctx context.Context, namespace string, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// ApplyStatus writes the status of one application (C-03).
	ApplyStatus(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// CreateRecord creates one immutable record in the KOS namespace: the
	// abandonment manifest of an orphaned application (C-04, C-33).
	CreateRecord(ctx context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// Delete removes one rendered object (C-04).
	Delete(ctx context.Context, namespace string, ref write.Ref) error
	// SetFinalizers writes the finalizer list of one application (C-33).
	SetFinalizers(ctx context.Context, ref write.Ref, finalizers []string) (*unstructured.Unstructured, error)
	// RecordEvent appends one event to the application's stream (C-64).
	RecordEvent(ctx context.Context, application *v1beta1.Application, eventType, reason, message string, now time.Time) error
}

// CapabilitySource lists the capabilities providers register (C-09).
type CapabilitySource interface {
	Capabilities(ctx context.Context) ([]Capability, error)
}

// StaticCapabilities is a fixed registration set.
type StaticCapabilities []Capability

// Capabilities implements CapabilitySource.
func (s StaticCapabilities) Capabilities(context.Context) ([]Capability, error) { return s, nil }

// Options configures a Controller.
type Options struct {
	// Capabilities supplies the registered capabilities; nil registers none.
	Capabilities CapabilitySource
	// Volumes answers the volume facts the upgrade preflight reads; nil reads
	// the Volume objects the application declares.
	Volumes VolumeSource
	// Clock supplies the round time; defaults to time.Now.
	Clock func() time.Time
}

// Controller reconciles applications: it renders the definition's package into
// the declared namespace and drives the phase of C-33.
type Controller struct {
	store        Store
	capabilities CapabilitySource
	volumes      VolumeSource
	clock        func() time.Time
}

// NewController builds a controller over one store.
func NewController(store Store, options Options) *Controller {
	controller := &Controller{store: store, capabilities: options.Capabilities, volumes: options.Volumes, clock: options.Clock}
	if controller.capabilities == nil {
		controller.capabilities = StaticCapabilities(nil)
	}
	if controller.volumes == nil {
		controller.volumes = storeVolumes{store: store}
	}
	if controller.clock == nil {
		controller.clock = time.Now
	}
	return controller
}

// Reconcile runs one round of one application (C-06).
func (c *Controller) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	if request.Kind != "Application" {
		return reconcile.Result{}, nil
	}
	ref := write.Ref{Group: api.Group, Version: "v1beta1", Kind: "Application", Namespace: request.Namespace, Name: request.Name}
	stored, err := c.store.Get(ctx, ref)
	if apierrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	application := &v1beta1.Application{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, application); err != nil {
		return reconcile.Result{}, fmt.Errorf("decode the application: %w", err)
	}
	now := c.clock()

	action, record, err := c.pendingAction(ctx, application)
	if err != nil {
		return reconcile.Result{}, err
	}
	if application.DeletionTimestamp != nil {
		return c.uninstall(ctx, application, action, record, now)
	}
	if !hasFinalizer(application) {
		finalizers := append(append([]string{}, application.Finalizers...), Finalizer)
		if _, err := c.store.SetFinalizers(ctx, ref, finalizers); err != nil {
			return reconcile.Result{}, err
		}
	}
	if action == ActionRetry {
		return c.retry(ctx, application, record, now)
	}
	if action == ActionConfirm {
		return c.confirm(ctx, application, record, now)
	}
	if action == ActionRestart {
		return c.restart(ctx, application, record, now)
	}
	if record != nil {
		// An orphan without an uninstall has nothing to abandon; drop it so it
		// cannot fire later.
		if err := c.deleteRecord(ctx, record); err != nil {
			return reconcile.Result{}, err
		}
	}
	if application.Status.Phase == PhaseFailed {
		return reconcile.Result{}, nil
	}
	return c.install(ctx, application, now)
}

// install moves one application towards Ready: prerequisites, then render and
// apply, then the phase and retry rules of C-33.
func (c *Controller) install(ctx context.Context, application *v1beta1.Application, now time.Time) (reconcile.Result, error) {
	definition, err := c.definition(ctx, application.Spec.DefinitionRef)
	if err != nil {
		return reconcile.Result{}, err
	}
	if definition == nil {
		return c.block(ctx, application, ConditionDefinitionMissing,
			fmt.Sprintf("definition %q does not exist", application.Spec.DefinitionRef))
	}
	if err := ValidateDefinition(definition.Spec); err != nil {
		return c.fail(ctx, application, ConditionInvalidPackage, err.Error())
	}
	// The package defaults take effect at the first installation and stay frozen
	// afterwards, even if the definition moves (C-32, C-67).
	defaults := resourceDefaults(application, definition)
	registered, err := c.capabilities.Capabilities(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	comparison, err := Compare(definition.Spec.Requires, registered)
	if err != nil {
		return c.fail(ctx, application, ConditionInvalidPackage, err.Error())
	}
	if len(comparison.Missing) > 0 {
		return c.block(ctx, application, ConditionCapabilityMissing,
			"missing required capabilities: "+strings.Join(comparison.Missing, ", "))
	}
	exists, err := c.namespaceExists(ctx, application.Spec.Namespace)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !exists {
		return c.block(ctx, application, ConditionNamespaceMissing,
			fmt.Sprintf("namespace %q does not exist", application.Spec.Namespace))
	}

	// A migration moving one of the volumes the application mounts stops its
	// workloads first: the flow waits for them to exit, and this round holds the
	// phase without writing the spec (C-40).
	if migrationName, err := c.quiesceFor(ctx, application); err != nil {
		return reconcile.Result{}, err
	} else if migrationName != "" {
		return c.quiesce(ctx, application, migrationName, comparison, now)
	}

	config, err := decodeConfig(application.Spec.Config)
	if err != nil {
		return c.fail(ctx, application, ConditionInvalidConfig, err.Error())
	}
	resolved, err := Resolve(definition.Spec.Config, config)
	if err != nil {
		return c.fail(ctx, application, ConditionInvalidConfig, err.Error())
	}

	// The move out of Pending is observable as its own phase before any
	// downstream write happens (C-33, TC-54).
	if application.Status.Phase != PhaseInstalling && application.Status.Phase != PhaseReady && application.Status.Phase != PhaseUpgrading {
		status := baseStatus(application)
		status.Phase = PhaseInstalling
		status.ResourceDefaults = defaults
		resetBudget(&status)
		status.Conditions = state.Set(status.Conditions, state.Blocked(
			application.Generation, "Installing", "the application is being installed"))
		return reconcile.Result{RequeueAfter: time.Second}, c.writeStatus(ctx, application, status)
	}
	if retryAt := application.Status.NextRetryAt; retryAt != nil && now.Before(retryAt.Time) {
		return reconcile.Result{RequeueAfter: retryAt.Sub(now)}, nil
	}

	// A ready application is judged by the baseline it applied, not by a fresh
	// render: a catalogue update alone never changes installed objects, and a
	// spec write is the upgrade request of C-34.
	if application.Status.Phase == PhaseReady || application.Status.Phase == PhaseUpgrading {
		return c.steady(ctx, application, definition, resolved, comparison, now)
	}

	next, err := c.buildTarget(ctx, application, definition, resolved)
	if err != nil {
		return c.fail(ctx, application, ConditionInvalidPackage, err.Error())
	}
	return c.recordBaseline(ctx, application, next, comparison, now)
}

// block writes a blocked state with the reason a prerequisite is missing
// (C-11, C-33). An application that already left Pending keeps its phase and its
// retry budget: C-33 allows no automatic rollback, and only a user retry resets
// the budget.
func (c *Controller) block(ctx context.Context, application *v1beta1.Application, conditionType, message string) (reconcile.Result, error) {
	status := blockedStatus(application, conditionType, message)
	return reconcile.Result{RequeueAfter: pendingResync}, c.writeStatus(ctx, application, status)
}

// blockedStatus keeps the phase and the retry facts of an application that
// already moved past Pending; a first or still-Pending application is Pending.
func blockedStatus(application *v1beta1.Application, conditionType, message string) v1beta1.ApplicationStatus {
	status := baseStatus(application)
	status.Phase = application.Status.Phase
	if application.Status.Phase == "" || application.Status.Phase == PhasePending {
		status.Phase = PhasePending
		status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
	} else {
		// The application already runs; a prerequisite that disappeared does not
		// make its workloads unavailable, so the observed health stays.
		status.Health = application.Status.Health
	}
	status.Conditions = state.Set(status.Conditions, state.Blocked(application.Generation, conditionType, message))
	status.Conditions = withFailure(status.Conditions, application.Generation, conditionType, message)
	return status
}

// fail writes a Failed state that no automatic retry leaves (C-33). The retry
// count stays observable; the schedule ends because nothing runs automatically
// any more. An application that is already Ready keeps its phase and its
// observed health: C-33 has no Ready→Failed rollback, and a re-render that
// failed on a package error did not observe the workloads as unavailable
// (C-08).
func (c *Controller) fail(ctx context.Context, application *v1beta1.Application, conditionType, message string) (reconcile.Result, error) {
	if application.Status.Phase == PhaseReady {
		status := baseStatus(application)
		status.Phase = PhaseReady
		status.Health = application.Status.Health
		status.Conditions = state.Set(status.Conditions, state.Blocked(application.Generation, conditionType, message))
		status.Conditions = withFailure(status.Conditions, application.Generation, conditionType, message)
		return reconcile.Result{RequeueAfter: readyResync}, c.writeStatus(ctx, application, status)
	}
	status := baseStatus(application)
	status.Phase = PhaseFailed
	status.NextRetryAt = nil
	status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
	status.Conditions = state.Set(status.Conditions, state.Blocked(application.Generation, conditionType, message))
	status.Conditions = withFailure(status.Conditions, application.Generation, conditionType, message)
	return reconcile.Result{}, c.writeStatus(ctx, application, status)
}

// recordFailure spends one attempt of the retry budget: within it the
// application stays Installing with an observable count and the next allowed
// time, and the fifth failure ends automatic retries (C-33).
func (c *Controller) recordFailure(ctx context.Context, application *v1beta1.Application, now time.Time, message string) (reconcile.Result, error) {
	if application.Status.Phase == PhaseReady {
		// C-33 has no Ready→Installing rollback: a re-apply that failed on a ready
		// application keeps its phase, and health stays the last observed value
		// because the workloads were not observed as unavailable (C-08). A ready
		// application spends no retry budget, so the failed round waits the same
		// interval a healthy one does instead of a fixed ten seconds (C-33).
		status := baseStatus(application)
		status.Phase = PhaseReady
		status.Health = application.Status.Health
		status.Conditions = state.Set(status.Conditions, state.Blocked(application.Generation, ConditionRetrying, message))
		status.Conditions = withFailure(status.Conditions, application.Generation, ConditionRetrying, message)
		return reconcile.Result{RequeueAfter: readyResync}, c.writeStatus(ctx, application, status)
	}
	attempts := application.Status.RetryCount + 1
	status := baseStatus(application)
	status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
	if attempts >= MaxAttempts {
		status.Phase = PhaseFailed
		status.RetryCount = attempts
		status.NextRetryAt = nil
		status.Conditions = state.Set(status.Conditions, state.Blocked(application.Generation, ConditionFailed, message))
		status.Conditions = withFailure(status.Conditions, application.Generation, ConditionFailed, message)
		return reconcile.Result{}, c.writeStatus(ctx, application, status)
	}
	wait := Backoff(int(attempts))
	next := metav1.NewTime(now.Add(wait))
	status.Phase = PhaseInstalling
	status.RetryCount = attempts
	status.NextRetryAt = &next
	status.Conditions = state.Set(status.Conditions, state.Blocked(application.Generation, ConditionRetrying, message))
	status.Conditions = withFailure(status.Conditions, application.Generation, ConditionRetrying, message)
	return reconcile.Result{RequeueAfter: wait}, c.writeStatus(ctx, application, status)
}

// retry consumes a retry action: the budget resets, the phase returns to
// Installing and no attempt runs in the same round (C-33).
func (c *Controller) retry(ctx context.Context, application *v1beta1.Application, record *unstructured.Unstructured, now time.Time) (reconcile.Result, error) {
	if application.Status.Phase != PhaseFailed && rolledBackReport(application) != nil {
		// A rolled-back upgrade is retried by the user, not by the schedule:
		// clearing the record lets the next round attempt the target again while
		// the application stays Ready (C-34).
		status := baseStatus(application)
		status.Phase = application.Status.Phase
		status.Upgrade = nil
		meta.RemoveStatusCondition(&status.Conditions, ConditionUpgradeFailed)
		status.Conditions = state.Set(status.Conditions, state.Applied(application.Generation, "the upgrade will be attempted again"))
		if err := c.writeStatus(ctx, application, status); err != nil {
			return reconcile.Result{}, err
		}
		if err := c.deleteRecord(ctx, record); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
	status := baseStatus(application)
	resetBudget(&status)
	status.Phase = PhaseInstalling
	status.Conditions = state.Set(status.Conditions, state.Blocked(
		application.Generation, "Installing", "the retry budget was reset"))
	next := metav1.NewTime(now.Add(InitialBackoff))
	status.NextRetryAt = &next
	if err := c.writeStatus(ctx, application, status); err != nil {
		return reconcile.Result{}, err
	}
	if err := c.deleteRecord(ctx, record); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: InitialBackoff}, nil
}

// restart consumes a restart action: the rendered workloads are deleted and
// rebuilt once from the baseline, while the user spec, volumes and routes stay
// untouched (C-66). The round deletes them itself, so their absence is not an
// external write and the drift policies of C-35 do not apply to it: adopt and
// ignore keep external values, and an object that is gone carries none. Rebuilding
// here rather than waiting for the next apply keeps a restart working under every
// policy, including the ones that would otherwise leave the missing objects alone.
// The action never leaves Failed: only a retry does (C-33).
func (c *Controller) restart(ctx context.Context, application *v1beta1.Application, record *unstructured.Unstructured, now time.Time) (reconcile.Result, error) {
	if application.Status.Phase == PhaseFailed {
		return reconcile.Result{}, c.deleteRecord(ctx, record)
	}
	workloads := WorkloadRefs(application.Status.Rendered)
	for _, rendered := range workloads {
		namespace := rendered.Namespace
		if namespace == "" {
			namespace = application.Spec.Namespace
		}
		if err := c.store.Delete(ctx, namespace, referenceOf(rendered)); err != nil && !apierrors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
	}
	if err := c.rebuildWorkloads(ctx, application, workloads); err != nil {
		return reconcile.Result{}, err
	}
	// The record is consumed in the same round: a second round would delete the
	// workloads this one just rebuilt (C-66).
	if err := c.deleteRecord(ctx, record); err != nil {
		return reconcile.Result{}, err
	}
	return c.install(ctx, application, now)
}

// rebuildWorkloads writes the workload objects of the baseline back after a
// restart deleted them (C-66). Only the deleted objects are written, so the rest
// of the set keeps whatever the drift policies of C-35 left in place.
func (c *Controller) rebuildWorkloads(ctx context.Context, application *v1beta1.Application, workloads []v1beta1.ObjectRef) error {
	applied := application.Status.Applied
	if applied == nil || len(workloads) == 0 {
		return nil
	}
	objects, err := convergeApplied(applied)
	if err != nil {
		return err
	}
	deleted := make(map[string]bool, len(workloads))
	for _, ref := range workloads {
		deleted[identityOfRef(ref)] = true
	}
	for _, object := range objects {
		if !deleted[identityOfRef(objectRefOf(object))] {
			continue
		}
		stampOwner(object, string(application.UID))
		if _, err := c.store.Apply(ctx, application.Spec.Namespace, object); err != nil {
			return err
		}
	}
	return nil
}

// resourceDefaults returns the package defaults frozen for this application:
// the set status already carries, or the definition's current values on the
// first installation. An empty set is snapshotted too, so a later definition
// that gains defaults still cannot reach an application that exists (C-32).
func resourceDefaults(application *v1beta1.Application, definition *v1beta1.ApplicationDefinition) *v1beta1.ResourceRequirements {
	if application.Status.ResourceDefaults != nil {
		return application.Status.ResourceDefaults
	}
	if definition.Spec.Resources == nil {
		return &v1beta1.ResourceRequirements{}
	}
	return definition.Spec.Resources.DeepCopy()
}

// frozenDefaults reads the package defaults already frozen for an application;
// an application installed before any defaults existed carries an empty set.
func frozenDefaults(application *v1beta1.Application) *v1beta1.ResourceRequirements {
	if application.Status.ResourceDefaults != nil {
		return application.Status.ResourceDefaults
	}
	return &v1beta1.ResourceRequirements{}
}

// resourceStatus projects the resource facts the workload converges to: the
// values KOS sets, or the unlimited marker with the LimitRange that still
// applies (C-67).
func (c *Controller) resourceStatus(ctx context.Context, application *v1beta1.Application, resources *v1beta1.ResourceRequirements) (*v1beta1.ResourceStatus, error) {
	status := &v1beta1.ResourceStatus{}
	if !ResourcesEmpty(resources) {
		status.Requests = resources.Requests
		status.Limits = resources.Limits
		return status, nil
	}
	status.Unlimited = true
	limitRange, err := c.limitRangeName(ctx, application.Spec.Namespace)
	if err != nil {
		return nil, err
	}
	status.LimitRange = limitRange
	return status, nil
}

// uninstall drives the Uninstalling phase: it deletes the rendered objects,
// keeps the data (C-39), and reports a block that outlasts the stall window. An
// orphan action gives up the blocked objects instead of waiting (C-33).
func (c *Controller) uninstall(ctx context.Context, application *v1beta1.Application, action string, record *unstructured.Unstructured, now time.Time) (reconcile.Result, error) {
	if record != nil && action != ActionOrphan {
		// A retry means nothing once the application is going away; consuming the
		// record keeps the KOS namespace from holding work nobody will read (C-04).
		if err := c.deleteRecord(ctx, record); err != nil {
			return reconcile.Result{}, err
		}
		record = nil
	}
	if action == ActionOrphan {
		if err := c.writeAbandonment(ctx, application); err != nil {
			return reconcile.Result{}, err
		}
		if err := c.clearFinalizer(ctx, application); err != nil {
			return reconcile.Result{}, err
		}
		if err := c.deleteRecord(ctx, record); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}
	blocked := make([]v1beta1.ObjectRef, 0, len(application.Status.Rendered))
	for _, rendered := range application.Status.Rendered {
		ref := referenceOf(rendered)
		// The ref carries the namespace the object actually lives in; the
		// application's declared namespace may have been rewritten since (C-32).
		namespace := ref.Namespace
		if namespace == "" {
			namespace = application.Spec.Namespace
		}
		if err := c.store.Delete(ctx, namespace, ref); err != nil && !apierrors.IsNotFound(err) {
			blocked = appendUniqueRef(blocked, rendered)
			continue
		}
		if _, err := c.store.Get(ctx, ref); err == nil {
			blocked = appendUniqueRef(blocked, rendered)
		} else if !apierrors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
	}
	if len(blocked) == 0 {
		return reconcile.Result{}, c.clearFinalizer(ctx, application)
	}
	blockedSince := now
	if current := application.Status.Uninstall; current != nil && current.BlockedSince != nil {
		blockedSince = current.BlockedSince.Time
	}
	since := metav1.NewTime(blockedSince)
	message := "the uninstall is blocked by " + describe(blocked)
	status := baseStatus(application)
	status.Phase = PhaseUninstalling
	status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
	status.Uninstall = &v1beta1.UninstallStatus{
		Blocked:      blocked,
		BlockedSince: &since,
		Stalled:      now.Sub(blockedSince) > UninstallStallWindow,
	}
	status.Conditions = state.Set(status.Conditions, state.Blocked(application.Generation, ConditionUninstallBlocked, message))
	status.Conditions = withFailure(status.Conditions, application.Generation, ConditionUninstallBlocked, message)
	return reconcile.Result{RequeueAfter: time.Minute}, c.writeStatus(ctx, application, status)
}

// writeAbandonment writes the immutable abandonment manifest of one orphan: the
// blocked objects it gives up and the volumes it keeps (C-33). The application
// reconciler is the writer C-04 allocates; writing once leaves the manifest
// readable after the application object is gone.
func (c *Controller) writeAbandonment(ctx context.Context, application *v1beta1.Application) error {
	name := AbandonmentName(string(application.UID))
	ref := write.Ref{Version: "v1", Kind: "ConfigMap", Namespace: api.SystemNamespace, Name: name}
	if _, err := c.store.Get(ctx, ref); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	entries := make([]Abandonment, 0)
	if application.Status.Uninstall != nil {
		for _, blocked := range application.Status.Uninstall.Blocked {
			entries = append(entries, Abandonment{Object: blocked, Disposition: DispositionAbandoned})
		}
	}
	for _, volume := range application.Spec.Volumes {
		entries = append(entries, Abandonment{
			Object: v1beta1.ObjectRef{
				Group:     api.Group,
				Version:   "v1beta1",
				Kind:      "Volume",
				Namespace: application.Namespace,
				Name:      volume,
			},
			Disposition: DispositionRetained,
		})
	}
	encoded, err := EncodeAbandonment(entries)
	if err != nil {
		return err
	}
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"immutable":  true,
		"metadata": map[string]any{
			"name":      name,
			"namespace": api.SystemNamespace,
			"labels": map[string]any{
				markers.ManagedBy:      write.ManagedByValue,
				write.AbandonmentLabel: string(application.UID),
			},
		},
		"data": map[string]any{AbandonmentKey: encoded},
	}}
	if _, err := c.store.CreateRecord(ctx, document); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// definition reads the definition an application was installed from.
func (c *Controller) definition(ctx context.Context, name string) (*v1beta1.ApplicationDefinition, error) {
	if name == "" {
		return nil, nil
	}
	ref := write.Ref{Group: api.Group, Version: "v1beta1", Kind: "ApplicationDefinition", Name: name}
	stored, err := c.store.Get(ctx, ref)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	definition := &v1beta1.ApplicationDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, definition); err != nil {
		return nil, fmt.Errorf("decode the definition: %w", err)
	}
	return definition, nil
}

// namespaceExists reports whether the declared namespace is present (C-54).
func (c *Controller) namespaceExists(ctx context.Context, namespace string) (bool, error) {
	if namespace == "" {
		return false, nil
	}
	_, err := c.store.Get(ctx, write.Ref{Version: "v1", Kind: "Namespace", Name: namespace})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// pendingAction reads the first unconsumed action record of one application
// (C-04, C-33).
func (c *Controller) pendingAction(ctx context.Context, application *v1beta1.Application) (string, *unstructured.Unstructured, error) {
	if application.UID == "" {
		return "", nil, nil
	}
	records, err := c.store.List(ctx, configMapResource, api.SystemNamespace, ActionOwnerLabel+"="+string(application.UID))
	if apierrors.IsNotFound(err) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	sort.Slice(records, func(left, right int) bool {
		return records[left].GetName() < records[right].GetName()
	})
	prefix := actionRecordPrefix + string(application.UID) + "-"
	target := application.Namespace + "/" + application.Name
	for index := range records {
		record := &records[index]
		// Only a one-shot record of this application is an action: the immutable
		// abandonment manifest shares the owner label but is not consumed, and a
		// record naming another application is left to its own reconciler (C-04).
		if !strings.HasPrefix(record.GetName(), prefix) {
			continue
		}
		action := record.GetLabels()[ActionLabel]
		if action == "" {
			continue
		}
		if named, _, _ := unstructured.NestedString(record.Object, "data", ActionDataKey); named != target {
			continue
		}
		return action, record, nil
	}
	return "", nil, nil
}

// deleteRecord removes a consumed action record (C-04).
func (c *Controller) deleteRecord(ctx context.Context, record *unstructured.Unstructured) error {
	if record == nil {
		return nil
	}
	ref := write.RefOf(record)
	err := c.store.Delete(ctx, api.SystemNamespace, ref)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// clearFinalizer releases the uninstall gate so the API server can remove the
// object (C-33).
func (c *Controller) clearFinalizer(ctx context.Context, application *v1beta1.Application) error {
	ref := write.Ref{Group: api.Group, Version: "v1beta1", Kind: "Application", Namespace: application.Namespace, Name: application.Name}
	remaining := make([]string, 0, len(application.Finalizers))
	for _, finalizer := range application.Finalizers {
		if finalizer != Finalizer {
			remaining = append(remaining, finalizer)
		}
	}
	_, err := c.store.SetFinalizers(ctx, ref, remaining)
	return err
}

// writeStatus writes the status only when it changed, so a repeat observation
// produces no write (C-06).
func (c *Controller) writeStatus(ctx context.Context, application *v1beta1.Application, status v1beta1.ApplicationStatus) error {
	if sameStatus(application.Status, status) {
		return nil
	}
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return fmt.Errorf("encode the application status: %w", err)
	}
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      application.Name,
			"namespace": application.Namespace,
		},
		"status": encoded,
	}}
	_, err = c.store.ApplyStatus(ctx, document)
	return err
}

// sameStatus compares two statuses by their encoding.
func sameStatus(left, right v1beta1.ApplicationStatus) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

// baseStatus carries the status facts a new round starts from: the conditions,
// the retry count and schedule, and the rendered objects an uninstall later
// deletes. Only a user retry or a completed install resets the budget (C-33).
// The quiesce condition is dropped here, so a round that finds no migration
// running clears the stop it recorded before (C-40).
func baseStatus(application *v1beta1.Application) v1beta1.ApplicationStatus {
	conditions := append([]metav1.Condition(nil), application.Status.Conditions...)
	meta.RemoveStatusCondition(&conditions, ConditionQuiesced)
	return v1beta1.ApplicationStatus{
		ObjectStatus: v1beta1.ObjectStatus{
			Conditions: conditions,
			// An install that is not Ready is not available; a status write
			// always carries a concrete value, never an empty health object.
			Health: v1beta1.Health{State: v1beta1.HealthStateUnavailable},
		},
		RetryCount:       application.Status.RetryCount,
		NextRetryAt:      application.Status.NextRetryAt,
		Rendered:         append([]v1beta1.ObjectRef(nil), application.Status.Rendered...),
		Applied:          application.Status.Applied,
		Upgrade:          application.Status.Upgrade,
		Drift:            append([]v1beta1.DriftEntry(nil), application.Status.Drift...),
		Resources:        application.Status.Resources,
		ResourceDefaults: application.Status.ResourceDefaults,
	}
}

// settlePhase keeps the phase an application entered a stop with (C-66): a
// paused application holds its phase while health reports the zero effective
// replicas. A phase that never reached Ready is not preserved.
func settlePhase(application *v1beta1.Application, status *v1beta1.ApplicationStatus) {
	if application.Spec.Paused && (application.Status.Phase == PhaseReady || application.Status.Phase == PhaseUpgrading) {
		status.Phase = application.Status.Phase
	}
}

// resetBudget clears the retry count and schedule of C-33. Only a user retry or
// a completed install may do so.
func resetBudget(status *v1beta1.ApplicationStatus) {
	status.RetryCount = 0
	status.NextRetryAt = nil
}

// clearFailures removes every failure condition of a round that found nothing
// to report; C-07 keeps a condition only while it applies.
func clearFailures(conditions []metav1.Condition) []metav1.Condition {
	for _, other := range failureConditions {
		meta.RemoveStatusCondition(&conditions, other)
	}
	return conditions
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

// applicationHealth derives the availability axis from the stored workloads
// (C-08). A workload whose status the server has not filled in yet is not an
// unready workload: health is Unknown until its status exists. An application
// whose only workloads are Jobs is judged by the Job conditions (C-11). A paused
// application has zero effective desired replicas and is Healthy when observed
// (C-66).
func applicationHealth(objects []*unstructured.Unstructured, comparison CapabilityResult, paused bool) v1beta1.HealthState {
	if jobs := jobObjects(objects); len(jobs) > 0 && !hasReplicaWorkload(objects) {
		jobState := jobHealth(jobs)
		if jobState == v1beta1.HealthStateHealthy && len(comparison.Unsatisfied) > 0 {
			jobState = v1beta1.HealthStateDegraded
		}
		return state.EvaluateHealth(state.HealthInput{IsObserved: true, IsJobOnly: true, JobState: jobState})
	}
	desired, ready, observed := replicaFacts(objects)
	return state.EvaluateHealth(state.HealthInput{
		IsObserved:                observed,
		EffectiveDesiredReplicas:  state.EffectiveDesiredReplicas(desired, paused, false),
		ReadyReplicas:             ready,
		IsStopped:                 paused,
		HasInsufficientCapability: len(comparison.Unsatisfied) > 0,
	})
}

// jobObjects returns the rendered batch Jobs.
func jobObjects(objects []*unstructured.Unstructured) []*unstructured.Unstructured {
	var jobs []*unstructured.Unstructured
	for _, object := range objects {
		kind := object.GroupVersionKind()
		if kind.Group == "batch" && kind.Kind == "Job" {
			jobs = append(jobs, object)
		}
	}
	return jobs
}

// hasReplicaWorkload reports whether any rendered workload is judged by replica
// counts, in which case the application is not a job-only one (C-08, C-11).
func hasReplicaWorkload(objects []*unstructured.Unstructured) bool {
	for _, object := range objects {
		switch object.GetKind() {
		case "Deployment", "StatefulSet", "ReplicaSet", "DaemonSet":
			return true
		}
	}
	return false
}

// jobHealth reads a job-only application's health from the Job conditions: a
// failed Job is unavailable, every Job complete is healthy, anything still
// running is degraded, and a Job the server has not observed yet is unknown.
func jobHealth(jobs []*unstructured.Unstructured) v1beta1.HealthState {
	complete := true
	for _, job := range jobs {
		conditions, found, _ := unstructured.NestedSlice(job.Object, "status", "conditions")
		if !found || len(conditions) == 0 {
			return v1beta1.HealthStateUnknown
		}
		jobComplete := false
		for _, raw := range conditions {
			condition, _ := raw.(map[string]any)
			conditionType, _ := condition["type"].(string)
			status, _ := condition["status"].(string)
			if status != "True" {
				continue
			}
			switch conditionType {
			case "Failed":
				return v1beta1.HealthStateUnavailable
			case "Complete":
				jobComplete = true
			}
		}
		if !jobComplete {
			complete = false
		}
	}
	if complete {
		return v1beta1.HealthStateHealthy
	}
	return v1beta1.HealthStateDegraded
}

// replicaFacts sums the desired and ready replicas of the stored workloads and
// reports whether every workload has a status to read them from.
func replicaFacts(objects []*unstructured.Unstructured) (int32, int32, bool) {
	var desired, ready int32
	observed := true
	for _, object := range objects {
		switch object.GetKind() {
		case "Deployment", "StatefulSet", "ReplicaSet":
			if !hasStatus(object) {
				observed = false
			}
			replicas, found, _ := unstructured.NestedInt64(object.Object, "spec", "replicas")
			if !found {
				replicas = 1
			}
			readyReplicas, _, _ := unstructured.NestedInt64(object.Object, "status", "readyReplicas")
			desired += int32(replicas)
			ready += int32(readyReplicas)
		case "DaemonSet":
			if !hasStatus(object) {
				observed = false
			}
			scheduled, _, _ := unstructured.NestedInt64(object.Object, "status", "desiredNumberScheduled")
			readyCount, _, _ := unstructured.NestedInt64(object.Object, "status", "numberReady")
			desired += int32(scheduled)
			ready += int32(readyCount)
		}
	}
	return desired, ready, observed
}

// hasStatus reports whether the server has written a status for the object.
func hasStatus(object *unstructured.Unstructured) bool {
	status, found, _ := unstructured.NestedMap(object.Object, "status")
	return found && len(status) > 0
}

// permanent reports a write the same package cannot make succeed by retrying
// (C-33): a refused write or a request the API server rejects as malformed.
func permanent(err error) bool {
	var denied *write.Denied
	if errors.As(err, &denied) {
		return true
	}
	var unknown *write.UnknownResource
	if errors.As(err, &unknown) {
		return true
	}
	// A 404 on an apply means the served resource or namespace is gone; the same
	// package cannot make it succeed by retrying (C-33).
	return apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsForbidden(err) || apierrors.IsNotFound(err)
}

// decodeConfig reads the submitted config values of C-32.
func decodeConfig(config runtime.RawExtension) (map[string]any, error) {
	if len(config.Raw) == 0 {
		return map[string]any{}, nil
	}
	var values map[string]any
	if err := json.Unmarshal(config.Raw, &values); err != nil {
		return nil, fmt.Errorf("the config is not an object: %w", err)
	}
	if values == nil {
		values = map[string]any{}
	}
	return values, nil
}

// claimNames resolves the claim each declared volume is bound to (C-40): the
// binding a migration wrote into the volume status, or the volume's own name
// before any migration moved it. The render follows the binding, so a cutover
// re-renders the workloads onto the new claim without changing the application
// (C-32, C-38).
func (c *Controller) claimNames(ctx context.Context, application *v1beta1.Application) (map[string]string, error) {
	claims := make(map[string]string, len(application.Spec.Volumes))
	for _, name := range application.Spec.Volumes {
		claims[name] = name
		volume, err := c.store.Get(ctx, write.Ref{
			Group: api.Group, Version: "v1beta1", Kind: "Volume",
			Namespace: application.Spec.Namespace, Name: name,
		})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		bound, _, err := unstructured.NestedString(volume.Object, "status", "boundClaim")
		if err != nil {
			return nil, err
		}
		if bound != "" {
			claims[name] = bound
		}
	}
	return claims, nil
}

// renderInputFor is what a translation template reads for one application
// (C-32), with the binding a migration may have written (C-40).
func (c *Controller) renderInputFor(ctx context.Context, application *v1beta1.Application, config map[string]any) (RenderInput, error) {
	claims, err := c.claimNames(ctx, application)
	if err != nil {
		return RenderInput{}, err
	}
	return renderInput(application, config, claims), nil
}

// stampOwner marks a rendered object with the application's UID (C-02).
func stampOwner(object *unstructured.Unstructured, uid string) {
	if uid == "" {
		return
	}
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[markers.OwnerUID] = uid
	object.SetLabels(labels)
}

// objectRefOf reads the identity of an object into a status reference.
func objectRefOf(object *unstructured.Unstructured) v1beta1.ObjectRef {
	kind := object.GroupVersionKind()
	return v1beta1.ObjectRef{
		Group:     kind.Group,
		Version:   kind.Version,
		Kind:      kind.Kind,
		Namespace: object.GetNamespace(),
		Name:      object.GetName(),
	}
}

// referenceOf turns a status reference into a write reference.
func referenceOf(ref v1beta1.ObjectRef) write.Ref {
	return write.Ref{Group: ref.Group, Version: ref.Version, Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name}
}

// appendUniqueRef appends a reference unless it is already listed.
func appendUniqueRef(items []v1beta1.ObjectRef, item v1beta1.ObjectRef) []v1beta1.ObjectRef {
	for _, existing := range items {
		if reflect.DeepEqual(existing, item) {
			return items
		}
	}
	return append(items, item)
}

// describe renders blocked objects for a condition message.
func describe(objects []v1beta1.ObjectRef) string {
	names := make([]string, 0, len(objects))
	for _, object := range objects {
		name := object.Kind + " " + object.Name
		if object.Namespace != "" {
			name = object.Kind + " " + object.Namespace + "/" + object.Name
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// hasFinalizer reports whether the application carries the uninstall gate.
func hasFinalizer(application *v1beta1.Application) bool {
	for _, finalizer := range application.Finalizers {
		if finalizer == Finalizer {
			return true
		}
	}
	return false
}

// configMapResource resolves the action record carrier.
var configMapResource = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
