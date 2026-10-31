package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
)

// Conditions and phases the backup reconciler writes (C-07, C-41, C-42). A
// failure keeps its own condition; it never stands in for Reconciled.
const (
	ConditionInvalidSpec          = "InvalidSpec"
	ConditionNoCoveredObjects     = "NoCoveredObjects"
	ConditionSameFailureDomain    = "SameFailureDomain"
	ConditionFailureDomainUnknown = "FailureDomainUnknown"
	ConditionQuiesceRequired      = "QuiesceRequired"
	ConditionBackupFailed         = "BackupFailed"
	ConditionDrillMismatch        = "DrillMismatch"
	ConditionScheduleJobFailed    = "ScheduleJobFailed"

	PhasePending   = "Pending"
	PhaseRunning   = "Running"
	PhaseScheduled = "Scheduled"
	PhaseFailed    = "Failed"
)

// BackupHook is the package hook an application-hook backup runs before the
// copy; a non-zero exit fails the whole backup (C-32, C-41).
const BackupHook = "backup"

// PolicyLabel marks the schedule jobs of one policy, so a failed trigger is
// observable instead of the policy reporting itself healthy (C-04, C-41).
const PolicyLabel = "kos.tsic.top/backup-policy"

// Resync interval of a policy that is blocked on something cheaper to recheck
// than a run; a scheduled policy waits for its own next instant (C-06).
const pendingResync = 30 * time.Second

// Resources the reconciler reads and the schedule job it writes.
var (
	applicationResource = schema.GroupVersionResource{Group: api.Group, Version: "v1beta1", Resource: "applications"}
	volumeResource      = schema.GroupVersionResource{Group: api.Group, Version: "v1beta1", Resource: "volumes"}
	definitionResource  = schema.GroupVersionResource{Group: api.Group, Version: "v1beta1", Resource: "applicationdefinitions"}
	claimResource       = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	persistentResource  = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
	cronJobResource     = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}
	jobResource         = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
)

// hostnameLabel is the node affinity key a node-pinned claim resolves its node
// from (C-38).
const hostnameLabel = "kubernetes.io/hostname"

// Options configures a Controller.
type Options struct {
	// Clock supplies the round time; defaults to time.Now.
	Clock func() time.Time
	// DefaultTarget is the local disk the deployment reserves for backups; a
	// policy that leaves target.path empty writes there (C-41).
	DefaultTarget string
	// WorkerImage is the digest-pinned image of the backup component the
	// schedule job runs; a policy is blocked without one (C-17, C-41).
	WorkerImage string
	// Owner is the global Owner's username; the restore preflight uses it to
	// exempt the Owner from the cross-owner rule, the same exemption pkg/access
	// applies to the Owner's writes (C-59, C-60).
	Owner string
}

// Controller reconciles backup policies: it validates the policy, refuses a
// target in the source's failure domain, materializes the schedule CronJob, and
// hands each capture and drill to a Worker, recording what the worker reports
// (C-41, C-42). No round copies data or computes a checksum.
type Controller struct {
	store         Store
	worker        Worker
	clock         func() time.Time
	defaultTarget string
	image         string

	mutex     sync.Mutex
	submitted map[string]bool
}

// NewController builds a controller over one store and one worker.
func NewController(store Store, worker Worker, options Options) *Controller {
	controller := &Controller{
		store: store, worker: worker, clock: options.Clock,
		defaultTarget: options.DefaultTarget, image: options.WorkerImage,
		submitted: make(map[string]bool),
	}
	if controller.clock == nil {
		controller.clock = time.Now
	}
	return controller
}

