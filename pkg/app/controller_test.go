package app

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
)

// workload builds one stored workload object with its observed replica counts.
func workload(kind string, desired, ready int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       kind,
		"spec":       map[string]any{"replicas": desired},
		"status":     map[string]any{"readyReplicas": ready},
	}}
}

// TestApplicationHealthReadsObservedStatus covers C-08: health follows the
// stored workload status, not the desired documents a render produces.
func TestApplicationHealthReadsObservedStatus(t *testing.T) {
	cases := []struct {
		name       string
		objects    []*unstructured.Unstructured
		comparison CapabilityResult
		want       v1beta1.HealthState
	}{
		{
			name:    "all replicas ready",
			objects: []*unstructured.Unstructured{workload("Deployment", 2, 2)},
			want:    v1beta1.HealthStateHealthy,
		},
		{
			name:    "no replica ready",
			objects: []*unstructured.Unstructured{workload("Deployment", 2, 0)},
			want:    v1beta1.HealthStateUnavailable,
		},
		{
			name:    "part of the replicas ready",
			objects: []*unstructured.Unstructured{workload("Deployment", 2, 1)},
			want:    v1beta1.HealthStateDegraded,
		},
		{
			name: "workload status not observed yet",
			objects: []*unstructured.Unstructured{{Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"spec":       map[string]any{"replicas": int64(2)},
				"status":     map[string]any{},
			}}},
			want: v1beta1.HealthStateUnknown,
		},
		{
			name:       "a guarantee is not met",
			objects:    []*unstructured.Unstructured{workload("Deployment", 1, 1)},
			comparison: CapabilityResult{Unsatisfied: []string{"storage.block.snapshot"}},
			want:       v1beta1.HealthStateDegraded,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := applicationHealth(test.objects, test.comparison, false); got != test.want {
				t.Fatalf("health = %q, want %q", got, test.want)
			}
		})
	}
}

// recordStore serves a fixed record set and records deletions.
type recordStore struct {
	Store
	objects []unstructured.Unstructured
	deleted []string
}

// List returns the fixed record set.
func (s *recordStore) List(context.Context, schema.GroupVersionResource, string, string) ([]unstructured.Unstructured, error) {
	return s.objects, nil
}

// Delete records the consumed record.
func (s *recordStore) Delete(_ context.Context, _ string, ref write.Ref) error {
	s.deleted = append(s.deleted, ref.Name)
	return nil
}

// TestPendingActionIgnoresTheAbandonmentManifest covers C-04/C-33: only a
// one-shot action record bound to this application is consumed; the immutable
// abandonment manifest and records for other applications are left alone.
func TestPendingActionIgnoresTheAbandonmentManifest(t *testing.T) {
	application := &v1beta1.Application{ObjectMeta: metav1.ObjectMeta{
		Name: "web", Namespace: "default", UID: "uid-1",
	}}
	record := func(name, owner, target string) unstructured.Unstructured {
		return unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name": name,
				"labels": map[string]any{
					ActionLabel:      ActionOrphan,
					ActionOwnerLabel: owner,
				},
			},
			"data": map[string]any{ActionDataKey: target},
		}}
	}
	manifest := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": AbandonmentName("uid-1"),
			"labels": map[string]any{
				ActionOwnerLabel:       "uid-1",
				write.AbandonmentLabel: "uid-1",
			},
		},
	}}

	store := &recordStore{objects: []unstructured.Unstructured{
		manifest,
		record("kos-action-uid-1-aaa", "uid-1", "other/name"),
		record(ActionRecordName("uid-1", ActionOrphan), "uid-1", "default/web"),
	}}
	action, got, err := NewController(store, Options{}).pendingAction(context.Background(), application)
	if err != nil {
		t.Fatalf("pending action: %v", err)
	}
	if action != ActionOrphan || got == nil || got.GetName() != ActionRecordName("uid-1", ActionOrphan) {
		t.Fatalf("pending action = %q/%v, want the application's orphan record", action, got)
	}
}

