package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
)

// Condition types a restore job fails with (C-07, C-42).
const (
	ConditionPreflightFailed    = "PreflightFailed"
	ConditionVerificationFailed = "VerificationFailed"
)

// RestoreController drives a RestoreJob through the four phases C-42 fixes:
// Preflight, Restoring, Verifying and a terminal Succeeded or Failed. The copy
// and the checksum go to the Worker; the controller only records and judges
// (C-06, C-42).
type RestoreController struct {
	store         Store
	plane         DataPlane
	worker        Worker
	clock         func() time.Time
	defaultTarget string
	owner         string

	mutex     sync.Mutex
	submitted map[string]bool
}

// NewRestoreController builds a restore controller over one store and one
// worker.
func NewRestoreController(store Store, plane DataPlane, worker Worker, options Options) *RestoreController {
	controller := &RestoreController{
		store: store, plane: plane, worker: worker, clock: options.Clock,
		defaultTarget: options.DefaultTarget, owner: options.Owner,
		submitted: make(map[string]bool),
	}
	if controller.clock == nil {
		controller.clock = time.Now
	}
	return controller
}

// Reconcile runs one round of one restore job (C-06, C-42).
func (c *RestoreController) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	if request.Kind != "RestoreJob" {
		return reconcile.Result{}, nil
	}
	stored, err := c.store.Get(ctx, write.Ref{Group: api.Group, Version: "v1beta1", Kind: "RestoreJob", Namespace: request.Namespace, Name: request.Name})
	if apierrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	job := &v1beta1.RestoreJob{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, job); err != nil {
		return reconcile.Result{}, fmt.Errorf("decode the restore job: %w", err)
	}
	if job.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}
	status := *job.Status.DeepCopy()
	c.observeResults(ctx, job, &status)
	if status.Phase == v1beta1.RestorePhaseSucceeded || status.Phase == v1beta1.RestorePhaseFailed {
		return c.settle(ctx, job, &status)
	}
	switch status.Phase {
	case "", v1beta1.RestorePhasePreflight:
		return c.preflight(ctx, job, &status)
	case v1beta1.RestorePhaseRestoring:
		return c.restoring(ctx, job, &status)
	case v1beta1.RestorePhaseVerifying:
		return c.verifying(ctx, job, &status)
	default:
		return reconcile.Result{}, nil
	}
}

