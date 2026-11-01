package kossettings

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// usageFunc adapts a function to UsageSource.
type usageFunc func(ctx context.Context) ([]NodeUsage, error)

// NodeUsage implements UsageSource.
func (f usageFunc) NodeUsage(ctx context.Context) ([]NodeUsage, error) { return f(ctx) }

// TestNodeUsageStatesWhyItCannotRead checks that a reading KOS cannot trust is
// withheld with a reason instead of being reported as zero use: no metric
// source, a failed read and an unparseable sample each leave it unavailable
// (C-36, C-53).
func TestNodeUsageStatesWhyItCannotRead(t *testing.T) {
	cases := []struct {
		name          string
		usage         UsageSource
		wantAvailable bool
	}{
		{"no metric source", nil, false},
		{"read failure", usageFunc(func(context.Context) ([]NodeUsage, error) {
			return nil, errors.New("metric source down")
		}), false},
		{"unparseable cpu", usageFunc(func(context.Context) ([]NodeUsage, error) {
			return []NodeUsage{{Name: "node-a", CPU: "not-a-quantity", Memory: "1Gi"}}, nil
		}), false},
		{"unparseable memory", usageFunc(func(context.Context) ([]NodeUsage, error) {
			return []NodeUsage{{Name: "node-a", CPU: "1", Memory: "not-a-quantity"}}, nil
		}), false},
		{"sample without a node", usageFunc(func(context.Context) ([]NodeUsage, error) {
			return []NodeUsage{{CPU: "1", Memory: "1Gi"}}, nil
		}), true},
		{"samples", usageFunc(func(context.Context) ([]NodeUsage, error) {
			return []NodeUsage{{Name: "node-a", CPU: "1", Memory: "1Gi"}}, nil
		}), true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			readings := &Readings{Usage: testCase.usage}
			usage, reason := readings.nodeUsage(context.Background())
			if !testCase.wantAvailable {
				if reason == "" {
					t.Fatalf("reason is empty, want the item to state why it cannot read")
				}
				if len(usage) != 0 {
					t.Fatalf("usage = %v, want no figure beside the reason", usage)
				}
				return
			}
			if reason != "" {
				t.Fatalf("reason = %q, want the reading available", reason)
			}
			if sample, ok := usage["node-a"]; ok && sample.CPU != "1" {
				t.Fatalf("node-a sample = %+v, want the reported cpu", sample)
			}
		})
	}
}

// TestMissingSamples checks the coverage rule the cluster total depends on: an
// empty sample set covers no node, a partial set names the uncovered node, and
// a full set names none (C-36).
func TestMissingSamples(t *testing.T) {
	nodes := []corev1.Node{{}, {}}
	nodes[0].Name = "node-a"
	nodes[1].Name = "node-b"
	cases := []struct {
		name  string
		usage map[string]NodeUsage
		want  []string
	}{
		{"empty sample set", map[string]NodeUsage{}, []string{"node-a", "node-b"}},
		{"one node unsampled", map[string]NodeUsage{"node-a": {Name: "node-a", CPU: "1", Memory: "1Gi"}}, []string{"node-b"}},
		{"every node sampled", map[string]NodeUsage{
			"node-a": {Name: "node-a", CPU: "1", Memory: "1Gi"},
			"node-b": {Name: "node-b", CPU: "2", Memory: "2Gi"},
		}, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			missing := missingSamples(nodes, testCase.usage)
			if len(missing) != len(testCase.want) {
				t.Fatalf("missing = %v, want %v", missing, testCase.want)
			}
			for index, want := range testCase.want {
				if missing[index] != want {
					t.Fatalf("missing = %v, want %v", missing, testCase.want)
				}
			}
		})
	}
}
