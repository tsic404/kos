package migration

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
)

// TestJobState covers the outcome the flow reads from a step Job: a completed
// Job, a failed one with the reason the attempt reports, and a Job the runner
// has not finished (C-40).
func TestJobState(t *testing.T) {
	cases := []struct {
		name       string
		conditions []any
		want       string
		message    string
	}{
		{name: "no conditions is running", want: jobRunning},
		{
			name: "complete",
			conditions: []any{map[string]any{
				"type": "Complete", "status": "True",
			}},
			want: jobComplete,
		},
		{
			name: "failed with a message",
			conditions: []any{map[string]any{
				"type": "Failed", "status": "True", "message": "the target class refuses writes",
			}},
			want:    jobFailed,
			message: "the target class refuses writes",
		},
		{
			name: "failed with only a reason",
			conditions: []any{map[string]any{
				"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded",
			}},
			want:    jobFailed,
			message: "BackoffLimitExceeded",
		},
		{
			name: "an unmet condition is not an outcome",
			conditions: []any{map[string]any{
				"type": "Failed", "status": "False",
			}},
			want: jobRunning,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			job := &unstructured.Unstructured{Object: map[string]any{}}
			if test.conditions != nil {
				job.Object["status"] = map[string]any{"conditions": test.conditions}
			}
			outcome, message := jobState(job)
			if outcome != test.want {
				t.Fatalf("outcome = %q, want %q", outcome, test.want)
			}
			if message != test.message {
				t.Fatalf("message = %q, want %q", message, test.message)
			}
		})
	}
}

// TestExpired covers the downtime window's boundary: the window counts from
// Quiesce, and the attempt fails at the deadline itself, not one round later
// (C-40).
func TestExpired(t *testing.T) {
	started := time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC)
	migration := &v1beta1.VolumeMigration{Spec: v1beta1.VolumeMigrationSpec{
		Window: v1beta1.MigrationWindow{Duration: "30m"},
	}}
	if expired(migration, started) {
		t.Fatal("a migration that never quiesced reports an expired window")
	}
	migration.Status.WindowStartedAt = &metav1.Time{Time: started}
	for _, test := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "inside the window", now: started.Add(29 * time.Minute), want: false},
		{name: "at the deadline", now: started.Add(30 * time.Minute), want: true},
		{name: "past the deadline", now: started.Add(31 * time.Minute), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := expired(migration, test.now); got != test.want {
				t.Fatalf("expired = %v, want %v", got, test.want)
			}
		})
	}
}

// TestExited covers what the flow waits for in Quiesce: a scalable workload
// that asks for zero replicas and reports none ready has exited (C-40, C-66).
func TestExited(t *testing.T) {
	cases := []struct {
		name  string
		spec  map[string]any
		ready int64
		want  bool
	}{
		{name: "no replica count is the rendered count", want: false},
		{name: "one replica is not stopped", spec: map[string]any{"replicas": int64(1)}, want: false},
		{name: "zero replicas is stopped", spec: map[string]any{"replicas": int64(0)}, want: true},
		{name: "zero asked but still ready", spec: map[string]any{"replicas": int64(0)}, ready: 2, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			workload := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "media", "namespace": "apps"},
			}}
			if test.spec != nil {
				workload.Object["spec"] = test.spec
			}
			if test.ready > 0 {
				workload.Object["status"] = map[string]any{"readyReplicas": test.ready}
			}
			if got := exited(workload); got != test.want {
				t.Fatalf("exited = %v, want %v", got, test.want)
			}
		})
	}
}
