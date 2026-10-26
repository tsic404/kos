package state_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/state"
)

func TestReconciledThreeValues(t *testing.T) {
	applied := state.Applied(4, "desired state applied")
	blocked := state.Blocked(4, "NamespaceMissing", "namespace web does not exist")
	unobserved := state.Unobserved(4, "the object could not be read")

	for _, test := range []struct {
		name       string
		condition  metav1.Condition
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{"applied", applied, metav1.ConditionTrue, state.ReasonApplied},
		{"blocked", blocked, metav1.ConditionFalse, "NamespaceMissing"},
		{"unobserved", unobserved, metav1.ConditionUnknown, state.ReasonNotObserved},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.condition.Type != api.ConditionReconciled {
				t.Fatalf("type = %q, want %q", test.condition.Type, api.ConditionReconciled)
			}
			if test.condition.Status != test.wantStatus {
				t.Errorf("status = %q, want %q", test.condition.Status, test.wantStatus)
			}
			if test.condition.Reason != test.wantReason {
				t.Errorf("reason = %q, want %q", test.condition.Reason, test.wantReason)
			}
			if test.condition.ObservedGeneration != 4 {
				t.Errorf("observedGeneration = %d, want 4", test.condition.ObservedGeneration)
			}
		})
	}
}

func TestReconciledOfMissingConditionIsUnknown(t *testing.T) {
	condition := state.ReconciledOf(nil)
	if condition.Type != api.ConditionReconciled {
		t.Fatalf("type = %q, want %q", condition.Type, api.ConditionReconciled)
	}
	if condition.Status != metav1.ConditionUnknown {
		t.Fatalf("status = %q, want Unknown", condition.Status)
	}
	if condition.Reason != state.ReasonNotObserved {
		t.Fatalf("reason = %q, want %q", condition.Reason, state.ReasonNotObserved)
	}
}

func TestSetPreservesTransitionTimeWhileTheStatusHolds(t *testing.T) {
	first := state.Set(nil, state.Applied(1, "first"))
	second := state.Set(first, state.Applied(2, "second"))

	if !second[0].LastTransitionTime.Time.Equal(first[0].LastTransitionTime.Time) {
		t.Errorf("transition time moved from %v to %v", first[0].LastTransitionTime, second[0].LastTransitionTime)
	}
	if second[0].Message != "second" {
		t.Errorf("message = %q, want %q", second[0].Message, "second")
	}
	if second[0].ObservedGeneration != 2 {
		t.Errorf("observedGeneration = %d, want 2", second[0].ObservedGeneration)
	}
}

func TestSetMovesTransitionTimeWhenTheStatusChanges(t *testing.T) {
	previous := metav1.NewTime(time.Unix(0, 0))
	conditions := []metav1.Condition{{
		Type:               api.ConditionReconciled,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: previous,
	}}
	updated := state.Set(conditions, state.Applied(1, "done"))

	if updated[0].Status != metav1.ConditionTrue {
		t.Fatalf("status = %q, want True", updated[0].Status)
	}
	if !updated[0].LastTransitionTime.After(previous.Time) {
		t.Errorf("transition time = %v, want later than %v", updated[0].LastTransitionTime, previous)
	}
}

func TestFailureConditionIsSeparateFromReconciled(t *testing.T) {
	conditions := state.Set(nil, state.Blocked(3, "NamespaceMissing", "namespace web does not exist"))
	conditions = state.Set(conditions, state.Failure("NamespaceMissing", 3, "NamespaceMissing", "namespace web does not exist"))

	reconciled := state.ReconciledOf(conditions)
	if reconciled.Status != metav1.ConditionFalse || reconciled.Reason != "NamespaceMissing" {
		t.Fatalf("reconciled = %v/%v, want False/NamespaceMissing", reconciled.Status, reconciled.Reason)
	}
	failure, ok := state.Find(conditions, "NamespaceMissing")
	if !ok {
		t.Fatal("failure condition missing")
	}
	if failure.Status != metav1.ConditionTrue {
		t.Errorf("failure status = %q, want True", failure.Status)
	}
	if len(conditions) != 2 {
		t.Errorf("conditions = %d, want 2 separate entries", len(conditions))
	}
}
