package app_test

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
)

// TestConvergeReplicas covers the pause and scale rules of C-66 on the rendered
// workloads: a stop drives the count to zero without a spec write, and a
// written count replaces the package's value.
func TestConvergeReplicas(t *testing.T) {
	cases := []struct {
		name     string
		paused   bool
		replicas *int32
		want     int64
	}{
		{name: "paused drives to zero", paused: true, replicas: new(int32(4)), want: 0},
		{name: "written count converges", replicas: new(int32(3)), want: 3},
		{name: "absent count keeps the render", want: 2},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			deployment := renderedWorkload("Deployment", 2)
			app.Converge([]*unstructured.Unstructured{deployment}, app.WorkloadTarget{
				Paused:   test.paused,
				Replicas: test.replicas,
			})
			got, _, _ := unstructured.NestedInt64(deployment.Object, "spec", "replicas")
			if got != test.want {
				t.Fatalf("replicas = %d, want %d", got, test.want)
			}
		})
	}
}

// TestConvergeResources covers C-67's workload side: the declared set lands on
// every container and init container, and an undeclared field is not invented.
func TestConvergeResources(t *testing.T) {
	deployment := renderedWorkload("Deployment", 1)
	app.Converge([]*unstructured.Unstructured{deployment}, app.WorkloadTarget{
		Resources: &v1beta1.ResourceRequirements{
			Requests: v1beta1.ResourceList{CPU: "100m", Memory: "128Mi"},
			Limits:   v1beta1.ResourceList{CPU: "500m"},
		},
	})
	for _, key := range []string{"containers", "initContainers"} {
		containers, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", key)
		for _, raw := range containers {
			container := raw.(map[string]any)
			resources, ok := container["resources"].(map[string]any)
			if !ok {
				t.Fatalf("%s: the container carries no resources after convergence", key)
			}
			if got, _ := resources["limits"].(map[string]any)["cpu"].(string); got != "500m" {
				t.Fatalf("%s: limit cpu = %q, want 500m", key, got)
			}
			if _, found := resources["limits"].(map[string]any)["memory"]; found {
				t.Fatalf("%s: an undeclared memory limit was invented", key)
			}
			requests, _ := resources["requests"].(map[string]any)
			if requests["cpu"] != "100m" || requests["memory"] != "128Mi" {
				t.Fatalf("%s: requests = %v, want cpu 100m and memory 128Mi", key, requests)
			}
		}
	}
}

// TestConvergeLeavesNonWorkloads checks that a stop does not touch the objects
// that carry entries and data (C-66).
func TestConvergeLeavesNonWorkloads(t *testing.T) {
	service := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]any{"name": "web", "namespace": "apps"},
		"spec": map[string]any{
			"selector": map[string]any{"app": "web"},
			"ports":    []any{map[string]any{"port": int64(80)}},
		},
	}}
	before := service.DeepCopy()
	app.Converge([]*unstructured.Unstructured{service}, app.WorkloadTarget{Paused: true, Replicas: new(int32(0))})
	if !reflect.DeepEqual(before.Object, service.Object) {
		t.Fatal("a stop changed a Service")
	}
}

// TestWorkloadRefs covers the restart selection: only workloads are rebuilt.
func TestWorkloadRefs(t *testing.T) {
	refs := []v1beta1.ObjectRef{
		{Kind: "StatefulSet", Name: "web"},
		{Kind: "Service", Name: "web"},
		{Kind: "ConfigMap", Name: "web"},
		{Kind: "PersistentVolumeClaim", Name: "data"},
		{Kind: "Deployment", Name: "worker"},
	}
	got := app.WorkloadRefs(refs)
	if len(got) != 2 || got[0].Kind != "StatefulSet" || got[1].Kind != "Deployment" {
		t.Fatalf("workload refs = %v, want the StatefulSet and the Deployment", got)
	}
}

// renderedWorkload renders a workload with the given replica count and no
// container resources.
func renderedWorkload(kind string, replicas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       kind,
		"metadata":   map[string]any{"name": "web", "namespace": "apps"},
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{"matchLabels": map[string]any{"app": "web"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
				"spec": map[string]any{
					"containers":     []any{map[string]any{"name": "web", "image": "nginx"}},
					"initContainers": []any{map[string]any{"name": "init", "image": "busybox"}},
				},
			},
		},
	}}
}