// Reconcile runs one round of one backup policy (C-06).
func (c *Controller) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	if request.Kind != "BackupPolicy" {
		return reconcile.Result{}, nil
	}
	stored, err := c.store.Get(ctx, write.Ref{Group: api.Group, Version: "v1beta1", Kind: "BackupPolicy", Namespace: request.Namespace, Name: request.Name})
	if apierrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	policy := &v1beta1.BackupPolicy{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, policy); err != nil {
		return reconcile.Result{}, fmt.Errorf("decode the backup policy: %w", err)
	}
	if policy.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}
	if problem := validateWorkerImage(c.image); problem != "" {
		return c.block(ctx, policy, ConditionInvalidSpec, problem)
	}
	schedule, selector, problem := validatePolicy(policy.Spec, c.defaultTarget, c.clock())
	if problem != "" {
		return c.block(ctx, policy, ConditionInvalidSpec, problem)
	}
	applications, volumes, err := c.covered(ctx, policy, selector)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(volumes) == 0 {
		return c.block(ctx, policy, ConditionNoCoveredObjects, "spec.selector 没有覆盖任何卷，备份范围为空")
	}
	domains, hasUnknown, err := c.sourceDomains(ctx, volumes)
	if err != nil {
		return reconcile.Result{}, err
	}
	if policy.Spec.Target.FailureDomain != "" && domains[strings.TrimSpace(policy.Spec.Target.FailureDomain)] {
		return c.block(ctx, policy, ConditionSameFailureDomain, fmt.Sprintf(
			"备份目标与源卷处于同一失败域 %q，拒绝备份：同失败域的副本是快照，不是备份", policy.Spec.Target.FailureDomain))
	}
	if policy.Spec.Consistency == v1beta1.BackupConsistencyQuiesced {
		for _, application := range applications {
			if !application.Spec.Paused {
				return c.block(ctx, policy, ConditionQuiesceRequired, fmt.Sprintf("静默一致备份要求应用 %q 停机", application.Name))
			}
		}
	}
	if err := c.ensureSchedule(ctx, policy); err != nil {
		return reconcile.Result{}, err
	}
	status := baseStatus(policy)
	if hasUnknown || strings.TrimSpace(policy.Spec.Target.FailureDomain) == "" {
		message := "源卷的失败域无法判定：允许备份并持续提示"
		if strings.TrimSpace(policy.Spec.Target.FailureDomain) == "" {
			message = "目标没有声明失败域：无法证明它与源卷不同，允许备份并持续提示"
		}
		status.Conditions = state.Set(status.Conditions, state.Condition(ConditionFailureDomainUnknown,
			metav1.ConditionTrue, policy.Generation, "Unknown", message))
	}
	failure, err := failedScheduleJob(ctx, c.store, policy)
	if err != nil {
		return reconcile.Result{}, err
	}
	if failure != "" {
		status.Conditions = state.Set(status.Conditions, state.Condition(ConditionScheduleJobFailed,
			metav1.ConditionTrue, policy.Generation, ConditionScheduleJobFailed, failure))
	}
	c.observeResults(policy, &status, applications, volumes)
	if status.Running == nil {
		scheduled, err := c.dueRun(ctx, policy, &status)
		if err != nil {
			return reconcile.Result{}, err
		}
		if scheduled != nil {
			if err := c.startCapture(ctx, policy, &status, applications, volumes, *scheduled); err != nil {
				return reconcile.Result{}, err
			}
		}
	}
	if status.Running == nil {
		if err := c.startDrill(policy, &status); err != nil {
			return reconcile.Result{}, err
		}
	}
	if status.Running != nil {
		if err := c.ensureSubmitted(policy, &status, volumes); err != nil {
			return reconcile.Result{}, err
		}
	}
	c.settleStatus(policy, schedule, &status)
	if err := c.writeStatus(ctx, policy, status); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: c.nextRequeue(&status)}, nil
}

// block writes a policy whose spec or preflight keeps it from running, with the
// reason that has to be fixed (C-07). A blocked policy materializes no schedule
// and keeps no next run, so nothing runs or claims to run while it stands.
func (c *Controller) block(ctx context.Context, policy *v1beta1.BackupPolicy, conditionType, message string) (reconcile.Result, error) {
	if err := c.removeSchedule(ctx, policy); err != nil {
		return reconcile.Result{}, err
	}
	status := baseStatus(policy)
	status.Phase = PhasePending
	status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
	status.NextRunTime = nil
	status.Running = nil
	status.Conditions = state.Set(status.Conditions, state.Blocked(policy.Generation, conditionType, message))
	if err := c.writeStatus(ctx, policy, status); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: pendingResync}, nil
}

// settleStatus derives the phase, the health and the failure conditions from
// the in-flight action, the last run and its drill (C-07, C-08, C-41, C-42).
func (c *Controller) settleStatus(policy *v1beta1.BackupPolicy, schedule Schedule, status *v1beta1.BackupPolicyStatus) {
	status.Phase = PhaseScheduled
	status.Health = v1beta1.Health{State: v1beta1.HealthStateHealthy}
	if status.LastBackup != nil && !status.LastBackup.Succeeded {
		status.Phase = PhaseFailed
		status.Health = v1beta1.Health{State: v1beta1.HealthStateDegraded}
		conditionType, reason := ConditionBackupFailed, ConditionBackupFailed
		if status.LastDrill != nil && status.LastDrill.Run == status.LastBackup.Path && status.LastDrill.Phase == v1beta1.DrillPhaseFailed {
			conditionType, reason = ConditionDrillMismatch, ConditionDrillMismatch
		}
		status.Conditions = state.Set(status.Conditions, state.Failure(conditionType, policy.Generation, reason, status.LastBackup.Reason))
	}
	if status.Running != nil {
		status.Phase = PhaseRunning
	}
	if next := schedule.Next(c.clock()); !next.IsZero() {
		instant := metav1.NewTime(next)
		status.NextRunTime = &instant
	} else {
		status.NextRunTime = nil
	}
	message := "策略已生效"
	if status.NextRunTime != nil {
		message = "策略已生效，下次计划 " + status.NextRunTime.UTC().Format(time.RFC3339)
	}
	status.Conditions = state.Set(status.Conditions, state.Applied(policy.Generation, message))
}