// preflight validates the request before any data moves: the run is the
// policy's own successful one, below the policy's target, and the manifest
// belongs to that policy; the target is a new instance that does not exist yet,
// or an in-place restore whose application is stopped (C-41, C-42).
func (c *RestoreController) preflight(ctx context.Context, job *v1beta1.RestoreJob, status *v1beta1.RestoreJobStatus) (reconcile.Result, error) {
	if job.Spec.Policy == "" {
		return c.fail(ctx, job, status, ConditionPreflightFailed, "恢复作业需要 spec.policy：恢复目标由产生该备份的策略决定")
	}
	policy, err := c.policyOf(ctx, job)
	if err != nil {
		return reconcile.Result{}, err
	}
	if policy == nil {
		return c.fail(ctx, job, status, ConditionPreflightFailed, fmt.Sprintf("恢复作业引用的备份策略 %q 不存在", job.Spec.Policy))
	}
	// The global Owner writes every object (C-59); pkg/access exempts it from
	// the member ownership rule, so the disaster-recovery path must exempt it
	// here too.
	if policy.Spec.Owner != job.Spec.Owner && job.Spec.Owner != c.owner {
		return c.fail(ctx, job, status, ConditionPreflightFailed,
			fmt.Sprintf("备份策略属于 %q，恢复作业属于 %q：跨所有者的恢复需要全局 Owner", policy.Spec.Owner, job.Spec.Owner))
	}
	last := policy.Status.LastBackup
	if last == nil || !last.Succeeded || last.Path == "" {
		return c.fail(ctx, job, status, ConditionPreflightFailed, "备份策略没有成功的备份可恢复")
	}
	run := last.Path
	if !withinDirectory(policyTarget(policy, c.defaultTarget), run) {
		return c.fail(ctx, job, status, ConditionPreflightFailed, "备份运行不在策略的备份目标之下，拒绝读取")
	}
	manifest, err := ReadManifest(run)
	if err != nil {
		return c.fail(ctx, job, status, ConditionPreflightFailed, "读取备份清单失败："+err.Error())
	}
	if manifest.Policy != policy.Name {
		return c.fail(ctx, job, status, ConditionPreflightFailed,
			fmt.Sprintf("备份属于策略 %q，不是本作业引用的策略 %q", manifest.Policy, policy.Name))
	}
	if manifest.Namespace != policy.Namespace {
		return c.fail(ctx, job, status, ConditionPreflightFailed,
			fmt.Sprintf("备份属于命名空间 %q，不能恢复到 %q", manifest.Namespace, job.Namespace))
	}
	covered := make(map[string]bool, len(manifest.Volumes))
	for _, volume := range manifest.Volumes {
		covered[volume.Name] = true
	}
	for _, application := range manifest.Applications {
		for _, name := range stringList(application.Spec["volumes"]) {
			if !covered[name] {
				return c.fail(ctx, job, status, ConditionPreflightFailed, fmt.Sprintf("备份未覆盖应用 %q 引用的卷 %q", application.Name, name))
			}
		}
	}
	if job.Spec.InPlace {
		if len(manifest.Applications) != 1 {
			return c.fail(ctx, job, status, ConditionPreflightFailed, "原地恢复要求备份覆盖单个应用")
		}
		application, err := c.applicationOf(ctx, job.Namespace, manifest.Applications[0].Name)
		if err != nil {
			return reconcile.Result{}, err
		}
		if application == nil {
			return c.fail(ctx, job, status, ConditionPreflightFailed, "原地恢复要求源应用存在")
		}
		if !application.Spec.Paused {
			return c.fail(ctx, job, status, ConditionPreflightFailed, fmt.Sprintf("原地恢复要求应用 %q 停机", application.Name))
		}
		status.TargetApplication = application.Name
		for _, volume := range manifest.Volumes {
			status.TargetVolumes = append(status.TargetVolumes, volume.Name)
		}
	} else {
		if job.Spec.TargetApplication == "" {
			return c.fail(ctx, job, status, ConditionPreflightFailed, "恢复到新实例需要 spec.targetApplication")
		}
		if len(manifest.Applications) != 1 {
			return c.fail(ctx, job, status, ConditionPreflightFailed, "恢复到新实例要求备份覆盖单个应用")
		}
		targets, problem := targetNames(job.Spec.TargetApplication, manifest)
		if problem != "" {
			return c.fail(ctx, job, status, ConditionPreflightFailed, problem)
		}
		existing, err := c.applicationOf(ctx, job.Namespace, targets.application)
		if err != nil {
			return reconcile.Result{}, err
		}
		if existing != nil {
			return c.fail(ctx, job, status, ConditionPreflightFailed, fmt.Sprintf("目标应用 %q 已存在：恢复到新实例不覆盖现有对象", targets.application))
		}
		for _, name := range targets.volumes {
			volume, err := c.volumeOf(ctx, job.Namespace, name)
			if err != nil {
				return reconcile.Result{}, err
			}
			if volume != nil {
				return c.fail(ctx, job, status, ConditionPreflightFailed, fmt.Sprintf("目标卷 %q 已存在：恢复到新实例不覆盖现有对象", name))
			}
		}
		status.TargetApplication = targets.application
		status.TargetVolumes = targets.volumes
	}
	startedAt := metav1.NewTime(c.clock())
	status.StartedAt = &startedAt
	status.BackupPath = run
	baseline := manifest.Baseline
	status.Baseline = &baseline
	status.Phase = v1beta1.RestorePhaseRestoring
	status.Conditions = state.Set(status.Conditions, state.Blocked(job.Generation, v1beta1.RestorePhaseRestoring, "正在把备份写入新实例"))
	return c.settle(ctx, job, status)
}

// restoring hands the copy to the worker; the next observation creates the new
// instance and advances to Verifying (C-41, C-42).
func (c *RestoreController) restoring(ctx context.Context, job *v1beta1.RestoreJob, status *v1beta1.RestoreJobStatus) (reconcile.Result, error) {
	manifest, err := ReadManifest(status.BackupPath)
	if err != nil {
		return c.fail(ctx, job, status, ConditionPreflightFailed, "读取备份清单失败："+err.Error())
	}
	if len(status.TargetVolumes) != len(manifest.Volumes) {
		return c.fail(ctx, job, status, ConditionPreflightFailed, "恢复目标卷与备份记录的卷数量不一致")
	}
	if status.Running == nil {
		status.Running = c.begin(TaskCopy, status.BackupPath, status.TargetVolumes)
	}
	if err := c.ensureSubmitted(job, status, manifest); err != nil {
		return reconcile.Result{}, err
	}
	return c.settle(ctx, job, status)
}

