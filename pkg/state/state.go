// Package state implements the two status axes shared by every object with a
// status writer: the three-valued Reconciled condition and health aggregation
// (C-07, C-08). Phase stays the object's own flow stage; nothing here derives
// one axis from the other.
package state

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api"
)

// Reasons this package writes itself. The reason of a blocked condition and of
// a failure condition belongs to the object's own contract.
const (
	// ReasonApplied is the reason of Reconciled=True.
	ReasonApplied = "Applied"
	// ReasonNotObserved is the reason of Reconciled=Unknown when no writer has
	// observed the object yet.
	ReasonNotObserved = "NotObserved"
)

// Applied returns Reconciled=True for a round that applied the whole desired
// state and left no pending action (C-07).
func Applied(observedGeneration int64, message string) metav1.Condition {
	return Condition(api.ConditionReconciled, metav1.ConditionTrue, observedGeneration, ReasonApplied, message)
}

// Blocked returns Reconciled=False with the reason that keeps the object open:
// pending work or a dependency that is not met (C-06, C-07).
func Blocked(observedGeneration int64, reason, message string) metav1.Condition {
	return Condition(api.ConditionReconciled, metav1.ConditionFalse, observedGeneration, reason, message)
}

// Unobserved returns Reconciled=Unknown for an object its writer cannot observe (C-07).
func Unobserved(observedGeneration int64, message string) metav1.Condition {
	return Condition(api.ConditionReconciled, metav1.ConditionUnknown, observedGeneration, ReasonNotObserved, message)
}

// Condition builds a condition of any type. Failure conditions keep their own
// type and never stand in for Reconciled (C-07).
func Condition(conditionType string, status metav1.ConditionStatus, observedGeneration int64, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: observedGeneration,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
}

// Failure builds a failure condition: a condition of its own type whose True
// value reports the failure (C-07).
func Failure(conditionType string, observedGeneration int64, reason, message string) metav1.Condition {
	return Condition(conditionType, metav1.ConditionTrue, observedGeneration, reason, message)
}

// Find returns the condition of one type.
func Find(conditions []metav1.Condition, conditionType string) (metav1.Condition, bool) {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}

// ReconciledOf returns the Reconciled condition. An object without one has not
// been observed by its writer, so the value is Unknown (C-07).
func ReconciledOf(conditions []metav1.Condition) metav1.Condition {
	if condition, ok := Find(conditions, api.ConditionReconciled); ok {
		return condition
	}
	return metav1.Condition{
		Type:    api.ConditionReconciled,
		Status:  metav1.ConditionUnknown,
		Reason:  ReasonNotObserved,
		Message: "no writer has observed the object",
	}
}

// Set applies condition to the list. The transition time moves only when the
// status changes, so an unchanged round produces an unchanged condition (C-07).
func Set(conditions []metav1.Condition, condition metav1.Condition) []metav1.Condition {
	meta.SetStatusCondition(&conditions, condition)
	return conditions
}