// nextRequeue waits for the next scheduled instant, bounded so a settled policy
// is re-observed even if its schedule is far away. A handed-off action is
// re-observed sooner, because the round only learns its outcome by polling the
// worker (C-06).
func (c *Controller) nextRequeue(status *v1beta1.BackupPolicyStatus) time.Duration {
	if status.Running != nil {
		return time.Second
	}
	if status.NextRunTime == nil {
		return pendingResync
	}
	wait := status.NextRunTime.Sub(c.clock())
	if wait < time.Second {
		return time.Second
	}
	if wait > time.Hour {
		return time.Hour
	}
	return wait
}

// covered lists the applications and volumes the selector covers. The selector
// is the only binding path (C-41): an application's volume references never
// widen the range, and the ownership rule of C-60 keeps another account's
// objects out, whatever the selector matches.
func (c *Controller) covered(ctx context.Context, policy *v1beta1.BackupPolicy, selector labels.Selector) ([]v1beta1.Application, []v1beta1.Volume, error) {
	items, err := c.store.List(ctx, applicationResource, policy.Namespace, selector.String())
	if err != nil {
		return nil, nil, err
	}
	var applications []v1beta1.Application
	for _, item := range items {
		application := v1beta1.Application{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &application); err != nil {
			return nil, nil, fmt.Errorf("decode an application: %w", err)
		}
		if application.Spec.Owner != policy.Spec.Owner {
			continue
		}
		applications = append(applications, application)
	}
	items, err = c.store.List(ctx, volumeResource, policy.Namespace, selector.String())
	if err != nil {
		return nil, nil, err
	}
	var volumes []v1beta1.Volume
	for _, item := range items {
		volume := v1beta1.Volume{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &volume); err != nil {
			return nil, nil, fmt.Errorf("decode a volume: %w", err)
		}
		if volume.Spec.Owner != policy.Spec.Owner {
			continue
		}
		volumes = append(volumes, volume)
	}
	sort.Slice(applications, func(left, right int) bool { return applications[left].Name < applications[right].Name })
	sort.Slice(volumes, func(left, right int) bool { return volumes[left].Name < volumes[right].Name })
	return applications, volumes, nil
}

// sourceDomains reads the failure domain every covered volume's data lives in;
// hasUnknown reports a volume whose domain cannot be determined (C-41).
func (c *Controller) sourceDomains(ctx context.Context, volumes []v1beta1.Volume) (map[string]bool, bool, error) {
	domains := make(map[string]bool, len(volumes))
	hasUnknown := false
	for _, volume := range volumes {
		domain, known, err := c.failureDomainOf(ctx, volume)
		if err != nil {
			return nil, false, err
		}
		if !known {
			hasUnknown = true
			continue
		}
		domains[domain] = true
	}
	return domains, hasUnknown, nil
}

// failureDomainOf resolves one volume's failure domain: the node a pinned claim
// is bound to, or the storage provider that holds it (C-41).
func (c *Controller) failureDomainOf(ctx context.Context, volume v1beta1.Volume) (string, bool, error) {
	if volume.Status.Node != "" {
		return "node:" + volume.Status.Node, true, nil
	}
	claimName := volume.Status.BoundClaim
	if claimName == "" {
		claimName = volume.Name
	}
	claim, err := c.store.GetResource(ctx, claimResource, volume.Namespace, claimName)
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	pvName, _, err := unstructured.NestedString(claim.Object, "spec", "volumeName")
	if err != nil {
		return "", false, err
	}
	if pvName == "" {
		return "", false, nil
	}
	persistent, err := c.store.GetResource(ctx, persistentResource, "", pvName)
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if node := nodeAffinityOf(persistent); node != "" {
		return "node:" + node, true, nil
	}
	driver, _, err := unstructured.NestedString(persistent.Object, "spec", "csi", "driver")
	if err != nil {
		return "", false, err
	}
	if driver != "" {
		return "storage:" + driver, true, nil
	}
	return "", false, nil
}

// nodeAffinityOf reads the node a persistent volume is pinned to, if any.
func nodeAffinityOf(persistent *unstructured.Unstructured) string {
	terms, found, err := unstructured.NestedSlice(persistent.Object, "spec", "nodeAffinity", "required", "nodeSelectorTerms")
	if err != nil || !found {
		return ""
	}
	for _, term := range terms {
		fields, ok := term.(map[string]any)
		if !ok {
			continue
		}
		expressions, ok := fields["matchExpressions"].([]any)
		if !ok {
			continue
		}
		for _, expression := range expressions {
			parts, ok := expression.(map[string]any)
			if !ok || parts["key"] != hostnameLabel || parts["operator"] != "In" {
				continue
			}
			values, ok := parts["values"].([]any)
			if !ok || len(values) == 0 {
				continue
			}
			if node, ok := values[0].(string); ok {
				return node
			}
		}
	}
	return ""
}