// TestBlockedStatusKeepsPhaseAndBudget covers the C-33 rule this change
// restores: a prerequisite that disappears after the application started does
// not roll the phase back and does not hand out a fresh retry budget.
func TestBlockedStatusKeepsPhaseAndBudget(t *testing.T) {
	next := metav1.NewTime(time.Unix(0, 0))
	running := &v1beta1.Application{Status: v1beta1.ApplicationStatus{
		ObjectStatus: v1beta1.ObjectStatus{
			Phase:  PhaseReady,
			Health: v1beta1.Health{State: v1beta1.HealthStateHealthy},
		},
		RetryCount:  3,
		NextRetryAt: &next,
	}}
	status := blockedStatus(running, ConditionDefinitionMissing, "the definition is gone")
	if status.Phase != PhaseReady {
		t.Fatalf("phase = %q, want %q", status.Phase, PhaseReady)
	}
	if status.RetryCount != 3 || status.NextRetryAt == nil {
		t.Fatalf("retry facts = %d/%v, want 3 and the schedule kept", status.RetryCount, status.NextRetryAt)
	}
	if status.Health.State != v1beta1.HealthStateHealthy {
		t.Fatalf("health = %q, want the observed %q", status.Health.State, v1beta1.HealthStateHealthy)
	}
	fresh := &v1beta1.Application{}
	if status := blockedStatus(fresh, ConditionNamespaceMissing, "the namespace is gone"); status.Phase != PhasePending {
		t.Fatalf("a first block wrote phase %q, want %q", status.Phase, PhasePending)
	}
}

// statusStore captures the status a round writes.
type statusStore struct {
	Store
	written *v1beta1.ApplicationStatus
}

// ApplyStatus records the submitted status.
func (s *statusStore) ApplyStatus(_ context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	raw, found, err := unstructured.NestedMap(document.Object, "status")
	if err != nil || !found {
		return document, nil
	}
	status := v1beta1.ApplicationStatus{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &status); err != nil {
		return document, nil
	}
	s.written = &status
	return document, nil
}

// TestRecordFailureKeepsReadyPhaseAndObservedHealth covers C-33/C-08: a failed
// re-apply on a ready application does not roll the phase back and does not
// overwrite the observed health, and it does not spend the retry budget.
func TestRecordFailureKeepsReadyPhaseAndObservedHealth(t *testing.T) {
	store := &statusStore{}
	now := time.Unix(1_700_000_000, 0)
	controller := NewController(store, Options{Clock: func() time.Time { return now }})
	application := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Status: v1beta1.ApplicationStatus{
			ObjectStatus: v1beta1.ObjectStatus{
				Phase:  PhaseReady,
				Health: v1beta1.Health{State: v1beta1.HealthStateHealthy},
			},
			RetryCount: 2,
		},
	}
	result, err := controller.recordFailure(context.Background(), application, now, "the downstream write failed")
	if err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("a failed re-apply must schedule another round")
	}
	if result.RequeueAfter != readyResync {
		t.Fatalf("requeue = %v, want the ready resync %v", result.RequeueAfter, readyResync)
	}
	if store.written == nil {
		t.Fatal("no status was written")
	}
	if store.written.Phase != PhaseReady {
		t.Fatalf("phase = %q, want %q", store.written.Phase, PhaseReady)
	}
	if store.written.Health.State != v1beta1.HealthStateHealthy {
		t.Fatalf("health = %q, want the observed %q", store.written.Health.State, v1beta1.HealthStateHealthy)
	}
	if store.written.RetryCount != 2 {
		t.Fatalf("retryCount = %d, want 2 (a ready application spends no budget)", store.written.RetryCount)
	}
	failure, found := state.Find(store.written.Conditions, ConditionRetrying)
	if !found || failure.Status != metav1.ConditionTrue {
		t.Fatalf("the retrying condition is missing or not True: %v", store.written.Conditions)
	}
}

