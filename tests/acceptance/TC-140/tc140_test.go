// Package tc140 verifies that a definition update never reaches an installed
// application: the volume set and the materialized entries stay as they were
// installed, while a new installation follows the new package (AC-68, TC-140).
package tc140

import (
	"context"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const targetNamespace = "apps"

// deployTemplate renders one Deployment that names no claim, so the volume set
// is observed on the application spec alone.
const deployTemplate = `apiVersion: apps/v1
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
          image: example.test/app:1
`

func TestTC140DefinitionUpdateLeavesInstalledObjectsAlone(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.InstallDefinition(t, harness.DefinitionWithDefaults(t, "package", deployTemplate,
		[]string{"data"},
		[]v1beta1.EndpointDefault{
			{Name: "web", Kind: v1beta1.EndpointKindHTTPHost},
			{Name: "metrics", Kind: v1beta1.EndpointKindTCP, Port: 9090},
		}))

	gateway := cluster.StartControlPlane(t, nil)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.ApplicationManifest("first", harness.Namespace, "package", targetNamespace, nil, nil), nil); status != http.StatusAccepted {
		t.Fatalf("create first: status %d, body %v", status, body)
	}
	// The first installation records the hostname its http entry needs (C-32).
	putHosts(t, gateway, cluster, "first", map[string]any{"web": "first.example.test"})
	controller := cluster.ApplicationController(t, app.Options{})
	if phase := cluster.SettleApplication(t, controller, "first", 5); phase != app.PhaseReady {
		t.Fatalf("first application phase = %q, want %q", phase, app.PhaseReady)
	}

	// The package moves: another volume name, another port, one more entry, and
	// the http declaration the first installation recorded is renamed.
	cluster.InstallDefinition(t, harness.DefinitionWithDefaults(t, "package", deployTemplate,
		[]string{"cache"},
		[]v1beta1.EndpointDefault{
			{Name: "portal", Kind: v1beta1.EndpointKindHTTPHost},
			{Name: "metrics", Kind: v1beta1.EndpointKindTCP, Port: 9091},
			{Name: "admin", Kind: v1beta1.EndpointKindUDP, Port: 9092},
		}))
	published, _, _ := unstructured.NestedStringSlice(cluster.Get(t, "v1beta1", "ApplicationDefinition", "package").Object, "spec", "volumes")
	if strings.Join(published, ",") != "cache" {
		t.Fatalf("the definition update did not land: spec.volumes = %v", published)
	}

	cluster.ApplicationRound(t, controller, "first")

	// Nothing the installation wrote moved (C-32).
	if got := volumeNames(t, cluster, "first"); strings.Join(got, ",") != "data" {
		t.Fatalf("first application spec.volumes = %v, want data", got)
	}
	metrics := cluster.Get(t, "v1beta1", "Endpoint", "first-metrics")
	if port, _, _ := unstructured.NestedInt64(metrics.Object, "spec", "port"); port != 9090 {
		t.Fatalf("first-metrics port = %d, want the installed 9090", port)
	}
	if host, _, _ := unstructured.NestedString(cluster.Get(t, "v1beta1", "Endpoint", "first-web").Object, "spec", "host"); host != "first.example.test" {
		t.Fatalf("first-web host = %q, want the installed first.example.test", host)
	}
	for _, gone := range []string{"first-admin", "first-portal"} {
		if _, err := cluster.GetObject(context.Background(), "v1beta1", "Endpoint", harness.Namespace, gone); !apierrors.IsNotFound(err) {
			t.Fatalf("the definition update materialized %s: %v", gone, err)
		}
	}
	if count := endpointsOf(t, cluster, "first"); count != 2 {
		t.Fatalf("first application has %d endpoints, want the installed two", count)
	}

	// The stored hostname names a declaration the package no longer carries.
	// It is the record of the installed entry, so an unrelated update of the
	// application is still accepted (C-32).
	stored := cluster.Get(t, "v1beta1", "Application", "first")
	spec, _, _ := unstructured.NestedMap(stored.Object, "spec")
	spec["replicas"] = int64(2)
	update := map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":            "first",
			"namespace":       harness.Namespace,
			"resourceVersion": stored.GetResourceVersion(),
		},
		"spec": spec,
	}
	if status, body := gateway.Call(t, http.MethodPut, "/api/v1/resources/Application/first?namespace="+harness.Namespace, update, nil); status != http.StatusAccepted {
		t.Fatalf("an update keeping the stored hostname: status %d, body %v", status, body)
	}

	// A new installation takes the new package (C-32).
	second := harness.ApplicationManifest("second", harness.Namespace, "package", targetNamespace, nil, nil)
	second["spec"].(map[string]any)["endpointHosts"] = map[string]any{"portal": "second.example.test"}
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", second, nil); status != http.StatusAccepted {
		t.Fatalf("create second: status %d, body %v", status, body)
	}
	if phase := cluster.SettleApplication(t, controller, "second", 5); phase != app.PhaseReady {
		t.Fatalf("second application phase = %q, want %q; status %+v", phase, app.PhaseReady, cluster.ApplicationStatusOf(t, "second"))
	}
	if got := volumeNames(t, cluster, "second"); strings.Join(got, ",") != "cache" {
		t.Fatalf("second application spec.volumes = %v, want cache", got)
	}
	metricsOfSecond := cluster.Get(t, "v1beta1", "Endpoint", "second-metrics")
	if port, _, _ := unstructured.NestedInt64(metricsOfSecond.Object, "spec", "port"); port != 9091 {
		t.Fatalf("second-metrics port = %d, want 9091", port)
	}
	if host, _, _ := unstructured.NestedString(cluster.Get(t, "v1beta1", "Endpoint", "second-portal").Object, "spec", "host"); host != "second.example.test" {
		t.Fatalf("second-portal host = %q, want second.example.test", host)
	}
	admin := cluster.Get(t, "v1beta1", "Endpoint", "second-admin")
	if kind, _, _ := unstructured.NestedString(admin.Object, "spec", "kind"); kind != string(v1beta1.EndpointKindUDP) {
		t.Fatalf("second-admin kind = %q, want %s", kind, v1beta1.EndpointKindUDP)
	}
}