// dueRun reports the scheduled instant a run still answers. The CronJob is the
// schedule trigger (C-04): the cluster's cron controller writes lastScheduleTime
// when it starts a job, and a newly observed instant is the run this round owes.
func (c *Controller) dueRun(ctx context.Context, policy *v1beta1.BackupPolicy, status *v1beta1.BackupPolicyStatus) (*time.Time, error) {
	stored, err := c.store.GetResource(ctx, cronJobResource, policy.Namespace, policy.Name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cronJob := &batchv1.CronJob{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, cronJob); err != nil {
		return nil, fmt.Errorf("decode the schedule CronJob: %w", err)
	}
	if cronJob.Status.LastScheduleTime == nil {
		return nil, nil
	}
	scheduled := cronJob.Status.LastScheduleTime.Time
	if status.LastBackup != nil && status.LastBackup.ScheduledAt != nil && !scheduled.After(status.LastBackup.ScheduledAt.Time) {
		return nil, nil
	}
	return &scheduled, nil
}

// scheduleLabelValue derives the label value that ties schedule jobs to one
// policy (C-41). A policy name is a DNS subdomain and may exceed the 63-character
// label bound, so a long name keeps a readable prefix plus a digest of the whole
// name; the CronJob label and the failure selector both use this function.
func scheduleLabelValue(name string) string {
	const limit = 63
	if len(name) <= limit {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:8])
	prefix := strings.TrimRight(name[:limit-len(suffix)], "-._")
	if prefix == "" {
		prefix = "policy"
	}
	return prefix + suffix
}

// failedScheduleJob reports the newest schedule job of the policy when it
// failed, so a trigger that never ran is not reported as a healthy policy
// (C-04, C-41).
func failedScheduleJob(ctx context.Context, store Store, policy *v1beta1.BackupPolicy) (string, error) {
	items, err := store.List(ctx, jobResource, policy.Namespace, PolicyLabel+"="+scheduleLabelValue(policy.Name))
	if err != nil {
		return "", err
	}
	var newest *batchv1.Job
	for index := range items {
		job := &batchv1.Job{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(items[index].Object, job); err != nil {
			continue
		}
		if newest == nil || job.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = job
		}
	}
	if newest == nil {
		return "", nil
	}
	for _, condition := range newest.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return fmt.Sprintf("计划作业 %q 失败：%s", newest.Name, condition.Message), nil
		}
	}
	if newest.Status.Failed > 0 {
		return fmt.Sprintf("计划作业 %q 有 %d 次失败", newest.Name, newest.Status.Failed), nil
	}
	return "", nil
}

// startCapture hands one run to the worker and records it as in flight. A hook
// that fails settles the run immediately, without a fallback to a weaker
// consistency level (C-41).
func (c *Controller) startCapture(ctx context.Context, policy *v1beta1.BackupPolicy, status *v1beta1.BackupPolicyStatus, applications []v1beta1.Application, volumes []v1beta1.Volume, scheduled time.Time) error {
	run := runDirectory(c.targetPath(policy), policy.Namespace, policy.Name, scheduled)
	if policy.Spec.Consistency == v1beta1.BackupConsistencyHook {
		failure, err := c.runHooks(ctx, applications)
		if err != nil {
			return err
		}
		if failure != "" {
			status.LastBackup = c.failRecord(run, policy.Spec.Consistency, scheduled, failure)
			return nil
		}
	}
	scheduledAt := metav1.NewTime(scheduled)
	status.Running = c.begin(TaskCapture, run, taskScope(volumeNames(volumes)))
	status.Running.ScheduledAt = &scheduledAt
	return nil
}

// startDrill hands the next drill step to the worker: Restoring copies the run
// into the drill destination, Verifying measures the copy against the recorded
// baseline (C-42).
func (c *Controller) startDrill(policy *v1beta1.BackupPolicy, status *v1beta1.BackupPolicyStatus) error {
	if !policy.Spec.Drill.Enabled {
		return nil
	}
	last := status.LastBackup
	if last == nil || !last.Succeeded || last.Baseline == nil {
		return nil
	}
	drill := status.LastDrill
	if drill == nil || drill.Run != last.Path {
		status.LastDrill = &v1beta1.DrillRecord{Phase: v1beta1.DrillPhaseRestoring, Run: last.Path}
		drill = status.LastDrill
	}
	if drill.Phase != v1beta1.DrillPhaseRestoring && drill.Phase != v1beta1.DrillPhaseVerifying {
		return nil
	}
	manifest, err := ReadManifest(drill.Run)
	if err != nil {
		return c.failDrill(drill, status.LastBackup, "读取备份清单失败："+err.Error())
	}
	scope := taskScope(snapshotNames(manifest.Volumes))
	switch drill.Phase {
	case v1beta1.DrillPhaseRestoring:
		status.Running = c.begin(TaskCopy, drill.Run, scope)
	case v1beta1.DrillPhaseVerifying:
		status.Running = c.begin(TaskMeasure, drill.Run, scope)
	}
	return nil
}

