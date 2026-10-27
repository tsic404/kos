package app_test

import (
	"strings"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
)

// TestValidateResources covers C-67's refusal rules: a limit below its request
// names the field, and a value that is not a non-negative quantity is refused.
func TestValidateResources(t *testing.T) {
	cases := []struct {
		name      string
		resources *v1beta1.ResourceRequirements
		field     string
	}{
		{
			name:      "a valid set passes",
			resources: &v1beta1.ResourceRequirements{Requests: v1beta1.ResourceList{CPU: "100m", Memory: "128Mi"}, Limits: v1beta1.ResourceList{CPU: "1", Memory: "256Mi"}},
		},
		{
			name:      "a cpu limit below the request names cpu",
			resources: &v1beta1.ResourceRequirements{Requests: v1beta1.ResourceList{CPU: "500m"}, Limits: v1beta1.ResourceList{CPU: "100m"}},
			field:     "spec.resources.limits.cpu",
		},
		{
			name:      "a memory limit below the request names memory",
			resources: &v1beta1.ResourceRequirements{Requests: v1beta1.ResourceList{Memory: "256Mi"}, Limits: v1beta1.ResourceList{Memory: "128Mi"}},
			field:     "spec.resources.limits.memory",
		},
		{
			name:      "an unparseable quantity names its field",
			resources: &v1beta1.ResourceRequirements{Requests: v1beta1.ResourceList{Memory: "lots"}},
			field:     "spec.resources.requests.memory",
		},
		{
			name:      "a negative quantity is refused",
			resources: &v1beta1.ResourceRequirements{Limits: v1beta1.ResourceList{CPU: "-1"}},
			field:     "spec.resources.limits.cpu",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := app.ValidateResources(test.resources, "spec.resources")
			if test.field == "" {
				if err != nil {
					t.Fatalf("valid resources were refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the resource set must be refused")
			}
			if !strings.Contains(err.Error(), test.field) {
				t.Fatalf("refusal %q does not name %s", err.Error(), test.field)
			}
		})
	}
}

// TestMergeResources covers C-67's override rule: a submitted field wins and
// every field it leaves out keeps the base value.
func TestMergeResources(t *testing.T) {
	base := &v1beta1.ResourceRequirements{
		Requests: v1beta1.ResourceList{CPU: "100m", Memory: "128Mi"},
		Limits:   v1beta1.ResourceList{CPU: "500m", Memory: "512Mi"},
	}
	merged := app.MergeResources(base, &v1beta1.ResourceRequirements{
		Limits: v1beta1.ResourceList{CPU: "1"},
	})
	want := v1beta1.ResourceRequirements{
		Requests: v1beta1.ResourceList{CPU: "100m", Memory: "128Mi"},
		Limits:   v1beta1.ResourceList{CPU: "1", Memory: "512Mi"},
	}
	if *merged != want {
		t.Fatalf("merged = %+v, want %+v", *merged, want)
	}
	if app.MergeResources(nil, nil) != nil {
		t.Fatal("two empty sets must merge to nil")
	}
	if got := app.MergeResources(base, nil); got == nil || *got != *base {
		t.Fatalf("a write without resources must keep the base, got %+v", got)
	}
}

// TestResourcesEmpty covers the unlimited marker: a set with no field is empty.
func TestResourcesEmpty(t *testing.T) {
	if !app.ResourcesEmpty(nil) {
		t.Fatal("nil must read as empty")
	}
	if !app.ResourcesEmpty(&v1beta1.ResourceRequirements{Requests: v1beta1.ResourceList{CPU: " "}}) {
		t.Fatal("a blank field must read as empty")
	}
	if app.ResourcesEmpty(&v1beta1.ResourceRequirements{Limits: v1beta1.ResourceList{Memory: "1Gi"}}) {
		t.Fatal("a declared limit is not empty")
	}
}
