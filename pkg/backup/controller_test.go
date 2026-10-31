package backup

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/write"
)

// testWorkerImage is a digest-pinned reference the cases hand the controller.
const testWorkerImage = "example.test/kos-backup@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// fakeStore is an in-memory Store: a round's reads and writes are observable
// without an API server, and several controllers can share one.
type fakeStore struct {
	mutex   sync.Mutex
	objects map[string]*unstructured.Unstructured
}

// newFakeStore builds an empty store.
func newFakeStore() *fakeStore {
	return &fakeStore{objects: make(map[string]*unstructured.Unstructured)}
}

// fakeKey names one stored object.
func fakeKey(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}

// fakeKinds maps the resources the controllers read to their kinds.
var fakeKinds = map[schema.GroupVersionResource]string{
	applicationResource: "Application",
	volumeResource:      "Volume",
	definitionResource:  "ApplicationDefinition",
	claimResource:       "PersistentVolumeClaim",
	persistentResource:  "PersistentVolume",
	cronJobResource:     "CronJob",
	jobResource:         "Job",
}

// put stores one object.
func (s *fakeStore) put(object *unstructured.Unstructured) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.objects[fakeKey(object.GetKind(), object.GetNamespace(), object.GetName())] = object.DeepCopy()
}

// lookup reads one object.
func (s *fakeStore) lookup(kind, namespace, name string) (*unstructured.Unstructured, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	object, ok := s.objects[fakeKey(kind, namespace, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: api.Group, Resource: strings.ToLower(kind)}, name)
	}
	return object.DeepCopy(), nil
}

// Get implements Store.
func (s *fakeStore) Get(_ context.Context, ref write.Ref) (*unstructured.Unstructured, error) {
	return s.lookup(ref.Kind, ref.Namespace, ref.Name)
}

// List implements Store.
func (s *fakeStore) List(_ context.Context, resource schema.GroupVersionResource, namespace, selector string) ([]unstructured.Unstructured, error) {
	kind, known := fakeKinds[resource]
	if !known {
		return nil, nil
	}
	parsed, err := labels.Parse(selector)
	if err != nil {
		return nil, err
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	var matched []unstructured.Unstructured
	for _, object := range s.objects {
		if object.GetKind() != kind || object.GetNamespace() != namespace {
			continue
		}
		if !parsed.Matches(labels.Set(object.GetLabels())) {
			continue
		}
		matched = append(matched, *object.DeepCopy())
	}
	return matched, nil
}

// GetResource implements Store.
func (s *fakeStore) GetResource(_ context.Context, resource schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	kind, known := fakeKinds[resource]
	if !known {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: resource.Resource}, name)
	}
	return s.lookup(kind, namespace, name)
}

// ApplyStatus implements Store.
func (s *fakeStore) ApplyStatus(_ context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	key := fakeKey(document.GetKind(), document.GetNamespace(), document.GetName())
	object, ok := s.objects[key]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: api.Group, Resource: strings.ToLower(document.GetKind())}, document.GetName())
	}
	status, _, err := unstructured.NestedMap(document.Object, "status")
	if err != nil {
		return nil, err
	}
	object.Object["status"] = status
	return object.DeepCopy(), nil
}

// ApplyDownstream implements Store. Like the API server, an apply of the main
// resource leaves the status subresource alone.
func (s *fakeStore) ApplyDownstream(_ context.Context, _ string, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	key := fakeKey(document.GetKind(), document.GetNamespace(), document.GetName())
	stored := document.DeepCopy()
	if existing, ok := s.objects[key]; ok {
		if status, hasStatus := existing.Object["status"]; hasStatus {
			stored.Object["status"] = status
		}
	}
	s.objects[key] = stored
	return stored.DeepCopy(), nil
}