// begin records one action as in flight together with the identity a result
// pairs on: an ordinal unique on the object and the volumes it touches (C-06).
func (c *Controller) begin(kind, run, scope string) *v1beta1.Running {
	now := c.clock()
	startedAt := metav1.NewTime(now)
	return &v1beta1.Running{Kind: kind, Run: run, Scope: scope, Sequence: now.UnixNano(), StartedAt: &startedAt}
}

// taskScope is the stable name set one action reads or writes (C-06).
func taskScope(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// volumeNames lists the covered volume object names.
func volumeNames(volumes []v1beta1.Volume) []string {
	names := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		names = append(names, volume.Name)
	}
	return names
}

// snapshotNames lists the manifest volume names.
func snapshotNames(volumes []Snapshot) []string {
	names := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		names = append(names, volume.Name)
	}
	return names
}

// ensureSubmitted submits the recorded in-flight action exactly once per
// controller lifetime, so a round that merely observes does not enqueue the
// same copy again (C-06).
func (c *Controller) ensureSubmitted(policy *v1beta1.BackupPolicy, status *v1beta1.BackupPolicyStatus, volumes []v1beta1.Volume) error {
	running := status.Running
	key := actionIdentity(string(policy.UID), running.Sequence, running.Scope, running.Kind, running.Run)
	if c.isSubmitted(key) {
		return nil
	}
	task, err := c.taskFor(string(policy.UID), *running, volumes)
	if err != nil {
		c.settleTaskFailure(status, *running, err)
		return nil
	}
	c.worker.Submit(task)
	c.markSubmitted(key)
	return nil
}

// taskFor builds the task one recorded action names, carrying the identity the
// result pairs on (C-06).
func (c *Controller) taskFor(owner string, running v1beta1.Running, volumes []v1beta1.Volume) (Task, error) {
	task := Task{Owner: owner, Sequence: running.Sequence, Scope: running.Scope, Kind: running.Kind, Run: running.Run}
	switch running.Kind {
	case TaskCapture:
		sources := make([]Source, 0, len(volumes))
		for _, volume := range volumes {
			sources = append(sources, Source{Namespace: volume.Namespace, Volume: volume.Name})
		}
		task.Sources = sources
		return task, nil
	case TaskCopy, TaskMeasure:
		manifest, err := ReadManifest(running.Run)
		if err != nil {
			return Task{}, err
		}
		if running.Kind == TaskCopy {
			copies := make([]Copy, 0, len(manifest.Volumes))
			for _, volume := range manifest.Volumes {
				copies = append(copies, Copy{Recorded: volume.Name, Destination: filepath.Join(running.Run, DrillDir, volume.Name)})
			}
			task.Copies = copies
			return task, nil
		}
		directories := make([]VolumeDir, 0, len(manifest.Volumes))
		for _, volume := range manifest.Volumes {
			directories = append(directories, VolumeDir{Volume: volume.Name, Path: filepath.Join(running.Run, DrillDir, volume.Name)})
		}
		task.Directories = directories
		return task, nil
	default:
		return Task{}, fmt.Errorf("unknown task kind %q", running.Kind)
	}
}

// settleTaskFailure records an action that could not even be handed off, so a
// broken run or drill never stays in flight forever (C-42).
func (c *Controller) settleTaskFailure(status *v1beta1.BackupPolicyStatus, running v1beta1.Running, failure error) {
	reason := "备份组件未能接受任务：" + failure.Error()
	if running.Kind == TaskCapture {
		scheduled := c.clock()
		if running.ScheduledAt != nil {
			scheduled = running.ScheduledAt.Time
		}
		record := c.failRecord(running.Run, "", scheduled, reason)
		if running.StartedAt != nil {
			record.StartedAt = running.StartedAt
		}
		status.LastBackup = record
	} else if status.LastDrill != nil {
		_ = c.failDrill(status.LastDrill, status.LastBackup, reason)
	}
	status.Running = nil
}

