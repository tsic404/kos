package state_test

import (
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/state"
)

func TestEvaluateHealth(t *testing.T) {
	tests := []struct {
		name   string
		input  state.HealthInput
		expect v1beta1.HealthState
	}{
		{
			name:   "unobservable is unknown",
			input:  state.HealthInput{IsObserved: false, EffectiveDesiredReplicas: 2, ReadyReplicas: 2},
			expect: v1beta1.HealthStateUnknown,
		},
		{
			name:   "unknown outranks unavailable",
			input:  state.HealthInput{IsObserved: false, HasLostRequiredVolume: true},
			expect: v1beta1.HealthStateUnknown,
		},
		{
			name:   "all replicas ready is healthy",
			input:  state.HealthInput{IsObserved: true, EffectiveDesiredReplicas: 2, ReadyReplicas: 2},
			expect: v1beta1.HealthStateHealthy,
		},
		{
			name:   "zero ready replicas is unavailable",
			input:  state.HealthInput{IsObserved: true, EffectiveDesiredReplicas: 2},
			expect: v1beta1.HealthStateUnavailable,
		},
		{
			name: "an unavailable required dependency is unavailable",
			input: state.HealthInput{
				IsObserved:                       true,
				EffectiveDesiredReplicas:         2,
				ReadyReplicas:                    2,
				HasUnavailableRequiredDependency: true,
			},
			expect: v1beta1.HealthStateUnavailable,
		},
		{
			name: "a lost required volume is unavailable",
			input: state.HealthInput{
				IsObserved:               true,
				EffectiveDesiredReplicas: 2,
				ReadyReplicas:            2,
				HasLostRequiredVolume:    true,
			},
			expect: v1beta1.HealthStateUnavailable,
		},
		{
			name:   "partly ready replicas are degraded",
			input:  state.HealthInput{IsObserved: true, EffectiveDesiredReplicas: 3, ReadyReplicas: 1},
			expect: v1beta1.HealthStateDegraded,
		},
		{
			name: "a degraded optional dependency is degraded",
			input: state.HealthInput{
				IsObserved:                    true,
				EffectiveDesiredReplicas:      2,
				ReadyReplicas:                 2,
				HasDegradedOptionalDependency: true,
			},
			expect: v1beta1.HealthStateDegraded,
		},
		{
			name: "downstream drift is degraded",
			input: state.HealthInput{
				IsObserved:               true,
				EffectiveDesiredReplicas: 2,
				ReadyReplicas:            2,
				HasDriftedDownstream:     true,
			},
			expect: v1beta1.HealthStateDegraded,
		},
		{
			name: "an insufficient capability guarantee is degraded",
			input: state.HealthInput{
				IsObserved:                true,
				EffectiveDesiredReplicas:  2,
				ReadyReplicas:             2,
				HasInsufficientCapability: true,
			},
			expect: v1beta1.HealthStateDegraded,
		},
		{
			name:   "a stopped object at zero replicas is healthy",
			input:  state.HealthInput{IsObserved: true, EffectiveDesiredReplicas: 0, IsStopped: true},
			expect: v1beta1.HealthStateHealthy,
		},
		{
			name:   "a running object scaled to zero is degraded",
			input:  state.HealthInput{IsObserved: true, EffectiveDesiredReplicas: 0},
			expect: v1beta1.HealthStateDegraded,
		},
		{
			name: "unavailable outranks degraded",
			input: state.HealthInput{
				IsObserved:               true,
				EffectiveDesiredReplicas: 2,
				ReadyReplicas:            2,
				HasLostRequiredVolume:    true,
				HasDriftedDownstream:     true,
			},
			expect: v1beta1.HealthStateUnavailable,
		},
		{
			name: "a job-only object follows its job conditions",
			input: state.HealthInput{
				IsObserved:               true,
				IsJobOnly:                true,
				JobState:                 v1beta1.HealthStateUnavailable,
				EffectiveDesiredReplicas: 3,
				ReadyReplicas:            3,
			},
			expect: v1beta1.HealthStateUnavailable,
		},
		{
			name: "a job-only object without observed conditions is unknown",
			input: state.HealthInput{
				IsObserved: true,
				IsJobOnly:  true,
			},
			expect: v1beta1.HealthStateUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := state.EvaluateHealth(test.input); got != test.expect {
				t.Fatalf("EvaluateHealth() = %q, want %q", got, test.expect)
			}
		})
	}
}

func TestEffectiveDesiredReplicas(t *testing.T) {
	tests := []struct {
		name                 string
		specReplicas         int32
		isPaused             bool
		isMaintenanceStopped bool
		expect               int32
	}{
		{"running keeps the spec value", 3, false, false, 3},
		{"paused desires zero", 3, true, false, 0},
		{"maintenance stop desires zero", 3, false, true, 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := state.EffectiveDesiredReplicas(test.specReplicas, test.isPaused, test.isMaintenanceStopped)
			if got != test.expect {
				t.Fatalf("EffectiveDesiredReplicas() = %d, want %d", got, test.expect)
			}
		})
	}
}