// DeleteDownstream implements Store.
func (s *fakeStore) DeleteDownstream(_ context.Context, _ string, ref write.Ref) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	key := fakeKey(ref.Kind, ref.Namespace, ref.Name)
	if _, ok := s.objects[key]; !ok {
		return apierrors.NewNotFound(schema.GroupResource{Group: ref.Group, Resource: strings.ToLower(ref.Kind)}, ref.Name)
	}
	delete(s.objects, key)
	return nil
}

// CreateObject implements Store.
func (s *fakeStore) CreateObject(_ context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	key := fakeKey(document.GetKind(), document.GetNamespace(), document.GetName())
	if _, exists := s.objects[key]; exists {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: api.Group, Resource: strings.ToLower(document.GetKind())}, document.GetName())
	}
	s.objects[key] = document.DeepCopy()
	return document.DeepCopy(), nil
}

// scheduleCronJob writes the newest scheduled instant on a stored CronJob, as
// the cluster's cron controller does.
func (s *fakeStore) scheduleCronJob(t *testing.T, namespace, name string, at time.Time) {
	t.Helper()
	stored, err := s.lookup("CronJob", namespace, name)
	if err != nil {
		t.Fatalf("read cron job %s/%s: %v", namespace, name, err)
	}
	cronJob := &batchv1.CronJob{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, cronJob); err != nil {
		t.Fatalf("decode cron job: %v", err)
	}
	cronJob.Status.LastScheduleTime = &metav1.Time{Time: at}
	document, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cronJob)
	if err != nil {
		t.Fatalf("encode cron job: %v", err)
	}
	s.put(&unstructured.Unstructured{Object: document})
}

// countingPlane counts the long actions a worker executed.
type countingPlane struct {
	DataPlane
	mutex    sync.Mutex
	captures int
	copies   int
}

// Capture implements DataPlane.
func (p *countingPlane) Capture(ctx context.Context, run string, sources []Source) (v1beta1.BackupBaseline, error) {
	p.mutex.Lock()
	p.captures++
	p.mutex.Unlock()
	return p.DataPlane.Capture(ctx, run, sources)
}

// Restore implements DataPlane.
func (p *countingPlane) Restore(ctx context.Context, run string, copies []Copy) error {
	p.mutex.Lock()
	p.copies++
	p.mutex.Unlock()
	return p.DataPlane.Restore(ctx, run, copies)
}

// counts reports the actions executed so far.
func (p *countingPlane) counts() (int, int) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.captures, p.copies
}

// fakePolicy stores one backup policy.
func (s *fakeStore) putPolicy(namespace, name, uid, owner, target string, last *v1beta1.BackupRecord) {
	policy := &v1beta1.BackupPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.Group + "/v1beta1", Kind: "BackupPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(uid)},
		Spec: v1beta1.BackupPolicySpec{
			Owner: owner, Selector: "backup=true", Schedule: "0 3 * * *",
			Consistency: v1beta1.BackupConsistencyCrash,
			Target:      v1beta1.BackupTarget{Path: target, FailureDomain: "node:node-b"},
		},
		Status: v1beta1.BackupPolicyStatus{LastBackup: last},
	}
	document, err := runtime.DefaultUnstructuredConverter.ToUnstructured(policy)
	if err != nil {
		panic(err)
	}
	s.put(&unstructured.Unstructured{Object: document})
}

// putApplication stores one covered application.
func (s *fakeStore) putApplication(namespace, name, owner string, volumes []string) {
	application := &v1beta1.Application{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.Group + "/v1beta1", Kind: "Application"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"backup": "true", "managed-by": write.ManagedByValue}},
		Spec:       v1beta1.ApplicationSpec{Owner: owner, Namespace: namespace, Volumes: volumes},
	}
	document, err := runtime.DefaultUnstructuredConverter.ToUnstructured(application)
	if err != nil {
		panic(err)
	}
	s.put(&unstructured.Unstructured{Object: document})
}

