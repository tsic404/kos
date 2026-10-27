package volume

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
)

// TestPlanForAnswersThreeQuestions pins C-38's expansion preflight: the backend
// and the filesystem each answer yes or no, an unanswerable question requires
// downtime, and an impossible expansion is never promised online.
func TestPlanForAnswersThreeQuestions(t *testing.T) {
	cases := []struct {
		name             string
		delta            string
		class            *storageClass
		backendOnline    bool
		filesystemOnline bool
		downtime         bool
		minimumEstimate  time.Duration
	}{
		{
			name:     "no class answers nothing",
			delta:    "1Gi",
			class:    nil,
			downtime: true,
		},
		{
			name:             "backend and filesystem both grow online",
			delta:            "1Gi",
			class:            &storageClass{AllowVolumeExpansion: true, Parameters: map[string]string{"fsType": "ext4"}},
			backendOnline:    true,
			filesystemOnline: true,
			minimumEstimate:  30 * time.Second,
		},
		{
			name:             "backend refuses expansion",
			delta:            "1Gi",
			class:            &storageClass{AllowVolumeExpansion: false, Parameters: map[string]string{"fsType": "xfs"}},
			filesystemOnline: true,
			downtime:         true,
		},
		{
			name:          "filesystem is not known to grow online",
			delta:         "1Gi",
			class:         &storageClass{AllowVolumeExpansion: true, Parameters: map[string]string{"fsType": "nfs"}},
			backendOnline: true,
			downtime:      true,
		},
		{
			name:          "the class carries no filesystem type",
			delta:         "1Gi",
			class:         &storageClass{AllowVolumeExpansion: true},
			backendOnline: true,
			downtime:      true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			plan := planFor(resource.MustParse(testCase.delta), testCase.class)
			if plan.BackendOnline != testCase.backendOnline {
				t.Fatalf("backendOnline = %v, want %v", plan.BackendOnline, testCase.backendOnline)
			}
			if plan.FilesystemOnline != testCase.filesystemOnline {
				t.Fatalf("filesystemOnline = %v, want %v", plan.FilesystemOnline, testCase.filesystemOnline)
			}
			if plan.DowntimeRequired != testCase.downtime {
				t.Fatalf("downtimeRequired = %v, want %v", plan.DowntimeRequired, testCase.downtime)
			}
			if testCase.minimumEstimate > 0 && plan.EstimatedDuration < testCase.minimumEstimate {
				t.Fatalf("estimate = %s, want at least %s", plan.EstimatedDuration, testCase.minimumEstimate)
			}
			if plan.DowntimeRequired && plan.EstimatedDuration != 0 {
				t.Fatalf("an unknown estimate must stay zero, got %s", plan.EstimatedDuration)
			}
		})
	}
}

// TestValidateSpec pins the required shape of C-38: mobility has no default and
// capacity must be a positive quantity.
func TestValidateSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    v1beta1.VolumeSpec
		problem string
	}{
		{name: "mobility missing", spec: v1beta1.VolumeSpec{Capacity: "1Gi"}, problem: "mobility"},
		{name: "mobility unknown", spec: v1beta1.VolumeSpec{Mobility: "local", Capacity: "1Gi"}, problem: "not one of"},
		{name: "capacity missing", spec: v1beta1.VolumeSpec{Mobility: v1beta1.VolumeMobilityNodeLocal}, problem: "capacity is required"},
		{name: "capacity unreadable", spec: v1beta1.VolumeSpec{Mobility: v1beta1.VolumeMobilityNodeLocal, Capacity: "big"}, problem: "not a quantity"},
		{name: "capacity zero", spec: v1beta1.VolumeSpec{Mobility: v1beta1.VolumeMobilityNodeLocal, Capacity: "0"}, problem: "positive"},
		{name: "node local", spec: v1beta1.VolumeSpec{Mobility: v1beta1.VolumeMobilityNodeLocal, Capacity: "10Gi"}},
		{name: "cluster movable", spec: v1beta1.VolumeSpec{Mobility: v1beta1.VolumeMobilityClusterMovable, Capacity: "10Gi"}},
		{name: "shared rwx", spec: v1beta1.VolumeSpec{Mobility: v1beta1.VolumeMobilitySharedRWX, Capacity: "10Gi"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, problem := validateSpec(testCase.spec)
			if testCase.problem == "" {
				if problem != "" {
					t.Fatalf("spec was refused: %s", problem)
				}
				return
			}
			if problem == "" {
				t.Fatal("spec was accepted, want a refusal")
			}
			if !strings.Contains(problem, testCase.problem) {
				t.Fatalf("problem = %q, want it to name %q", problem, testCase.problem)
			}
		})
	}
}

// TestBindingNodeOf pins the node a node-local volume is pinned to: the hostname
// affinity of its persistent volume (C-38).
func TestBindingNodeOf(t *testing.T) {
	pv := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"nodeAffinity": map[string]any{
				"required": map[string]any{
					"nodeSelectorTerms": []any{map[string]any{
						"matchExpressions": []any{map[string]any{
							"key":      hostnameLabel,
							"operator": "In",
							"values":   []any{"node-a"},
						}},
					}},
				},
			},
		},
	}}
	if node := bindingNodeOf(pv); node != "node-a" {
		t.Fatalf("binding node = %q, want node-a", node)
	}
	if node := bindingNodeOf(&unstructured.Unstructured{Object: map[string]any{}}); node != "" {
		t.Fatalf("a volume without affinity names node %q", node)
	}
}