// TestFailKeepsReadyPhaseAndObservedHealth covers C-33/C-08 on the
// non-retryable path: a ready application whose re-render hits an unfixable
// error keeps its phase and observed health and only gains the failure
// condition; an application that never became ready still ends in Failed.
func TestFailKeepsReadyPhaseAndObservedHealth(t *testing.T) {
	store := &statusStore{}
	controller := NewController(store, Options{})
	running := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Status: v1beta1.ApplicationStatus{
			ObjectStatus: v1beta1.ObjectStatus{
				Phase:  PhaseReady,
				Health: v1beta1.Health{State: v1beta1.HealthStateHealthy},
			},
			RetryCount: 2,
		},
	}
	result, err := controller.fail(context.Background(), running, ConditionInvalidPackage, "the package became invalid")
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if store.written == nil {
		t.Fatal("no status was written")
	}
	if store.written.Phase != PhaseReady {
		t.Fatalf("phase = %q, want %q", store.written.Phase, PhaseReady)
	}
	if store.written.Health.State != v1beta1.HealthStateHealthy {
		t.Fatalf("health = %q, want the observed %q", store.written.Health.State, v1beta1.HealthStateHealthy)
	}
	failure, found := state.Find(store.written.Conditions, ConditionInvalidPackage)
	if !found || failure.Status != metav1.ConditionTrue {
		t.Fatalf("the failure condition is missing or not True: %v", store.written.Conditions)
	}
	if result.RequeueAfter != readyResync {
		t.Fatalf("requeue = %v, want the ready resync %v", result.RequeueAfter, readyResync)
	}

	if _, err := controller.fail(context.Background(), &v1beta1.Application{}, ConditionInvalidPackage, "broken"); err != nil {
		t.Fatalf("fail on a pending application: %v", err)
	}
	if store.written.Phase != PhaseFailed {
		t.Fatalf("a pending application wrote phase %q, want %q", store.written.Phase, PhaseFailed)
	}
}

// job builds one stored batch Job with the given conditions.
func job(conditions ...map[string]any) *unstructured.Unstructured {
	status := map[string]any{}
	if len(conditions) > 0 {
		items := make([]any, 0, len(conditions))
		for _, condition := range conditions {
			items = append(items, condition)
		}
		status["conditions"] = items
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"status":     status,
	}}
}

// TestApplicationHealthReadsJobConditions covers C-11's job-only rule: a
// package that renders only Jobs is judged by the Job conditions, not by the
// replica aggregate.
func TestApplicationHealthReadsJobConditions(t *testing.T) {
	cases := []struct {
		name       string
		objects    []*unstructured.Unstructured
		comparison CapabilityResult
		want       v1beta1.HealthState
	}{
		{
			name:    "complete job",
			objects: []*unstructured.Unstructured{job(map[string]any{"type": "Complete", "status": "True"})},
			want:    v1beta1.HealthStateHealthy,
		},
		{
			name:    "failed job",
			objects: []*unstructured.Unstructured{job(map[string]any{"type": "Failed", "status": "True"})},
			want:    v1beta1.HealthStateUnavailable,
		},
		{
			name:    "running job",
			objects: []*unstructured.Unstructured{job(map[string]any{"type": "Complete", "status": "False"})},
			want:    v1beta1.HealthStateDegraded,
		},
		{
			name:    "job without status",
			objects: []*unstructured.Unstructured{job()},
			want:    v1beta1.HealthStateUnknown,
		},
		{
			name:       "complete job with an unmet guarantee",
			objects:    []*unstructured.Unstructured{job(map[string]any{"type": "Complete", "status": "True"})},
			comparison: CapabilityResult{Unsatisfied: []string{"storage.block.snapshot"}},
			want:       v1beta1.HealthStateDegraded,
		},
		{
			name: "a replica workload decides instead",
			objects: []*unstructured.Unstructured{
				job(map[string]any{"type": "Failed", "status": "True"}),
				workload("Deployment", 1, 1),
			},
			want: v1beta1.HealthStateHealthy,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := applicationHealth(test.objects, test.comparison, false); got != test.want {
				t.Fatalf("health = %q, want %q", got, test.want)
			}
		})
	}
}

// TestPermanentCoversUnservedResources covers the C-33 budget rule: a write the
// cluster cannot serve fails at once instead of spending all five attempts.
func TestPermanentCoversUnservedResources(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "denied write", err: &write.Denied{Subject: write.SubjectApplicationReconciler, Reason: "no"}, want: true},
		{name: "unknown resource", err: &write.UnknownResource{Ref: write.Ref{Group: "example.com", Kind: "Widget"}}, want: true},
		{name: "unserved resource", err: apierrors.NewNotFound(schema.GroupResource{Group: "example.com", Resource: "widgets"}, "one"), want: true},
		{name: "transient failure", err: errors.New("connection reset by peer")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := permanent(test.err); got != test.want {
				t.Fatalf("permanent = %v, want %v", got, test.want)
			}
		})
	}
}