// putVolume stores one covered volume.
func (s *fakeStore) putVolume(namespace, name, owner string) {
	volume := &v1beta1.Volume{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.Group + "/v1beta1", Kind: "Volume"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"backup": "true", "managed-by": write.ManagedByValue}},
		Spec:       v1beta1.VolumeSpec{Owner: owner, Mobility: v1beta1.VolumeMobilityClusterMovable, Capacity: "1Gi"},
	}
	document, err := runtime.DefaultUnstructuredConverter.ToUnstructured(volume)
	if err != nil {
		panic(err)
	}
	s.put(&unstructured.Unstructured{Object: document})
}

// putRestoreJob stores one restore job.
func (s *fakeStore) putRestoreJob(namespace, name, uid, owner, policy, target string) {
	job := &v1beta1.RestoreJob{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.Group + "/v1beta1", Kind: "RestoreJob"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(uid)},
		Spec: v1beta1.RestoreJobSpec{
			Owner: owner, Policy: policy, TargetApplication: target,
		},
	}
	document, err := runtime.DefaultUnstructuredConverter.ToUnstructured(job)
	if err != nil {
		panic(err)
	}
	s.put(&unstructured.Unstructured{Object: document})
}

// storedPolicy decodes one stored policy.
func (s *fakeStore) storedPolicy(t *testing.T, namespace, name string) *v1beta1.BackupPolicy {
	t.Helper()
	stored, err := s.lookup("BackupPolicy", namespace, name)
	if err != nil {
		t.Fatalf("read policy %s: %v", name, err)
	}
	policy := &v1beta1.BackupPolicy{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, policy); err != nil {
		t.Fatalf("decode policy %s: %v", name, err)
	}
	return policy
}

// storedRestoreJob decodes one stored restore job.
func (s *fakeStore) storedRestoreJob(t *testing.T, namespace, name string) *v1beta1.RestoreJob {
	t.Helper()
	stored, err := s.lookup("RestoreJob", namespace, name)
	if err != nil {
		t.Fatalf("read restore job %s: %v", name, err)
	}
	job := &v1beta1.RestoreJob{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, job); err != nil {
		t.Fatalf("decode restore job %s: %v", name, err)
	}
	return job
}

// reconcilePolicy runs one policy round and waits for the handed-off action.
func reconcilePolicy(t *testing.T, controller *Controller, worker *LocalWorker, namespace, name string) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{Kind: "BackupPolicy", Namespace: namespace, Name: name}); err != nil {
		t.Fatalf("reconcile policy %s: %v", name, err)
	}
	worker.Wait()
}

// reconcileRestore runs one restore round and waits for the handed-off action.
func reconcileRestore(t *testing.T, controller *RestoreController, worker *LocalWorker, namespace, name string) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{Kind: "RestoreJob", Namespace: namespace, Name: name}); err != nil {
		t.Fatalf("reconcile restore job %s: %v", name, err)
	}
	worker.Wait()
}