// observeResults applies every outcome the worker has reported (C-06). A result
// that does not match the recorded in-flight action is stale and dropped.
func (c *Controller) observeResults(policy *v1beta1.BackupPolicy, status *v1beta1.BackupPolicyStatus, applications []v1beta1.Application, volumes []v1beta1.Volume) {
	for _, result := range c.worker.Completed(string(policy.UID)) {
		c.forget(result.Task)
		running := status.Running
		if running == nil || actionIdentity(string(policy.UID), running.Sequence, running.Scope, running.Kind, running.Run) != result.Task.identity() {
			continue
		}
		switch result.Task.Kind {
		case TaskCapture:
			status.LastBackup = c.completeCapture(policy, *running, result, applications, volumes)
			if status.LastBackup.Succeeded {
				if err := pruneRuns(c.targetPath(policy), policy.Namespace, policy.Name, policy.Spec.Retention.Keep); err != nil {
					status.LastBackup.Succeeded = false
					status.LastBackup.Reason = "清理超出保留数的旧运行失败：" + err.Error()
				}
			}
		case TaskCopy:
			if result.Err != nil {
				_ = c.failDrill(status.LastDrill, status.LastBackup, "复制演练数据失败："+result.Err.Error())
			} else if status.LastDrill != nil {
				status.LastDrill.Phase = v1beta1.DrillPhaseVerifying
			}
		case TaskMeasure:
			manifest, err := ReadManifest(running.Run)
			if err != nil {
				_ = c.failDrill(status.LastDrill, status.LastBackup, "读取备份清单失败："+err.Error())
				break
			}
			if result.Err != nil {
				_ = c.failDrill(status.LastDrill, status.LastBackup, "无法测量演练数据："+result.Err.Error())
				break
			}
			diffs := Compare(manifest.Baseline, result.Baseline)
			if len(diffs) > 0 {
				if status.LastDrill != nil {
					status.LastDrill.Diffs = diffs
				}
				_ = c.failDrill(status.LastDrill, status.LastBackup, "演练与基线不一致："+DescribeDiffs(diffs))
				break
			}
			completedAt := metav1.NewTime(c.clock())
			if status.LastDrill != nil {
				status.LastDrill.Phase = v1beta1.DrillPhaseSucceeded
				status.LastDrill.Succeeded = true
				status.LastDrill.CompletedAt = &completedAt
			}
		}
		status.Running = nil
	}
}

// completeCapture turns one finished capture into the run record and writes the
// manifest a restore reads (C-41, C-42).
func (c *Controller) completeCapture(policy *v1beta1.BackupPolicy, running v1beta1.Running, result Result, applications []v1beta1.Application, volumes []v1beta1.Volume) *v1beta1.BackupRecord {
	completedAt := metav1.NewTime(c.clock())
	record := &v1beta1.BackupRecord{
		ScheduledAt: running.ScheduledAt,
		StartedAt:   running.StartedAt,
		CompletedAt: &completedAt,
		Consistency: policy.Spec.Consistency,
		Path:        running.Run,
	}
	if result.Err != nil {
		record.Reason = "复制卷数据失败：" + result.Err.Error()
		return record
	}
	baseline := result.Baseline
	manifest := Manifest{
		Policy: policy.Name, Namespace: policy.Namespace, CreatedAt: c.clock(),
		Consistency: policy.Spec.Consistency, Baseline: baseline,
	}
	for _, application := range applications {
		spec, err := specOf(&application)
		if err != nil {
			record.Reason = "编码应用快照失败：" + err.Error()
			return record
		}
		manifest.Applications = append(manifest.Applications, Snapshot{Name: application.Name, Spec: spec})
	}
	for _, volume := range volumes {
		spec, err := specOf(&volume)
		if err != nil {
			record.Reason = "编码卷快照失败：" + err.Error()
			return record
		}
		manifest.Volumes = append(manifest.Volumes, Snapshot{Name: volume.Name, Spec: spec})
	}
	if err := WriteManifest(running.Run, manifest); err != nil {
		record.Reason = "写入备份清单失败：" + err.Error()
		return record
	}
	record.Baseline = &baseline
	record.Succeeded = true
	return record
}

// runHooks runs the backup hook of every covered application. A missing hook or
// a non-zero exit fails the whole run: the policy never silently falls back to
// a weaker consistency level (C-41).
func (c *Controller) runHooks(ctx context.Context, applications []v1beta1.Application) (string, error) {
	for _, application := range applications {
		definition, err := c.definitionOf(ctx, application.Spec.DefinitionRef)
		if err != nil {
			return "", err
		}
		if definition == nil {
			return fmt.Sprintf("应用 %q 引用的定义 %q 不存在", application.Name, application.Spec.DefinitionRef), nil
		}
		hook, found := backupHook(definition.Spec.Hooks)
		if !found {
			return fmt.Sprintf("应用 %q 未声明备份钩子，应用钩子一致性要求钩子存在", application.Name), nil
		}
		if err := app.RunHook(ctx, hook); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return fmt.Sprintf("应用 %q 的备份钩子失败：%v", application.Name, err), nil
		}
	}
	return "", nil
}

// definitionOf reads one application definition; a missing definition is nil.
func (c *Controller) definitionOf(ctx context.Context, name string) (*v1beta1.ApplicationDefinition, error) {
	if name == "" {
		return nil, nil
	}
	stored, err := c.store.GetResource(ctx, definitionResource, "", name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	definition := &v1beta1.ApplicationDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, definition); err != nil {
		return nil, fmt.Errorf("decode the application definition: %w", err)
	}
	return definition, nil
}

// backupHook finds the backup hook of one definition.
func backupHook(hooks []v1beta1.Hook) (v1beta1.Hook, bool) {
	for _, hook := range hooks {
		if hook.Name == BackupHook {
			return hook, true
		}
	}
	return v1beta1.Hook{}, false
}

