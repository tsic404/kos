// Package tc127 verifies C-66's rule for a replica write during a stop: the
// write is accepted and readable while the workload stays at zero, and starting
// converges to the last written value; an application whose count was never
// written returns to the count it stopped with (AC-61, TC-127).
package tc127

import (
	"context"
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	targetNamespace = "apps"
	scaledName      = "scaled"
	untouchedName   = "untouched"
)

// lifecycleTemplate renders a Deployment with two replicas and a Service; the
// lifecycle cases use it where the package content is not itself under test.
const lifecycleTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: 2
  selector:
    matchLabels:
      app: {{ .Name }}
  template:
    metadata:
      labels:
        app: {{ .Name }}
    spec:
      containers:
        - name: app
          image: nginx:1.27
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  selector:
    app: {{ .Name }}
  ports:
    - port: 80
      targetPort: 80
`

func TestTC127ScaleWhileStopped(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "lifecycle", lifecycleTemplate, nil))

	gateway := cluster.StartControlPlane(t, nil)
	createApplication(t, gateway, scaledName, new(int64(2)))
	createApplication(t, gateway, untouchedName, nil)

	controller := cluster.ApplicationController(t, app.Options{})
	for _, name := range []string{scaledName, untouchedName} {
		reconcileOnce(t, controller, name)
		reconcileOnce(t, controller, name)
		if phase := phaseOf(t, cluster, name); phase != app.PhaseReady {
			t.Fatalf("%s: phase = %q, want %q", name, phase, app.PhaseReady)
		}
		if got := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, name); got != 2 {
			t.Fatalf("%s: replicas = %d, want 2", name, got)
		}
		cluster.SetWorkloadStatus(t, "Deployment", targetNamespace, name, 2, 2)
		reconcileOnce(t, controller, name)
	}

	for _, name := range []string{scaledName, untouchedName} {
		action(t, gateway, name, map[string]any{"action": "stop"})
		reconcileOnce(t, controller, name)
		if got := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, name); got != 0 {
			t.Fatalf("%s: stopped replicas = %d, want 0", name, got)
		}
	}

	// The count written during the stop is accepted and readable while the
	// workload still stays at zero (C-66).
	action(t, gateway, scaledName, map[string]any{"action": "scale", "replicas": 5})
	stored := cluster.Get(t, "v1beta1", "Application", scaledName)
	if replicas, _, _ := unstructured.NestedInt64(stored.Object, "spec", "replicas"); replicas != 5 {
		t.Fatalf("the projection shows replicas %d, want 5", replicas)
	}
	reconcileOnce(t, controller, scaledName)
	if got := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, scaledName); got != 0 {
		t.Fatalf("stopped replicas after the write = %d, want 0", got)
	}

	for _, test := range []struct {
		name string
		want int64
	}{{scaledName, 5}, {untouchedName, 2}} {
		action(t, gateway, test.name, map[string]any{"action": "start"})
		reconcileOnce(t, controller, test.name)
		if got := cluster.WorkloadReplicas(t, "Deployment", targetNamespace, test.name); got != test.want {
			t.Fatalf("%s: resumed replicas = %d, want %d", test.name, got, test.want)
		}
		cluster.SetWorkloadStatus(t, "Deployment", targetNamespace, test.name, test.want, test.want)
		reconcileOnce(t, controller, test.name)
		if phase := phaseOf(t, cluster, test.name); phase != app.PhaseReady {
			t.Fatalf("%s: resumed phase = %q, want %q", test.name, phase, app.PhaseReady)
		}
		if health := healthOf(t, cluster, test.name); health != string(v1beta1.HealthStateHealthy) {
			t.Fatalf("%s: resumed health = %q, want Healthy", test.name, health)
		}
	}
}

// createApplication posts one application; replicas nil leaves the count the
// package renders.
func createApplication(t *testing.T, gateway *harness.ControlPlane, name string, replicas *int64) {
	t.Helper()
	manifest := harness.ApplicationManifest(name, harness.Namespace, "lifecycle", targetNamespace, nil, nil)
	if replicas != nil {
		manifest["spec"].(map[string]any)["replicas"] = *replicas
	}
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
		t.Fatalf("create %s: status %d, body %v", name, status, body)
	}
}

// action posts one application action and requires acceptance.
func action(t *testing.T, gateway *harness.ControlPlane, name string, extra map[string]any) {
	t.Helper()
	body := map[string]any{"kind": "Application", "namespace": harness.Namespace, "name": name}
	for key, value := range extra {
		body[key] = value
	}
	if status, response := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", body, nil); status != http.StatusAccepted {
		t.Fatalf("action %v on %s: status %d, body %v", extra, name, status, response)
	}
}

// reconcileOnce drives one application round.
func reconcileOnce(t *testing.T, controller *app.Controller, name string) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: harness.Namespace, Name: name,
	}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

// phaseOf reads the stored phase of one application.
func phaseOf(t *testing.T, cluster *harness.Cluster, name string) string {
	t.Helper()
	phase, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", name).Object, "status", "phase")
	if err != nil {
		t.Fatalf("read the phase of %s: %v", name, err)
	}
	return phase
}

// healthOf reads the stored health state of one application.
func healthOf(t *testing.T, cluster *harness.Cluster, name string) string {
	t.Helper()
	health, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", name).Object, "status", "health", "state")
	if err != nil {
		t.Fatalf("read the health of %s: %v", name, err)
	}
	return health
}
