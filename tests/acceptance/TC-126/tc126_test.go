// Package tc126 verifies C-67's refusal of an illegal resource combination: a
// limit below its request is rejected before the write, the refusal names the
// field, and neither the workload nor the projection moves (AC-62, TC-126).
package tc126

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	targetNamespace = "apps"
	applicationName = "web"
)

// deploymentTemplate renders one Deployment with no container resources.
const deploymentTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: 1
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
`

func TestTC126IllegalResourceCombination(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "lifecycle", deploymentTemplate, nil))
	gateway := cluster.StartControlPlane(t, nil)

	// A create whose limit is below its request never reaches the cluster.
	invalid := harness.ApplicationManifest(applicationName, harness.Namespace, "lifecycle", targetNamespace, nil, nil)
	invalid["spec"].(map[string]any)["resources"] = resourceBody("500m", "100m")
	status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", invalid, nil)
	requireRefusal(t, status, body)
	if applicationExists(t, cluster) {
		t.Fatal("the refused create wrote the application")
	}

	// A valid application installs, then an illegal override is refused without
	// changing the stored spec.
	valid := harness.ApplicationManifest(applicationName, harness.Namespace, "lifecycle", targetNamespace, nil, nil)
	valid["spec"].(map[string]any)["resources"] = resourceBody("100m", "500m")
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", valid, nil); status != http.StatusAccepted {
		t.Fatalf("create the valid application: status %d, body %v", status, body)
	}
	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)
	running := cluster.Get(t, "v1beta1", "Application", applicationName)
	runningResources, _, _ := unstructured.NestedMap(running.Object, "spec", "resources")

	override := updateBody(t, running, resourceBody("500m", "100m"))
	status, body = gateway.Call(t, http.MethodPut, "/api/v1/resources/Application/"+applicationName+"?namespace="+harness.Namespace, override, nil)
	requireRefusal(t, status, body)

	stored := cluster.Get(t, "v1beta1", "Application", applicationName)
	storedResources, _, _ := unstructured.NestedMap(stored.Object, "spec", "resources")
	if !reflect.DeepEqual(runningResources, storedResources) {
		t.Fatalf("the refused write changed the resources: %v -> %v", runningResources, storedResources)
	}
	reconcileOnce(t, controller)
	workload := cluster.Workload(t, "Deployment", targetNamespace, applicationName)
	containers, _, _ := unstructured.NestedSlice(workload.Object, "spec", "template", "spec", "containers")
	resources, _ := containers[0].(map[string]any)["resources"].(map[string]any)
	if resources["limits"].(map[string]any)["cpu"] != "500m" {
		t.Fatalf("the workload moved after the refusal: %v", resources)
	}
	projected, _, _ := unstructured.NestedMap(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "resources")
	if projected == nil || projected["limits"].(map[string]any)["cpu"] != "500m" {
		t.Fatalf("the projection moved after the refusal: %v", projected)
	}
}

// requireRefusal requires the invalid-field answer that names the limit field.
func requireRefusal(t *testing.T, status int, body map[string]any) {
	t.Helper()
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %v", status, body)
	}
	failure, _ := body["error"].(map[string]any)
	if failure["code"] != "invalid_field" {
		t.Fatalf("code = %v, want invalid_field", failure["code"])
	}
	message, _ := failure["message"].(string)
	if !strings.Contains(message, "spec.resources.limits.cpu") {
		t.Fatalf("message %q does not name spec.resources.limits.cpu", message)
	}
}

// resourceBody renders one resource set with the given cpu values.
func resourceBody(request, limit string) map[string]any {
	return map[string]any{
		"requests": map[string]any{"cpu": request},
		"limits":   map[string]any{"cpu": limit},
	}
}

// updateBody builds the full update body of one stored application.
func updateBody(t *testing.T, stored *unstructured.Unstructured, resources map[string]any) map[string]any {
	t.Helper()
	spec, _, err := unstructured.NestedMap(stored.Object, "spec")
	if err != nil {
		t.Fatalf("read the spec: %v", err)
	}
	spec["resources"] = resources
	metadata := map[string]any{"name": applicationName, "namespace": harness.Namespace, "resourceVersion": stored.GetResourceVersion()}
	if labels := stored.GetLabels(); len(labels) > 0 {
		encoded := map[string]any{}
		for key, value := range labels {
			encoded[key] = value
		}
		metadata["labels"] = encoded
	}
	return map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   metadata,
		"spec":       spec,
	}
}

// applicationExists reports whether the refused application was stored anyway.
func applicationExists(t *testing.T, cluster *harness.Cluster) bool {
	t.Helper()
	_, err := cluster.Direct(t, api.Group, "v1beta1", "Application", harness.Namespace).
		Get(context.Background(), applicationName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read the application: %v", err)
	}
	return true
}

// reconcileOnce drives one application round.
func reconcileOnce(t *testing.T, controller *app.Controller) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: harness.Namespace, Name: applicationName,
	}); err != nil {
		t.Fatalf("reconcile %s: %v", applicationName, err)
	}
}
