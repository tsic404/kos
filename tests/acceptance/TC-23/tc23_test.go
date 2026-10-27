// Package tc23 verifies the minimal install path of a built-in definition: the
// write returns acceptance with a reference, the application reaches Ready, its
// downstream workload exists, and the KOS group serves exactly the ten kernel
// kinds (AC-11, TC-23).
package tc23

import (
	"context"
	"net/http"
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/app/builtin"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	// targetNamespace is the namespace the application declares.
	targetNamespace = "apps"
	// applicationName is the application the case installs.
	applicationName = "web"
)

func TestTC23BuiltinDefinitionReachesReady(t *testing.T) {
	cluster := harness.Start(t)
	definition, err := builtin.Definition("nginx")
	if err != nil {
		t.Fatalf("load the nginx definition: %v", err)
	}
	cluster.InstallDefinition(t, definition)
	cluster.EnsureNamespace(t, targetNamespace)
	gateway := cluster.StartControlPlane(t, nil)

	manifest := harness.ApplicationManifest(applicationName, harness.Namespace, "nginx", targetNamespace,
		map[string]any{"replicas": int64(2)}, nil)
	// The control plane is the creator, so a body without managed-by must still
	// become attributable; otherwise no later subject could write its status (C-02).
	metadata, _ := manifest["metadata"].(map[string]any)
	delete(metadata["labels"].(map[string]any), markers.ManagedBy)
	status, response := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil)
	if status != http.StatusAccepted {
		t.Fatalf("create application: status %d, body %v", status, response)
	}
	if accepted, _ := response["accepted"].(bool); !accepted {
		t.Fatalf("the create must answer with acceptance: %v", response)
	}
	if created := cluster.Get(t, "v1beta1", "Application", applicationName); created.GetLabels()[markers.ManagedBy] != write.ManagedByValue {
		t.Fatalf("the created application is not attributable: labels = %v", created.GetLabels())
	}
	ref, isObject := response["ref"].(map[string]any)
	if !isObject {
		t.Fatalf("the create must answer with an object reference: %v", response)
	}
	if kind, name := harness.String(ref, "kind"), harness.String(ref, "name"); kind != "Application" || name != applicationName {
		t.Fatalf("reference = %s/%s, want Application/%s", kind, name, applicationName)
	}

	controller := cluster.ApplicationController(t, app.Options{})
	for round := range 5 {
		if _, err := controller.Reconcile(context.Background(), reconcile.Request{
			Kind: "Application", Namespace: harness.Namespace, Name: applicationName,
		}); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if phaseOf(t, cluster) == app.PhaseReady {
			break
		}
	}
	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q; status = %v", phase, app.PhaseReady, cluster.Get(t, "v1beta1", "Application", applicationName).Object["status"])
	}

	workloads := cluster.Direct(t, "apps", "v1", "Deployment", targetNamespace)
	if _, err := workloads.Get(context.Background(), applicationName, metav1.GetOptions{}); err != nil {
		t.Fatalf("the rendered deployment is missing: %v", err)
	}

	assertKernelKinds(t, cluster)
}

// phaseOf reads the stored phase of the case's application.
func phaseOf(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	phase, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "phase")
	if err != nil {
		t.Fatalf("read the phase: %v", err)
	}
	return phase
}

// assertKernelKinds checks that discovery serves exactly the ten kernel kinds
// and that no extension object class is installed (C-01, C-19).
func assertKernelKinds(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	resources, err := cluster.Kube.Discovery().ServerResourcesForGroupVersion(api.Group + "/v1beta1")
	if err != nil {
		t.Fatalf("discover %s: %v", api.Group, err)
	}
	served := make(map[string]bool, len(resources.APIResources))
	for _, resource := range resources.APIResources {
		if resource.Name == "" || containsSlash(resource.Name) {
			continue
		}
		served[resource.Kind] = true
	}
	want := api.Kinds()
	if len(served) != len(want) {
		t.Fatalf("the KOS group serves %d kinds, want %d: %v", len(served), len(want), sorted(served))
	}
	for _, kind := range want {
		if !served[kind] {
			t.Errorf("%s is not served", kind)
		}
	}
	for _, kind := range []string{"Application", "ApplicationDefinition", "Volume", "Endpoint"} {
		if !served[kind] {
			t.Errorf("the four kernel objects the case installs are not discoverable: %s", kind)
		}
	}

	crds, err := cluster.APIExtensions.ApiextensionsV1().CustomResourceDefinitions().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list the CRDs: %v", err)
	}
	for _, crd := range crds.Items {
		if crd.Spec.Group != api.Group {
			t.Errorf("an extension object class is installed: %s", crd.Name)
		}
	}
}

// containsSlash reports a subresource name.
func containsSlash(name string) bool {
	for _, character := range name {
		if character == '/' {
			return true
		}
	}
	return false
}

// sorted returns the map keys in order.
func sorted(items map[string]bool) []string {
	names := make([]string, 0, len(items))
	for name := range items {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