// endpointsOf counts the stored endpoints that target one application.
func endpointsOf(t *testing.T, cluster *harness.Cluster, name string) int {
	t.Helper()
	count := 0
	for _, endpoint := range cluster.List(t, "v1beta1", "Endpoint") {
		if target, _, _ := unstructured.NestedString(endpoint.Object, "spec", "targetApp"); target == name {
			count++
		}
	}
	return count
}

// volumeNames reads the application's stored volume set.
func volumeNames(t *testing.T, cluster *harness.Cluster, name string) []string {
	t.Helper()
	names, _, err := unstructured.NestedStringSlice(cluster.Get(t, "v1beta1", "Application", name).Object, "spec", "volumes")
	if err != nil {
		t.Fatalf("read spec.volumes of %s: %v", name, err)
	}
	return names
}

// putHosts writes the hostnames of the entries whose form needs one (C-32).
func putHosts(t *testing.T, gateway *harness.ControlPlane, cluster *harness.Cluster, name string, hosts map[string]any) {
	t.Helper()
	stored := cluster.Get(t, "v1beta1", "Application", name)
	spec, _, _ := unstructured.NestedMap(stored.Object, "spec")
	spec["endpointHosts"] = hosts
	body := map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       harness.Namespace,
			"resourceVersion": stored.GetResourceVersion(),
		},
		"spec": spec,
	}
	if status, response := gateway.Call(t, http.MethodPut, "/api/v1/resources/Application/"+name+"?namespace="+harness.Namespace, body, nil); status != http.StatusAccepted {
		t.Fatalf("hosts of %s: status %d, body %v", name, status, response)
	}
}