// TestWorkerRoutesResultsByOwner runs two policies through one worker: each
// controller must take only its own object's outcomes, so neither loses a
// result or repeats a capture (C-06).
func TestWorkerRoutesResultsByOwner(t *testing.T) {
	store := newFakeStore()
	dataRoot := t.TempDir()
	plane := &countingPlane{DataPlane: DirDataPlane{Root: dataRoot}}
	worker := NewLocalWorker(plane)

	for _, fixture := range []struct{ name, uid, owner string }{
		{"alpha", "uid-alpha", "alice"},
		{"beta", "uid-beta", "bob"},
	} {
		target := filepath.Join(t.TempDir(), "backups")
		store.putPolicy("apps", fixture.name, fixture.uid, fixture.owner, target, nil)
		store.putApplication("apps", fixture.name+"-app", fixture.owner, []string{fixture.name + "-data"})
		store.putVolume("apps", fixture.name+"-data", fixture.owner)
		seedFile(t, filepath.Join(dataRoot, "apps", fixture.name+"-data", "note.txt"), fixture.name)
	}

	alpha := NewController(store, worker, Options{WorkerImage: testWorkerImage})
	beta := NewController(store, worker, Options{WorkerImage: testWorkerImage})
	scheduled := time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)

	reconcilePolicy(t, alpha, worker, "apps", "alpha")
	store.scheduleCronJob(t, "apps", "alpha", scheduled)
	reconcilePolicy(t, alpha, worker, "apps", "alpha")
	reconcilePolicy(t, beta, worker, "apps", "beta")
	store.scheduleCronJob(t, "apps", "beta", scheduled)
	reconcilePolicy(t, beta, worker, "apps", "beta")

	// Both outcomes are queued; each controller may take only its own.
	reconcilePolicy(t, alpha, worker, "apps", "alpha")
	reconcilePolicy(t, beta, worker, "apps", "beta")
	reconcilePolicy(t, alpha, worker, "apps", "alpha")
	reconcilePolicy(t, beta, worker, "apps", "beta")

	for _, name := range []string{"alpha", "beta"} {
		policy := store.storedPolicy(t, "apps", name)
		if policy.Status.LastBackup == nil || !policy.Status.LastBackup.Succeeded {
			t.Fatalf("policy %s did not record its own run: %+v", name, policy.Status.LastBackup)
		}
		if policy.Status.Running != nil {
			t.Fatalf("policy %s still holds an in-flight action: %+v", name, policy.Status.Running)
		}
	}
	alphaRun := store.storedPolicy(t, "apps", "alpha").Status.LastBackup.Path
	betaRun := store.storedPolicy(t, "apps", "beta").Status.LastBackup.Path
	if alphaRun == betaRun {
		t.Fatalf("both policies recorded the same run %q", alphaRun)
	}
	if captures, _ := plane.counts(); captures != 2 {
		t.Fatalf("captures = %d, want one per policy", captures)
	}
}