// failRecord settles a run that failed before any worker action.
func (c *Controller) failRecord(run string, consistency v1beta1.BackupConsistency, scheduled time.Time, reason string) *v1beta1.BackupRecord {
	scheduledAt := metav1.NewTime(scheduled)
	startedAt := metav1.NewTime(c.clock())
	completedAt := metav1.NewTime(c.clock())
	return &v1beta1.BackupRecord{
		ScheduledAt: &scheduledAt, StartedAt: &startedAt, CompletedAt: &completedAt,
		Consistency: consistency, Path: run, Succeeded: false, Reason: reason,
	}
}

// failDrill settles a drill that could not finish or found the restored data
// different from the baseline: the drill fails, and the run it verified is no
// longer a success (C-42).
func (c *Controller) failDrill(drill *v1beta1.DrillRecord, last *v1beta1.BackupRecord, reason string) error {
	if drill == nil {
		return nil
	}
	completedAt := metav1.NewTime(c.clock())
	drill.Phase = v1beta1.DrillPhaseFailed
	drill.Succeeded = false
	drill.Reason = reason
	drill.CompletedAt = &completedAt
	if last != nil {
		last.Succeeded = false
		last.Reason = reason
		last.Diffs = drill.Diffs
	}
	return nil
}

// specOf renders one object's spec as the manifest stores it.
func specOf(object any) (map[string]any, error) {
	document, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return nil, err
	}
	spec, _, err := unstructured.NestedMap(document, "spec")
	return spec, err
}

// runDirectory names one run by its scheduled instant, so a retried round
// addresses the same directory (C-06).
func runDirectory(target, namespace, policy string, scheduled time.Time) string {
	return filepath.Join(target, namespace, policy, scheduled.UTC().Format("20060102T150405Z"))
}

// pruneRuns keeps the newest keep run directories of a policy; zero keeps every
// run (C-41).
func pruneRuns(target, namespace, policy string, keep int32) error {
	if keep <= 0 {
		return nil
	}
	directory := filepath.Join(target, namespace, policy)
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var runs []string
	for _, entry := range entries {
		if entry.IsDir() {
			runs = append(runs, entry.Name())
		}
	}
	sort.Strings(runs)
	for len(runs) > int(keep) {
		if err := os.RemoveAll(filepath.Join(directory, runs[0])); err != nil {
			return err
		}
		runs = runs[1:]
	}
	return nil
}

// ensureSchedule materializes the policy's CronJob (C-04): the cluster's cron
// controller starts a job on the schedule, and the job asks the control plane
// for the run. The document is applied every round, so a drifted schedule is
// written back.
func (c *Controller) ensureSchedule(ctx context.Context, policy *v1beta1.BackupPolicy) error {
	_, err := c.store.ApplyDownstream(ctx, policy.Namespace, scheduleDocument(policy, c.image))
	return err
}

// removeSchedule deletes the CronJob of a policy whose preflight refuses to run,
// so a refused policy schedules nothing (C-41).
func (c *Controller) removeSchedule(ctx context.Context, policy *v1beta1.BackupPolicy) error {
	err := c.store.DeleteDownstream(ctx, policy.Namespace, write.Ref{Group: "batch", Version: "v1", Kind: "CronJob", Namespace: policy.Namespace, Name: policy.Name})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// scheduleDocument is the CronJob that carries one policy's schedule (C-04).
func scheduleDocument(policy *v1beta1.BackupPolicy, image string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "CronJob",
		"metadata": map[string]any{
			"name":      policy.Name,
			"namespace": policy.Namespace,
			"labels": map[string]any{
				markers.ManagedBy: write.ManagedByValue,
				markers.OwnerUID:  string(policy.UID),
			},
		},
		"spec": map[string]any{
			"schedule":                   policy.Spec.Schedule,
			"concurrencyPolicy":          "Forbid",
			"successfulJobsHistoryLimit": int64(1),
			"failedJobsHistoryLimit":     int64(1),
			"jobTemplate": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{PolicyLabel: scheduleLabelValue(policy.Name)}},
				"spec": map[string]any{
					"backoffLimit": int64(0),
					"template": map[string]any{"spec": map[string]any{
						"restartPolicy": "Never",
						"containers": []any{map[string]any{
							"name":    "backup",
							"image":   image,
							"command": []any{"kos-backup", "trigger", "--namespace", policy.Namespace, "--policy", policy.Name},
						}},
					}},
				},
			},
		},
	}}
}

// policyTarget resolves where a policy writes: its own path, or the local disk
// the deployment reserves for backups when the policy leaves it empty (C-41).
func policyTarget(policy *v1beta1.BackupPolicy, defaultTarget string) string {
	if policy.Spec.Target.Path != "" {
		return policy.Spec.Target.Path
	}
	return defaultTarget
}

// targetPath is policyTarget for the controller's deployment configuration.
func (c *Controller) targetPath(policy *v1beta1.BackupPolicy) string {
	return policyTarget(policy, c.defaultTarget)
}

