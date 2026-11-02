// Package tc141 verifies the two boundaries of default materialization: the
// installation does not create the volumes its package names, so an application
// whose claim is missing is not healthy, and an entry form that needs a
// hostname the application does not give stays unavailable (AC-69, TC-141).
package tc141

import (
	"context"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const targetNamespace = "apps"

// volumeTemplate renders one Deployment whose claim reference comes from the
// declared volume set.
const volumeTemplate = `apiVersion: apps/v1
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
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: {{ index .Volumes "data" }}
`

func TestTC141MissingVolumeAndHostnameBoundaries(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.InstallDefinition(t, harness.DefinitionWithDefaults(t, "package", volumeTemplate,
		[]string{"data"},
		[]v1beta1.EndpointDefault{{Name: "web", Kind: v1beta1.EndpointKindHTTPHost}}))

	// The installation names the volume and the entry form, but gives neither
	// the volume objects nor a hostname: both are user decisions (C-32).
	gateway := cluster.StartControlPlane(t, nil)
	manifest := harness.ApplicationManifest("boundary", harness.Namespace, "package", targetNamespace, nil, nil)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
		t.Fatalf("create: status %d, body %v", status, body)
	}
	controller := cluster.ApplicationController(t, app.Options{})
	if phase := cluster.SettleApplication(t, controller, "boundary", 5); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q", phase, app.PhaseReady)
	}

	// The installation created no volume object, and the claim the workload
	// references therefore does not exist.
	if created := cluster.List(t, "v1beta1", "Volume"); len(created) != 0 {
		t.Fatalf("the install created volumes: %v", created)
	}
	if _, err := cluster.GetObject(context.Background(), "v1", "PersistentVolumeClaim", targetNamespace, "data"); !apierrors.IsNotFound(err) {
		t.Fatalf("the claim data exists before the user created the volume: %v", err)
	}
	if claim := claimName(t, cluster, "boundary"); claim != "data" {
		t.Fatalf("the workload references claim %q, want data", claim)
	}

	// The HTTP declaration had no hostname, so its default value is unavailable:
	// no endpoint exists for it (C-32).
	if count := endpointsOf(t, cluster, "boundary"); count != 0 {
		t.Fatalf("%d endpoints were materialized without a hostname", count)
	}
	status := cluster.ApplicationStatusOf(t, "boundary")
	if status.Health.State == v1beta1.HealthStateHealthy {
		t.Fatalf("an application whose claim is missing reports %q", status.Health.State)
	}

	// The user builds the volume and its own entry; a later round leaves both
	// alone.
	volume := harness.Manifest("Volume", targetNamespace, "data", map[string]string{markers.ManagedBy: write.ManagedByValue}, nil)
	volume["spec"] = map[string]any{"mobility": "cluster-movable", "capacity": "1Gi"}
	createdVolume := cluster.CreateIn(t, targetNamespace, &unstructured.Unstructured{Object: volume})

	endpoint := harness.Manifest("Endpoint", harness.Namespace, "custom-web", map[string]string{markers.ManagedBy: write.ManagedByValue}, nil)
	endpoint["spec"] = map[string]any{"kind": "http.host", "host": "boundary.example.test", "targetApp": "boundary"}
	createdEndpoint := cluster.Create(t, &unstructured.Unstructured{Object: endpoint})

	cluster.ApplicationRound(t, controller, "boundary")
	if stored := cluster.GetIn(t, targetNamespace, "v1beta1", "Volume", "data"); stored.GetResourceVersion() != createdVolume.GetResourceVersion() {
		t.Fatal("the round rewrote the user's volume")
	}
	if stored := cluster.Get(t, "v1beta1", "Endpoint", "custom-web"); stored.GetResourceVersion() != createdEndpoint.GetResourceVersion() {
		t.Fatal("the round rewrote the user's endpoint")
	}
	if count := endpointsOf(t, cluster, "boundary"); count != 1 {
		t.Fatalf("%d endpoints target the application, want the user's one", count)
	}
	if names, _, _ := unstructured.NestedStringSlice(cluster.Get(t, "v1beta1", "Application", "boundary").Object, "spec", "volumes"); strings.Join(names, ",") != "data" {
		t.Fatalf("spec.volumes = %v, want data", names)
	}
}

// claimName reads the claim the rendered workload references.
func claimName(t *testing.T, cluster *harness.Cluster, name string) string {
	t.Helper()
	workload := cluster.Workload(t, "Deployment", targetNamespace, name)
	volumes, _, _ := unstructured.NestedSlice(workload.Object, "spec", "template", "spec", "volumes")
	if len(volumes) == 0 {
		t.Fatalf("%s references no volume", name)
	}
	claim, _, _ := unstructured.NestedString(volumes[0].(map[string]any), "persistentVolumeClaim", "claimName")
	return claim
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