// verifying hands the measurement to the worker and compares the result with
// the baseline on a later observation; any difference fails the job (C-42).
func (c *RestoreController) verifying(ctx context.Context, job *v1beta1.RestoreJob, status *v1beta1.RestoreJobStatus) (reconcile.Result, error) {
	manifest, err := ReadManifest(status.BackupPath)
	if err != nil {
		return c.fail(ctx, job, status, ConditionVerificationFailed, "读取备份清单失败："+err.Error())
	}
	if len(status.TargetVolumes) != len(manifest.Volumes) {
		return c.fail(ctx, job, status, ConditionVerificationFailed, "恢复目标卷与备份记录的卷数量不一致")
	}
	if status.Running == nil {
		status.Running = c.begin(TaskMeasure, status.BackupPath, status.TargetVolumes)
	}
	if err := c.ensureSubmitted(job, status, manifest); err != nil {
		return reconcile.Result{}, err
	}
	return c.settle(ctx, job, status)
}

// observeResults applies the outcomes the worker reported (C-06).
func (c *RestoreController) observeResults(ctx context.Context, job *v1beta1.RestoreJob, status *v1beta1.RestoreJobStatus) {
	for _, result := range c.worker.Completed(string(job.UID)) {
		c.forget(result.Task)
		running := status.Running
		if running == nil || actionIdentity(string(job.UID), running.Sequence, running.Scope, running.Kind, running.Run) != result.Task.identity() {
			continue
		}
		status.Running = nil
		switch result.Task.Kind {
		case TaskCopy:
			if result.Err != nil {
				c.failAsync(status, job.Generation, ConditionPreflightFailed, "复制恢复数据失败："+result.Err.Error())
				continue
			}
			manifest, err := ReadManifest(running.Run)
			if err != nil {
				c.failAsync(status, job.Generation, ConditionPreflightFailed, "读取备份清单失败："+err.Error())
				continue
			}
			if !job.Spec.InPlace {
				if err := c.createInstance(ctx, job, manifest, status); err != nil {
					c.failAsync(status, job.Generation, ConditionPreflightFailed, "创建新实例失败："+err.Error())
					continue
				}
			}
			status.Phase = v1beta1.RestorePhaseVerifying
		case TaskMeasure:
			if result.Err != nil {
				c.failAsync(status, job.Generation, ConditionVerificationFailed, "无法测量恢复数据："+result.Err.Error())
				continue
			}
			manifest, err := ReadManifest(running.Run)
			if err != nil {
				c.failAsync(status, job.Generation, ConditionVerificationFailed, "读取备份清单失败："+err.Error())
				continue
			}
			observed := result.Baseline
			status.Observed = &observed
			diffs := Compare(manifest.Baseline, observed)
			if len(diffs) > 0 {
				status.Diffs = diffs
				c.failAsync(status, job.Generation, ConditionVerificationFailed, "恢复数据与基线不一致："+DescribeDiffs(diffs))
				continue
			}
			status.Phase = v1beta1.RestorePhaseSucceeded
		}
	}
}

// failAsync settles a job whose handed-off action failed or mismatched.
func (c *RestoreController) failAsync(status *v1beta1.RestoreJobStatus, generation int64, conditionType, reason string) {
	completedAt := metav1.NewTime(c.clock())
	status.CompletedAt = &completedAt
	status.Phase = v1beta1.RestorePhaseFailed
	status.Health = v1beta1.Health{State: v1beta1.HealthStateUnavailable}
	status.Conditions = state.Set(status.Conditions, state.Blocked(generation, conditionType, reason))
	status.Conditions = state.Set(status.Conditions, state.Failure(conditionType, generation, conditionType, reason))
}

// ensureSubmitted hands the recorded action off exactly once per controller
// lifetime (C-06).
func (c *RestoreController) ensureSubmitted(job *v1beta1.RestoreJob, status *v1beta1.RestoreJobStatus, manifest Manifest) error {
	running := status.Running
	key := actionIdentity(string(job.UID), running.Sequence, running.Scope, running.Kind, running.Run)
	if c.isSubmitted(key) {
		return nil
	}
	task := Task{
		Owner: string(job.UID), Sequence: running.Sequence, Scope: running.Scope,
		Kind: running.Kind, Run: running.Run,
	}
	for index, volume := range manifest.Volumes {
		destination := c.plane.Locate(Source{Namespace: job.Namespace, Volume: status.TargetVolumes[index]})
		if running.Kind == TaskCopy {
			task.Copies = append(task.Copies, Copy{Recorded: volume.Name, Destination: destination})
		} else if running.Kind == TaskMeasure {
			task.Directories = append(task.Directories, VolumeDir{Volume: volume.Name, Path: destination})
		}
	}
	c.worker.Submit(task)
	c.markSubmitted(key)
	return nil
}

