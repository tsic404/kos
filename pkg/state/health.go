package state

import "github.com/tsic404/kos/api/v1beta1"

// HealthInput is the observed fact set one health evaluation reads (C-08).
// Callers pass the object's current facts; no field is inferred from phase.
type HealthInput struct {
	// IsObserved is false when the object, or the node that hosts it, cannot
	// be observed.
	IsObserved bool
	// EffectiveDesiredReplicas is the desired replica count after the pause
	// and maintenance rules of C-66; a stopped object has zero here.
	EffectiveDesiredReplicas int32
	// ReadyReplicas is the number of ready replicas.
	ReadyReplicas int32
	// IsStopped reports a pause or a maintenance stop, which makes the object
	// healthy at zero effective replicas.
	IsStopped bool
	// IsJobOnly marks an object whose workload consists of Jobs; its health is
	// read from the Job conditions instead of replica counts.
	IsJobOnly bool
	// JobState is the health derived from the Job conditions for a job-only
	// object; empty means the conditions were not observed.
	JobState v1beta1.HealthState
	// HasUnavailableRequiredDependency reports a required dependency that is
	// declared by the definition and unavailable now.
	HasUnavailableRequiredDependency bool
	// HasDegradedOptionalDependency reports a declared optional dependency
	// that is degraded now.
	HasDegradedOptionalDependency bool
	// HasLostRequiredVolume reports one of the object's required volumes lost.
	HasLostRequiredVolume bool
	// HasDriftedDownstream reports a downstream object changed outside KOS.
	HasDriftedDownstream bool
	// HasInsufficientCapability reports a required capability guarantee that
	// is not met.
	HasInsufficientCapability bool
}

// EvaluateHealth derives the single health value of one observed state. The
// priority is Unknown, then Unavailable, then Degraded, then Healthy, and an
// unobservable object is never inferred healthy (C-08).
func EvaluateHealth(input HealthInput) v1beta1.HealthState {
	if !input.IsObserved {
		return v1beta1.HealthStateUnknown
	}
	if input.IsJobOnly {
		if input.JobState == "" {
			return v1beta1.HealthStateUnknown
		}
		return input.JobState
	}
	if input.HasLostRequiredVolume || input.HasUnavailableRequiredDependency {
		return v1beta1.HealthStateUnavailable
	}
	if input.EffectiveDesiredReplicas > 0 && input.ReadyReplicas == 0 {
		return v1beta1.HealthStateUnavailable
	}
	if input.EffectiveDesiredReplicas == 0 {
		if input.IsStopped {
			return v1beta1.HealthStateHealthy
		}
		return v1beta1.HealthStateDegraded
	}
	if input.ReadyReplicas < input.EffectiveDesiredReplicas {
		return v1beta1.HealthStateDegraded
	}
	if input.HasDegradedOptionalDependency || input.HasDriftedDownstream || input.HasInsufficientCapability {
		return v1beta1.HealthStateDegraded
	}
	return v1beta1.HealthStateHealthy
}

// EffectiveDesiredReplicas applies the stop rules of C-66 without touching the
// spec value: a paused or maintenance-stopped object desires zero replicas.
func EffectiveDesiredReplicas(specReplicas int32, isPaused, isMaintenanceStopped bool) int32 {
	if isPaused || isMaintenanceStopped {
		return 0
	}
	return specReplicas
}
