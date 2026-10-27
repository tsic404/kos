// Package tc48 installs one application on two storage backends and compares
// the two instances: the rendered objects differ only where the volume policy
// is allowed to differ, and the claim the volume object created carries the
// volume name (AC-24, TC-48).
package tc48

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/volume"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	fastNamespace = "media-fast"
	slowNamespace = "media-slow"
	fastClass     = "fast"
	slowClass     = "slow"
	fastDriver    = "fast.example"
	slowDriver    = "slow.example"
)

// mediaTemplate renders one workload that mounts the claim the volume object
// creates. The claim name comes from the volume, never from the package.
const mediaTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
  labels:
    app: {{ .Name }}
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
`

func TestTC48TwoStorageBackends(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, fastNamespace)
	cluster.EnsureNamespace(t, slowNamespace)
	cluster.StorageClass(t, fastClass, fastDriver, true, "Retain", map[string]string{"fsType": "ext4"})
	cluster.StorageClass(t, slowClass, slowDriver, true, "Retain", map[string]string{"fsType": "ext4"})
	cluster.InstallDefinition(t, harness.DefinitionWithTemplate(t, "media", mediaTemplate, nil))
	cluster.InstallDefinition(t, harness.Definition(t, "notes", nil))

	controller := cluster.VolumeController(t, volume.Options{
		Capabilities: volume.Registered{{Name: volume.CapabilitySharedRWX, Version: volume.CapabilityVersionV1}},
	})

	fastVolume := installBackend(t, cluster, controller, fastNamespace, "data", fastClass, fastDriver, "pv-fast")
	slowVolume := installBackend(t, cluster, controller, slowNamespace, "data", slowClass, slowDriver, "pv-slow")

	if fastVolume.Status.Provider.Name != fastDriver || slowVolume.Status.Provider.Name != slowDriver {
		t.Fatalf("providers = %q and %q, want %q and %q",
			fastVolume.Status.Provider.Name, slowVolume.Status.Provider.Name, fastDriver, slowDriver)
	}

	installApplication(t, cluster, fastNamespace, "media", "media", "data")
	installApplication(t, cluster, slowNamespace, "media", "media", "data")
	// A second, different application installs on one of the two backends.
	installBackend(t, cluster, controller, fastNamespace, "docs", fastClass, fastDriver, "pv-notes")
	installApplication(t, cluster, fastNamespace, "notes", "notes", "docs")

	fastWorkload := deployment(t, cluster, fastNamespace, "media")
	slowWorkload := deployment(t, cluster, slowNamespace, "media")
	if equal := reflect.DeepEqual(comparable(fastWorkload), comparable(slowWorkload)); !equal {
		t.Fatalf("the two instances differ beyond the volume policy:\nfast: %v\nslow: %v",
			comparable(fastWorkload), comparable(slowWorkload))
	}
	for _, object := range []*unstructured.Unstructured{fastWorkload, slowWorkload} {
		claimName := mountedClaim(t, object)
		if claimName != "data" {
			t.Fatalf("%s mounts claim %q, want the volume name %q", object.GetName(), claimName, "data")
		}
	}
}

// installBackend creates one volume in a namespace, has the reconciler create
// and bind its claim, and returns the observed volume.
func installBackend(t *testing.T, cluster *harness.Cluster, controller *volume.Controller, namespace, volumeName, class, driver, pvName string) *v1beta1.Volume {
	t.Helper()
	spec := map[string]any{
		"mobility":     string(v1beta1.VolumeMobilityClusterMovable),
		"capacity":     "1Gi",
		"storageClass": class,
	}
	created := cluster.CreateIn(t, namespace, harness.Volume(namespace, volumeName, spec))
	harness.ReconcileVolume(t, controller, namespace, volumeName)

	claim := cluster.Claim(t, namespace, volumeName)
	if claim.GetName() != volumeName {
		t.Fatalf("claim name = %q, want the volume name %q", claim.GetName(), volumeName)
	}
	if claim.GetLabels()["managed-by"] == "" || claim.GetLabels()["owner-uid"] != string(created.GetUID()) {
		t.Fatalf("claim %s/%s is not attributed to the volume: %v", namespace, volumeName, claim.GetLabels())
	}

	cluster.PersistentVolume(t, pvName, class, driver, "Retain")
	cluster.BindClaim(t, namespace, volumeName, pvName)
	harness.ReconcileVolume(t, controller, namespace, volumeName)
	return storedVolume(t, cluster, namespace, volumeName)
}

// installApplication creates one application and drives it to Ready.
func installApplication(t *testing.T, cluster *harness.Cluster, namespace, name, definition, volumeName string) {
	t.Helper()
	application := harness.ApplicationIn(namespace, name, nil)
	application.Object["spec"] = map[string]any{
		"definitionRef": definition,
		"namespace":     namespace,
		"volumes":       []any{volumeName},
	}
	cluster.CreateIn(t, namespace, application)
	controller := cluster.ApplicationController(t, app.Options{})
	for round := 0; round < 6; round++ {
		harness.ReconcileApplicationRound(t, controller, namespace, name)
		if phase(t, cluster, namespace, name) == app.PhaseReady {
			return
		}
	}
	t.Fatalf("application %s/%s did not reach Ready: %s", namespace, name, phase(t, cluster, namespace, name))
}

// phase reads the stored phase of one application.
func phase(t *testing.T, cluster *harness.Cluster, namespace, name string) string {
	t.Helper()
	object, err := cluster.Direct(t, "kos.tsic.top", "v1beta1", "Application", namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get application %s/%s: %v", namespace, name, err)
	}
	phase, _, err := unstructured.NestedString(object.Object, "status", "phase")
	if err != nil {
		t.Fatalf("read phase of %s/%s: %v", namespace, name, err)
	}
	return phase
}

// storedVolume decodes one stored volume.
func storedVolume(t *testing.T, cluster *harness.Cluster, namespace, name string) *v1beta1.Volume {
	t.Helper()
	object, err := cluster.Direct(t, "kos.tsic.top", "v1beta1", "Volume", namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get volume %s/%s: %v", namespace, name, err)
	}
	decoded := &v1beta1.Volume{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, decoded); err != nil {
		t.Fatalf("decode volume %s/%s: %v", namespace, name, err)
	}
	return decoded
}

// deployment reads the rendered workload of one application.
func deployment(t *testing.T, cluster *harness.Cluster, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	object, err := cluster.Direct(t, "apps", "v1", "Deployment", namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment %s/%s: %v", namespace, name, err)
	}
	return object
}

// mountedClaim reads the claim a workload mounts.
func mountedClaim(t *testing.T, object *unstructured.Unstructured) string {
	t.Helper()
	volumes, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "volumes")
	if err != nil || !found {
		t.Fatalf("%s carries no volumes: %v", object.GetName(), err)
	}
	for _, raw := range volumes {
		entry, _ := raw.(map[string]any)
		source, _ := entry["persistentVolumeClaim"].(map[string]any)
		if name, ok := source["claimName"].(string); ok {
			return name
		}
	}
	t.Fatalf("%s mounts no claim", object.GetName())
	return ""
}

// comparable is the part of a workload two instances of one definition must
// share: the rendered spec and the package's own metadata. Server-assigned
// identity and the namespace the instance lives in are not package content.
func comparable(object *unstructured.Unstructured) map[string]string {
	fields := harness.Flatten(object.Object)
	for path := range fields {
		switch path {
		case "metadata.namespace", "metadata.uid", "metadata.creationTimestamp",
			"metadata.resourceVersion", "metadata.generation", "metadata.labels.owner-uid":
			delete(fields, path)
		}
	}
	return fields
}

// TestTC48ExpansionAppliesThroughTheAPIServer pins C-38's expansion on a real
// API server: the reconciler grows the claim the volume owns through
// server-side apply, so the stored size changes and the field manager records
// the apply. It also proves the apply is partial: the binding another writer
// set is not rewritten.
func TestTC48ExpansionAppliesThroughTheAPIServer(t *testing.T) {
	const (
		namespace = "media-grow"
		class     = "grow"
		driver    = "grow.example"
	)
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, namespace)
	cluster.StorageClass(t, class, driver, true, "Retain", map[string]string{"fsType": "ext4"})
	controller := cluster.VolumeController(t, volume.Options{})

	spec := map[string]any{
		"mobility":     string(v1beta1.VolumeMobilityClusterMovable),
		"capacity":     "1Gi",
		"storageClass": class,
	}
	cluster.CreateIn(t, namespace, harness.Volume(namespace, "data", spec))
	harness.ReconcileVolume(t, controller, namespace, "data")
	if size := claimSize(t, cluster.Claim(t, namespace, "data")); size != "1Gi" {
		t.Fatalf("created claim size = %q, want 1Gi", size)
	}

	cluster.PersistentVolume(t, "pv-grow", class, driver, "Retain")
	cluster.BindClaim(t, namespace, "data", "pv-grow")
	harness.ReconcileVolume(t, controller, namespace, "data")
	if phase := storedVolume(t, cluster, namespace, "data").Status.Phase; phase != volume.PhaseBound {
		t.Fatalf("volume is %q, want %q before the expansion", phase, volume.PhaseBound)
	}

	volumes := cluster.Direct(t, "kos.tsic.top", "v1beta1", "Volume", namespace)
	stored, err := volumes.Get(context.Background(), "data", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get volume %s/data: %v", namespace, err)
	}
	if err := unstructured.SetNestedField(stored.Object, "2Gi", "spec", "capacity"); err != nil {
		t.Fatalf("set the requested capacity: %v", err)
	}
	if _, err := volumes.Update(context.Background(), stored, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("raise the volume capacity: %v", err)
	}
	harness.ReconcileVolume(t, controller, namespace, "data")

	claim := cluster.Claim(t, namespace, "data")
	if size := claimSize(t, claim); size != "2Gi" {
		t.Fatalf("expanded claim size = %q, want 2Gi", size)
	}
	if bound := nestedString(t, claim.Object, "spec", "volumeName"); bound != "pv-grow" {
		t.Fatalf("the expansion rewrote the binding: volumeName = %q", bound)
	}
	applied := false
	for _, entry := range claim.GetManagedFields() {
		if entry.Manager == "kos-volume-reconciler" && entry.Operation == metav1.ManagedFieldsOperationApply {
			applied = true
		}
	}
	if !applied {
		t.Fatalf("no server-side apply by the volume reconciler in %v", claim.GetManagedFields())
	}
}

// claimSize reads the requested size of one claim.
func claimSize(t *testing.T, claim *unstructured.Unstructured) string {
	t.Helper()
	size, found, err := unstructured.NestedString(claim.Object, "spec", "resources", "requests", "storage")
	if err != nil || !found {
		t.Fatalf("claim %s carries no requested size: %v", claim.GetName(), err)
	}
	return size
}

// nestedString reads one nested string; it fails the case when it is absent.
func nestedString(t *testing.T, object map[string]any, path ...string) string {
	t.Helper()
	value, found, err := unstructured.NestedString(object, path...)
	if err != nil || !found {
		t.Fatalf("%v is not a string: %v", path, err)
	}
	return value
}