// begin records one restore action as in flight together with the identity a
// result pairs on: the job, an ordinal unique on it and the target volume set
// (C-06).
func (c *RestoreController) begin(kind, run string, targets []string) *v1beta1.Running {
	now := c.clock()
	startedAt := metav1.NewTime(now)
	return &v1beta1.Running{
		Kind: kind, Run: run, Scope: taskScope(targets),
		Sequence: now.UnixNano(), StartedAt: &startedAt,
	}
}

// settle writes the status and hands control back; a phase with an action in
// flight is re-observed quickly because the outcome is only visible by polling
// the worker (C-06).
func (c *RestoreController) settle(ctx context.Context, job *v1beta1.RestoreJob, status *v1beta1.RestoreJobStatus) (reconcile.Result, error) {
	if status.Phase == v1beta1.RestorePhaseSucceeded {
		completedAt := metav1.NewTime(c.clock())
		status.CompletedAt = &completedAt
		status.Health = v1beta1.Health{State: v1beta1.HealthStateHealthy}
		status.Conditions = state.Set(status.Conditions, state.Applied(job.Generation, "恢复数据与基线四项全等"))
	}
	if err := c.writeStatus(ctx, job, *status); err != nil {
		return reconcile.Result{}, err
	}
	switch status.Phase {
	case v1beta1.RestorePhaseSucceeded, v1beta1.RestorePhaseFailed:
		return reconcile.Result{}, nil
	default:
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
}

// fail ends a restore in Failed and keeps the reason readable (C-42).
func (c *RestoreController) fail(ctx context.Context, job *v1beta1.RestoreJob, status *v1beta1.RestoreJobStatus, conditionType, reason string) (reconcile.Result, error) {
	c.failAsync(status, job.Generation, conditionType, reason)
	return c.settle(ctx, job, status)
}

// policyOf reads the policy a restore names in the same namespace; a missing
// policy is nil.
func (c *RestoreController) policyOf(ctx context.Context, job *v1beta1.RestoreJob) (*v1beta1.BackupPolicy, error) {
	if job.Spec.Policy == "" {
		return nil, nil
	}
	stored, err := c.store.Get(ctx, write.Ref{Group: api.Group, Version: "v1beta1", Kind: "BackupPolicy", Namespace: job.Namespace, Name: job.Spec.Policy})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	policy := &v1beta1.BackupPolicy{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, policy); err != nil {
		return nil, fmt.Errorf("decode the backup policy: %w", err)
	}
	return policy, nil
}

// applicationOf reads one application; a missing object is nil.
func (c *RestoreController) applicationOf(ctx context.Context, namespace, name string) (*v1beta1.Application, error) {
	stored, err := c.store.GetResource(ctx, applicationResource, namespace, name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	application := &v1beta1.Application{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, application); err != nil {
		return nil, fmt.Errorf("decode the application: %w", err)
	}
	return application, nil
}

// volumeOf reads one volume; a missing object is nil.
func (c *RestoreController) volumeOf(ctx context.Context, namespace, name string) (*v1beta1.Volume, error) {
	stored, err := c.store.GetResource(ctx, volumeResource, namespace, name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	volume := &v1beta1.Volume{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, volume); err != nil {
		return nil, fmt.Errorf("decode the volume: %w", err)
	}
	return volume, nil
}

// instanceTargets names the new instance: the application keeps the requested
// name, and every volume becomes <target>-<sourceVolume> (C-41).
type instanceTargets struct {
	application string
	volumes     []string
}

// targetNames derives and validates the new instance names from the manifest.
func targetNames(target string, manifest Manifest) (instanceTargets, string) {
	names := instanceTargets{application: target}
	candidates := []string{names.application}
	for _, volume := range manifest.Volumes {
		candidates = append(candidates, instanceVolumeName(target, volume.Name))
	}
	for _, candidate := range candidates {
		if problems := validation.IsDNS1123Subdomain(candidate); len(problems) > 0 {
			return instanceTargets{}, fmt.Sprintf("目标名 %q 不合法：%s", candidate, problems[0])
		}
	}
	for _, volume := range manifest.Volumes {
		names.volumes = append(names.volumes, instanceVolumeName(target, volume.Name))
	}
	return names, ""
}

// instanceVolumeName is the name of the new volume one recorded volume becomes.
func instanceVolumeName(target, recorded string) string {
	return target + "-" + recorded
}

// createInstance creates the new application and volumes the restored data
// belongs to; the restore flow writes no existing object (C-41, ADR-015).
func (c *RestoreController) createInstance(ctx context.Context, job *v1beta1.RestoreJob, manifest Manifest, status *v1beta1.RestoreJobStatus) error {
	remapped := make(map[string]string, len(manifest.Volumes))
	for index, volume := range manifest.Volumes {
		remapped[volume.Name] = status.TargetVolumes[index]
		document := instanceDocument("Volume", job.Namespace, status.TargetVolumes[index], job.Spec.Owner, volume.Spec)
		if _, err := c.store.CreateObject(ctx, document); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return err
			}
			if err := c.requireOwnInstance(ctx, job, "Volume", status.TargetVolumes[index]); err != nil {
				return err
			}
		}
	}
	spec := runtime.DeepCopyJSON(manifest.Applications[0].Spec)
	if spec == nil {
		spec = map[string]any{}
	}
	volumes := make([]any, 0, len(remapped))
	for _, name := range stringList(spec["volumes"]) {
		volumes = append(volumes, remapped[name])
	}
	if len(volumes) > 0 {
		spec["volumes"] = volumes
	}
	document := instanceDocument("Application", job.Namespace, status.TargetApplication, job.Spec.Owner, spec)
	if _, err := c.store.CreateObject(ctx, document); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		return c.requireOwnInstance(ctx, job, "Application", status.TargetApplication)
	}
	return nil
}

