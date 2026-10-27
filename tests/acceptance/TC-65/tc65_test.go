// Package tc65 deletes an application that owns a volume: the ingress and the
// workloads go away, the volume and its claim stay, and the claim keeps no
// release marker, because deleting an application is not a volume delete
// (AC-32, TC-65).
package tc65

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/volume"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	namespace       = "apps"
	applicationName = "media"
	volumeName      = "data"
	class           = "standard"
	driver          = "standard.example"
)

// mediaTemplate renders the workload and the ingress an application owns, so
// the delete path has something to remove besides the data.
const mediaTemplate = `apiVersion: apps/v1
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
        - name: media
          image: example.test/media:1
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: {{ index .Volumes "data" }}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  rules:
    - host: media.test
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: {{ .Name }}
                port:
                  number: 80
`

func TestTC65DeletingApplicationKeepsData(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, namespace)
	cluster.StorageClass(t, class, driver, true, "Retain", map[string]string{"fsType": "ext4"})
	volumeController := cluster.VolumeController(t, volume.Options{})

	cluster.CreateIn(t, namespace, harness.Volume(namespace, volumeName, map[string]any{
		"mobility":     string(v1beta1.VolumeMobilityClusterMovable),
		"capacity":     "1Gi",
		"storageClass": class,
	}))
	harness.ReconcileVolume(t, volumeController, namespace, volumeName)
	cluster.PersistentVolume(t, "pv-data", class, driver, "Retain")
	cluster.BindClaim(t, namespace, volumeName, "pv-data")
	harness.ReconcileVolume(t, volumeController, namespace, volumeName)

	cluster.InstallDefinition(t, harness.DefinitionWithTemplate(t, "media", mediaTemplate, nil))
	application := harness.ApplicationIn(namespace, applicationName, nil)
	application.Object["spec"] = map[string]any{
		"definitionRef": "media",
		"namespace":     namespace,
		"volumes":       []any{volumeName},
	}
	cluster.CreateIn(t, namespace, application)
	applicationController := cluster.ApplicationController(t, app.Options{})
	for range 6 {
		harness.ReconcileApplicationRound(t, applicationController, namespace, applicationName)
		if phase(t, cluster) == app.PhaseReady {
			break
		}
	}
	if got := phase(t, cluster); got != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q before the delete", got, app.PhaseReady)
	}
	requireDeployment(t, cluster, true, applicationName)

	// The user deletes the application through the native path, as kubectl
	// does. The volume is not part of the application's rendered objects (F5).
	if err := cluster.Direct(t, api.Group, "v1beta1", "Application", namespace).Delete(
		context.Background(), applicationName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete application: %v", err)
	}
	for range 6 {
		harness.ReconcileApplicationRound(t, applicationController, namespace, applicationName)
	}
	harness.Eventually(t, 15*time.Second, "application to be removed", func() bool {
		_, err := cluster.Direct(t, api.Group, "v1beta1", "Application", namespace).Get(
			context.Background(), applicationName, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})

	requireVolume(t, cluster, volumeName)
	claim := cluster.Claim(t, namespace, volumeName)
	if claim.GetLabels()["released"] != "" {
		t.Fatalf("deleting the application marked the claim as released: %v", claim.GetLabels())
	}
	requireDeployment(t, cluster, false, applicationName)
	requireIngress(t, cluster, false, applicationName)
}

// phase reads the stored application phase.
func phase(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	object, err := cluster.Direct(t, api.Group, "v1beta1", "Application", namespace).Get(
		context.Background(), applicationName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get application: %v", err)
	}
	value, _, err := unstructured.NestedString(object.Object, "status", "phase")
	if err != nil {
		t.Fatalf("read application phase: %v", err)
	}
	return value
}

// requireVolume requires the volume object to exist with its claim.
func requireVolume(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "Volume", namespace).Get(
		context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Fatalf("volume %s must still exist: %v", name, err)
	}
	spec, _, err := unstructured.NestedMap(mustObject(t, cluster, api.Group, "v1beta1", "Volume", name).Object, "status")
	if err != nil || len(spec) == 0 {
		t.Fatalf("volume %s carries no status: %v", name, err)
	}
}

// requireDeployment requires the workload to be present or gone.
func requireDeployment(t *testing.T, cluster *harness.Cluster, present bool, name string) {
	t.Helper()
	_, err := cluster.Kube.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
	switch {
	case present && err != nil:
		t.Fatalf("deployment %s must exist: %v", name, err)
	case !present && !apierrors.IsNotFound(err):
		t.Fatalf("deployment %s must be gone (err = %v)", name, err)
	}
}

// requireIngress requires the ingress to be present or gone.
func requireIngress(t *testing.T, cluster *harness.Cluster, present bool, name string) {
	t.Helper()
	_, err := cluster.Kube.NetworkingV1().Ingresses(namespace).Get(context.Background(), name, metav1.GetOptions{})
	switch {
	case present && err != nil:
		t.Fatalf("ingress %s must exist: %v", name, err)
	case !present && !apierrors.IsNotFound(err):
		t.Fatalf("ingress %s must be gone (err = %v)", name, err)
	}
}

// mustObject reads one object through the native path.
func mustObject(t *testing.T, cluster *harness.Cluster, group, version, kind, name string) *unstructured.Unstructured {
	t.Helper()
	object, err := cluster.Direct(t, group, version, kind, namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s %s: %v", kind, name, err)
	}
	return object
}