// validatePolicy applies C-41's required shape and returns the parsed schedule
// and selector. A policy without a target path writes to the deployment's
// reserved disk; when neither exists the policy has nowhere to write.
func validatePolicy(spec v1beta1.BackupPolicySpec, defaultTarget string, now time.Time) (Schedule, labels.Selector, string) {
	if strings.TrimSpace(spec.Selector) == "" {
		return Schedule{}, nil, "spec.selector 不能为空：选择器是策略绑定覆盖对象的唯一路径"
	}
	selector, err := labels.Parse(spec.Selector)
	if err != nil {
		return Schedule{}, nil, "spec.selector 不是合法的标签选择器：" + err.Error()
	}
	schedule, err := ParseSchedule(spec.Schedule)
	if err != nil {
		return Schedule{}, nil, "spec.schedule 不是合法的五段 cron 表达式：" + err.Error()
	}
	if schedule.Next(now).IsZero() {
		return Schedule{}, nil, "spec.schedule 永不匹配任何时刻，策略不会运行"
	}
	switch spec.Consistency {
	case v1beta1.BackupConsistencyCrash, v1beta1.BackupConsistencyQuiesced, v1beta1.BackupConsistencyHook:
	case "":
		return Schedule{}, nil, "spec.consistency 不能为空：崩溃一致、静默一致或应用钩子"
	default:
		return Schedule{}, nil, fmt.Sprintf("spec.consistency %q 不是崩溃一致、静默一致或应用钩子", spec.Consistency)
	}
	if strings.TrimSpace(spec.Target.Path) == "" && strings.TrimSpace(defaultTarget) == "" {
		return Schedule{}, nil, "spec.target.path 不能为空：部署没有配置默认备份目标"
	}
	if problem := validateFailureDomain(spec.Target.FailureDomain); problem != "" {
		return Schedule{}, nil, problem
	}
	if spec.Retention.Keep < 0 {
		return Schedule{}, nil, "spec.retention.keep 不能为负"
	}
	return schedule, selector, ""
}

// validateFailureDomain requires the form the source side produces, so a typo
// cannot silently disable the same-domain guard (C-41).
func validateFailureDomain(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	for _, prefix := range []string{"node:", "storage:"} {
		if strings.HasPrefix(value, prefix) && strings.TrimSpace(strings.TrimPrefix(value, prefix)) != "" {
			return ""
		}
	}
	return "spec.target.failureDomain 必须形如 node:<节点> 或 storage:<驱动>"
}

// validateWorkerImage requires the digest-pinned image C-17 asks of a component
// reference: a mutable tag makes the schedule unreproducible.
func validateWorkerImage(image string) string {
	if image == "" {
		return "部署未配置备份组件镜像：计划触发无法物化"
	}
	index := strings.Index(image, "@sha256:")
	if index <= 0 {
		return "备份组件镜像必须按 sha256 摘要固定，不能使用可变标签"
	}
	sum := image[index+len("@sha256:"):]
	if len(sum) != 64 {
		return "备份组件镜像的 sha256 摘要必须是 64 个十六进制字符"
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return "备份组件镜像的 sha256 摘要必须是 64 个十六进制字符"
	}
	return ""
}

// baseStatus carries the status a round starts from: the newest run and drill
// stay, and the conditions this reconciler owns are cleared so a healthy round
// stops reporting an earlier failure (C-07). The record is copied, so a round
// that advances an action does not mutate the status it compares against.
func baseStatus(policy *v1beta1.BackupPolicy) v1beta1.BackupPolicyStatus {
	status := *policy.Status.DeepCopy()
	for _, conditionType := range []string{ConditionBackupFailed, ConditionDrillMismatch, ConditionFailureDomainUnknown, ConditionScheduleJobFailed} {
		meta.RemoveStatusCondition(&status.Conditions, conditionType)
	}
	return status
}

// writeStatus writes the status only when it changed, so a repeat observation
// produces no write (C-06).
func (c *Controller) writeStatus(ctx context.Context, policy *v1beta1.BackupPolicy, status v1beta1.BackupPolicyStatus) error {
	if sameStatus(policy.Status, status) {
		return nil
	}
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return fmt.Errorf("encode the backup policy status: %w", err)
	}
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "BackupPolicy",
		"metadata":   map[string]any{"name": policy.Name, "namespace": policy.Namespace},
		"status":     encoded,
	}}
	_, err = c.store.ApplyStatus(ctx, document)
	return err
}

// sameStatus compares two statuses by their encoding.
func sameStatus(left, right v1beta1.BackupPolicyStatus) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

// isSubmitted reports whether this controller already handed the action off.
func (c *Controller) isSubmitted(key string) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.submitted[key]
}

// markSubmitted records one handed-off action.
func (c *Controller) markSubmitted(key string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.submitted[key] = true
}

// forget drops a settled action, so a restarted controller resubmits an action
// left in flight instead of losing it (C-06).
func (c *Controller) forget(task Task) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	delete(c.submitted, task.identity())
}