// requireOwnInstance checks that a target object that appeared during the
// restore is the one this restore created. A user object with the same name is
// reported as a failure, never adopted silently (C-41).
func (c *RestoreController) requireOwnInstance(ctx context.Context, job *v1beta1.RestoreJob, kind, name string) error {
	stored, err := c.store.GetResource(ctx, instanceResource(kind), job.Namespace, name)
	if err != nil {
		return err
	}
	owner, _, _ := unstructured.NestedString(stored.Object, "spec", "owner")
	if owner == job.Spec.Owner && stored.GetLabels()[markers.ManagedBy] == write.ManagedByValue {
		return nil
	}
	return fmt.Errorf("目标%s %q 在恢复窗口内已存在，且不是本次恢复创建的：拒绝接管", kind, name)
}

// instanceResource resolves the class of one new-instance object.
func instanceResource(kind string) schema.GroupVersionResource {
	switch kind {
	case "Volume":
		return volumeResource
	default:
		return applicationResource
	}
}

// instanceDocument builds one object of the new instance. The owner is the
// restore request's owner, so the new instance belongs to the account that
// asked for the restore (C-60).
func instanceDocument(kind, namespace, name, owner string, spec map[string]any) *unstructured.Unstructured {
	if spec == nil {
		spec = map[string]any{}
	} else {
		spec = runtime.DeepCopyJSON(spec)
	}
	if owner != "" {
		spec["owner"] = owner
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    map[string]any{markers.ManagedBy: write.ManagedByValue},
		},
		"spec": spec,
	}}
}

// stringList reads a JSON list of strings from a spec.
func stringList(value any) []string {
	raw, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]string); ok {
			return typed
		}
		return nil
	}
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		if name, ok := item.(string); ok {
			names = append(names, name)
		}
	}
	return names
}

// withinDirectory reports whether path is root or below it, so a restore never
// reads a run outside the policy's backup target (C-41).
func withinDirectory(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// writeStatus writes the status only when it changed, so a repeat observation
// produces no write (C-06).
func (c *RestoreController) writeStatus(ctx context.Context, job *v1beta1.RestoreJob, status v1beta1.RestoreJobStatus) error {
	current, currentErr := json.Marshal(job.Status)
	next, nextErr := json.Marshal(status)
	if currentErr == nil && nextErr == nil && string(current) == string(next) {
		return nil
	}
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return fmt.Errorf("encode the restore status: %w", err)
	}
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "RestoreJob",
		"metadata":   map[string]any{"name": job.Name, "namespace": job.Namespace},
		"status":     encoded,
	}}
	_, err = c.store.ApplyStatus(ctx, document)
	return err
}

// isSubmitted reports whether this controller already handed the action off.
func (c *RestoreController) isSubmitted(key string) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.submitted[key]
}

// markSubmitted records one handed-off action.
func (c *RestoreController) markSubmitted(key string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.submitted[key] = true
}

// forget drops a settled action so a restarted controller resubmits it.
func (c *RestoreController) forget(task Task) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	delete(c.submitted, task.identity())
}