// seedRestoreRun writes one successful run and the policy that records it.
func seedRestoreRun(t *testing.T, store *fakeStore, plane DataPlane, dataRoot, policyOwner, policyName string) (string, v1beta1.BackupBaseline) {
	t.Helper()
	ctx := context.Background()
	target := filepath.Join(t.TempDir(), "backups")
	run := filepath.Join(target, "apps", policyName, "20261031T030000Z")
	source := Source{Namespace: "apps", Volume: "data"}
	seedFile(t, filepath.Join(dataRoot, "apps", "data", "note.txt"), "hello")
	seedFile(t, filepath.Join(dataRoot, "apps", "data", "assets", "logo.bin"), "1234567890")
	baseline, err := plane.Capture(ctx, run, []Source{source})
	if err != nil {
		t.Fatalf("capture the run: %v", err)
	}
	manifest := Manifest{
		Policy: policyName, Namespace: "apps", Baseline: baseline,
		Volumes:      []Snapshot{{Name: "data", Spec: map[string]any{}}},
		Applications: []Snapshot{{Name: "media", Spec: map[string]any{"volumes": []any{"data"}, "namespace": "apps"}}},
	}
	if err := WriteManifest(run, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	store.putPolicy("apps", policyName, "uid-"+policyName, policyOwner, target, &v1beta1.BackupRecord{
		Succeeded: true, Path: run, Baseline: &baseline,
	})
	return run, baseline
}

// TestRestoreJobsOnOneRunDoNotPair drives two restore jobs over the same run:
// each must submit and receive its own copy, never the other job's outcome
// (C-06).
func TestRestoreJobsOnOneRunDoNotPair(t *testing.T) {
	store := newFakeStore()
	dataRoot := t.TempDir()
	plane := &countingPlane{DataPlane: DirDataPlane{Root: dataRoot}}
	worker := NewLocalWorker(plane)
	seedRestoreRun(t, store, plane, dataRoot, "alice", "nightly")
	store.putRestoreJob("apps", "first", "uid-first", "alice", "nightly", "restored-first")
	store.putRestoreJob("apps", "second", "uid-second", "alice", "nightly", "restored-second")

	controller := NewRestoreController(store, plane, worker, Options{})
	for range 12 {
		reconcileRestore(t, controller, worker, "apps", "first")
		reconcileRestore(t, controller, worker, "apps", "second")
		if terminal(store.storedRestoreJob(t, "apps", "first").Status.Phase) && terminal(store.storedRestoreJob(t, "apps", "second").Status.Phase) {
			break
		}
	}
	for _, name := range []string{"first", "second"} {
		job := store.storedRestoreJob(t, "apps", name)
		if job.Status.Phase != v1beta1.RestorePhaseSucceeded {
			t.Fatalf("restore %s phase = %q (%s), want Succeeded", name, job.Status.Phase, failureText(job))
		}
	}
	if _, copies := plane.counts(); copies != 2 {
		t.Fatalf("copies = %d, want one per job", copies)
	}
}

// TestGlobalOwnerRestoresAnotherAccountsBackup covers the C-59 exemption the
// access layer applies: the global Owner may restore another account's backup,
// while a member may not (C-41, C-59).
func TestGlobalOwnerRestoresAnotherAccountsBackup(t *testing.T) {
	store := newFakeStore()
	dataRoot := t.TempDir()
	plane := &countingPlane{DataPlane: DirDataPlane{Root: dataRoot}}
	worker := NewLocalWorker(plane)
	seedRestoreRun(t, store, plane, dataRoot, "alice", "nightly")
	store.putRestoreJob("apps", "owner-restore", "uid-owner-restore", "root", "nightly", "restored-owner")
	store.putRestoreJob("apps", "member-restore", "uid-member-restore", "bob", "nightly", "restored-member")

	controller := NewRestoreController(store, plane, worker, Options{Owner: "root"})
	for range 12 {
		reconcileRestore(t, controller, worker, "apps", "owner-restore")
		if terminal(store.storedRestoreJob(t, "apps", "owner-restore").Status.Phase) {
			break
		}
	}
	if phase := store.storedRestoreJob(t, "apps", "owner-restore").Status.Phase; phase != v1beta1.RestorePhaseSucceeded {
		t.Fatalf("the global Owner's restore phase = %q (%s), want Succeeded",
			phase, failureText(store.storedRestoreJob(t, "apps", "owner-restore")))
	}

	reconcileRestore(t, controller, worker, "apps", "member-restore")
	member := store.storedRestoreJob(t, "apps", "member-restore")
	if member.Status.Phase != v1beta1.RestorePhaseFailed || !strings.Contains(failureText(member), "全局 Owner") {
		t.Fatalf("a member's cross-owner restore = %q (%s), want a preflight refusal",
			member.Status.Phase, failureText(member))
	}
}

// TestRestoreRefusesForeignObjectInWindow covers the restore window: an object
// with the target name created by someone else after the preflight is reported
// as a failure instead of being adopted (C-41).
func TestRestoreRefusesForeignObjectInWindow(t *testing.T) {
	store := newFakeStore()
	dataRoot := t.TempDir()
	plane := &countingPlane{DataPlane: DirDataPlane{Root: dataRoot}}
	worker := NewLocalWorker(plane)
	seedRestoreRun(t, store, plane, dataRoot, "alice", "nightly")
	store.putRestoreJob("apps", "restore", "uid-restore", "alice", "nightly", "restored")

	controller := NewRestoreController(store, plane, worker, Options{})
	reconcileRestore(t, controller, worker, "apps", "restore") // preflight
	reconcileRestore(t, controller, worker, "apps", "restore") // copy handed off and done
	store.putVolume("apps", "restored-data", "bob")
	for range 6 {
		reconcileRestore(t, controller, worker, "apps", "restore")
		if terminal(store.storedRestoreJob(t, "apps", "restore").Status.Phase) {
			break
		}
	}
	job := store.storedRestoreJob(t, "apps", "restore")
	if job.Status.Phase != v1beta1.RestorePhaseFailed {
		t.Fatalf("restore phase = %q, want Failed", job.Status.Phase)
	}
	if !strings.Contains(failureText(job), "拒绝接管") {
		t.Fatalf("failure %q does not report the foreign object", failureText(job))
	}
}

// TestScheduleLabelValue covers the label a policy's schedule jobs carry: a
// name longer than the 63-character label bound keeps a readable prefix and a
// digest, and two names sharing that prefix stay distinct (C-41).
func TestScheduleLabelValue(t *testing.T) {
	if got := scheduleLabelValue("nightly"); got != "nightly" {
		t.Fatalf("short name changed: %q", got)
	}
	prefix := strings.Repeat("a", 46)
	first := scheduleLabelValue(prefix + "one")
	second := scheduleLabelValue(prefix + "two")
	if first == second {
		t.Fatalf("two long names derived the same label %q", first)
	}
	for _, name := range []string{prefix + "one", prefix + "two", strings.Repeat("b", 63), strings.Repeat("c", 120)} {
		value := scheduleLabelValue(name)
		if len(value) > 63 {
			t.Errorf("label for a %d-character name is %d characters", len(name), len(value))
		}
		if problems := validation.IsValidLabelValue(value); len(problems) > 0 {
			t.Errorf("label %q for name %q is invalid: %v", value, name, problems)
		}
	}
}

// TestLongPolicyNameSchedules covers the schedule path of a policy whose name
// exceeds the label bound: the CronJob carries a valid derived label and the
// run still triggers and records (C-41).
func TestLongPolicyNameSchedules(t *testing.T) {
	store := newFakeStore()
	dataRoot := t.TempDir()
	plane := &countingPlane{DataPlane: DirDataPlane{Root: dataRoot}}
	worker := NewLocalWorker(plane)
	name := strings.Repeat("a", 70)
	target := filepath.Join(t.TempDir(), "backups")
	store.putPolicy("apps", name, "uid-long", "alice", target, nil)
	store.putApplication("apps", "media", "alice", []string{"data"})
	store.putVolume("apps", "data", "alice")
	seedFile(t, filepath.Join(dataRoot, "apps", "data", "note.txt"), "hello")

	controller := NewController(store, worker, Options{WorkerImage: testWorkerImage})
	reconcilePolicy(t, controller, worker, "apps", name)
	cronJob, err := store.lookup("CronJob", "apps", name)
	if err != nil {
		t.Fatalf("the schedule CronJob was not created: %v", err)
	}
	labels, _, err := unstructured.NestedStringMap(cronJob.Object, "spec", "jobTemplate", "metadata", "labels")
	if err != nil {
		t.Fatalf("read the job template labels: %v", err)
	}
	if got := labels[PolicyLabel]; got != scheduleLabelValue(name) {
		t.Fatalf("schedule label = %q, want %q", got, scheduleLabelValue(name))
	}
	if problems := validation.IsValidLabelValue(labels[PolicyLabel]); len(problems) > 0 {
		t.Fatalf("schedule label %q is invalid: %v", labels[PolicyLabel], problems)
	}

	scheduled := time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)
	store.scheduleCronJob(t, "apps", name, scheduled)
	reconcilePolicy(t, controller, worker, "apps", name)
	reconcilePolicy(t, controller, worker, "apps", name)
	policy := store.storedPolicy(t, "apps", name)
	if policy.Status.LastBackup == nil || !policy.Status.LastBackup.Succeeded {
		t.Fatalf("a long-named policy did not record its run: %+v", policy.Status.LastBackup)
	}
}

// terminal reports a restore phase that will not change again.
func terminal(phase string) bool {
	return phase == v1beta1.RestorePhaseSucceeded || phase == v1beta1.RestorePhaseFailed
}

// failureText renders the first failure condition of a restore job.
func failureText(job *v1beta1.RestoreJob) string {
	for _, condition := range job.Status.Conditions {
		if condition.Status == metav1.ConditionTrue && condition.Type != api.ConditionReconciled {
			return condition.Reason + ": " + condition.Message
		}
	}
	return "no failure condition"
}
