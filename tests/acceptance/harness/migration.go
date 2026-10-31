package harness

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/migration"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/volume"
)

// Migration builds a migration request in the namespace of the volume it moves
// (C-40).
func Migration(namespace, name string, spec map[string]any) *unstructured.Unstructured {
	manifest := Manifest("VolumeMigration", namespace, name, map[string]string{"managed-by": "acceptance"}, nil)
	manifest["spec"] = spec
	return &unstructured.Unstructured{Object: manifest}
}

// MigrationController builds the real migration controller over the cluster.
func (c *Cluster) MigrationController(t *testing.T, options migration.Options) *migration.Controller {
	t.Helper()
	return migration.NewController(migration.NewWriterStore(c.Writer()), options)
}

// ReconcileMigration runs one migration round and fails the case on error.
func ReconcileMigration(t *testing.T, r *migration.Controller, namespace, name string) reconcile.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		Kind: "VolumeMigration", Namespace: namespace, Name: name,
	})
	if err != nil {
		t.Fatalf("reconcile migration %s/%s: %v", namespace, name, err)
	}
	return result
}

// MigrationOf decodes one stored migration.
func (c *Cluster) MigrationOf(t *testing.T, namespace, name string) *v1beta1.VolumeMigration {
	t.Helper()
	object, err := c.Direct(t, api.Group, "v1beta1", "VolumeMigration", namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get migration %s/%s: %v", namespace, name, err)
	}
	decoded := &v1beta1.VolumeMigration{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, decoded); err != nil {
		t.Fatalf("decode migration %s/%s: %v", namespace, name, err)
	}
	return decoded
}

// JobOutcome writes the outcome a step Job reports, standing in for the Job
// controller envtest does not run: a failed Job reports the reason the flow
// reads, a completed one carries the Complete condition (C-40).
func (c *Cluster) JobOutcome(t *testing.T, namespace, name string, failed bool, message string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	resource := c.Direct(t, "batch", "v1", "Job", namespace)
	job, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job %s/%s: %v", namespace, name, err)
	}
	// The API server validates a finished Job's status: a Complete Job needs
	// the success criteria and a completion time, a failed one its failure
	// target. Both need a start time.
	now := metav1.NewTime(time.Now().UTC()).Format(time.RFC3339)
	status := map[string]any{"startTime": now}
	if failed {
		status["conditions"] = []any{
			map[string]any{"type": "FailureTarget", "status": "True", "lastTransitionTime": now, "reason": "Failed", "message": message},
			map[string]any{"type": "Failed", "status": "True", "lastTransitionTime": now, "reason": "Failed", "message": message},
		}
	} else {
		status["conditions"] = []any{
			map[string]any{"type": "SuccessCriteriaMet", "status": "True", "lastTransitionTime": now},
			map[string]any{"type": "Complete", "status": "True", "lastTransitionTime": now},
		}
		status["completionTime"] = now
	}
	job.Object["status"] = status
	if _, err := resource.UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write job %s/%s status: %v", namespace, name, err)
	}
}

// JobGone reports that a Job the flow created is gone. envtest runs no Job
// controller, so a Job the flow deleted may still be terminating on its own
// finalizer; that counts as gone, because the flow issued the delete.
func (c *Cluster) JobGone(t *testing.T, namespace, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	job, err := c.Direct(t, "batch", "v1", "Job", namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return true
	}
	return job.GetDeletionTimestamp() != nil
}

// ClaimExists reports whether a claim is still there, so a case can observe the
// flow keeping the source (C-40, F6).
func (c *Cluster) ClaimExists(t *testing.T, namespace, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	_, err := c.Direct(t, "", "v1", "PersistentVolumeClaim", namespace).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

// ClaimDeleted reports that a claim is gone or, without the controller that
// clears the protection finalizer, already scheduled for removal.
func (c *Cluster) ClaimDeleted(t *testing.T, namespace, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	claim, err := c.Direct(t, "", "v1", "PersistentVolumeClaim", namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return true
	}
	return claim.GetDeletionTimestamp() != nil
}

// MigrationWorld is the object set a C-40 case starts from: one volume bound
// with the source policy, one application that mounts the claim, and the
// controllers that drive them.
type MigrationWorld struct {
	Namespace   string
	Volume      string
	Application string
	SourceClaim string
	TargetClaim string
	App         *app.Controller
	Volumes     *volume.Controller
	Migrations  *migration.Controller
	Clock       *Clock
}

// StartMigrationWorld builds the world one migration case moves: the volume is
// bound with the source policy, the application mounts its claim and reaches
// Ready, and the migration targets the other policy the user declares.
func StartMigrationWorld(t *testing.T, cluster *Cluster, namespace, name, sourceClass, targetClass string) *MigrationWorld {
	t.Helper()
	cluster.EnsureNamespace(t, namespace)
	cluster.StorageClass(t, sourceClass, "local.example", true, "Retain", map[string]string{"fsType": "ext4"})
	cluster.StorageClass(t, targetClass, "network.example", true, "Retain", map[string]string{"fsType": "ext4"})

	volumes := cluster.VolumeController(t, volume.Options{})
	cluster.CreateIn(t, namespace, Volume(namespace, name, map[string]any{
		"mobility":     string(v1beta1.VolumeMobilityClusterMovable),
		"capacity":     "1Gi",
		"storageClass": sourceClass,
	}))
	ReconcileVolume(t, volumes, namespace, name)
	cluster.PersistentVolume(t, "pv-"+name, sourceClass, "local.example", "Retain")
	cluster.BindClaim(t, namespace, name, "pv-"+name)
	ReconcileVolume(t, volumes, namespace, name)
	if phase, _, _ := unstructured.NestedString(cluster.GetIn(t, namespace, "v1beta1", "Volume", name).Object, "status", "phase"); phase != volume.PhaseBound {
		t.Fatalf("volume %q is %q, want %q before the migration", name, phase, volume.PhaseBound)
	}

	template := fmt.Sprintf(`apiVersion: apps/v1
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
            claimName: {{ index .Volumes %q }}
`, name)
	definition := "media-" + name
	cluster.InstallDefinition(t, DefinitionWithTemplate(t, definition, template, nil))
	application := "media-" + name
	body := ApplicationManifest(application, namespace, definition, namespace, nil, []string{name})
	cluster.CreateIn(t, namespace, &unstructured.Unstructured{Object: body})
	apps := cluster.ApplicationController(t, app.Options{})
	for range 8 {
		ReconcileApplicationRound(t, apps, namespace, application)
		if phase, _, _ := unstructured.NestedString(cluster.GetIn(t, namespace, "v1beta1", "Application", application).Object, "status", "phase"); phase == app.PhaseReady {
			break
		}
	}
	if phase, _, _ := unstructured.NestedString(cluster.GetIn(t, namespace, "v1beta1", "Application", application).Object, "status", "phase"); phase != app.PhaseReady {
		t.Fatalf("application %q is %q, want %q before the migration", application, phase, app.PhaseReady)
	}

	clock := NewClock(time.Now())
	migrations := cluster.MigrationController(t, migration.Options{RunnerImage: "registry.kos.test/migration-runner:1", Clock: clock.Now})
	return &MigrationWorld{
		Namespace:   namespace,
		Volume:      name,
		Application: application,
		SourceClaim: name,
		TargetClaim: name + "-target",
		App:         apps,
		Volumes:     volumes,
		Migrations:  migrations,
		Clock:       clock,
	}
}

// RequestMigration writes the user's declared policy into the volume and the
// migration request that moves it to that policy (C-40).
func (w *MigrationWorld) RequestMigration(t *testing.T, cluster *Cluster, targetClass string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	resource := cluster.Direct(t, api.Group, "v1beta1", "Volume", w.Namespace)
	stored, err := resource.Get(ctx, w.Volume, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read volume %q: %v", w.Volume, err)
	}
	if err := unstructured.SetNestedField(stored.Object, targetClass, "spec", "storageClass"); err != nil {
		t.Fatalf("write the declared policy of volume %q: %v", w.Volume, err)
	}
	if _, err := resource.Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update volume %q: %v", w.Volume, err)
	}
	cluster.CreateIn(t, w.Namespace, Migration(w.Namespace, w.Volume, map[string]any{
		"volume":             w.Volume,
		"targetStorageClass": targetClass,
		"window":             map[string]any{"duration": "30m"},
	}))
}

// Round runs one round of each controller, in the order the flow depends on:
// the application stops the workloads the migration waits for, the volume
// follows the binding the flow writes.
func (w *MigrationWorld) Round(t *testing.T) {
	t.Helper()
	w.ApplicationRound(t)
	w.VolumeRound(t)
	w.MigrationRound(t)
}

// ApplicationRound runs one application round.
func (w *MigrationWorld) ApplicationRound(t *testing.T) {
	t.Helper()
	ReconcileApplicationRound(t, w.App, w.Namespace, w.Application)
}

// VolumeRound runs one volume round.
func (w *MigrationWorld) VolumeRound(t *testing.T) {
	t.Helper()
	ReconcileVolume(t, w.Volumes, w.Namespace, w.Volume)
}

// MigrationRound runs one migration round.
func (w *MigrationWorld) MigrationRound(t *testing.T) {
	t.Helper()
	ReconcileMigration(t, w.Migrations, w.Namespace, w.Volume)
}

// Phase reads the stored migration phase.
func (w *MigrationWorld) Phase(t *testing.T, cluster *Cluster) string {
	t.Helper()
	return cluster.MigrationOf(t, w.Namespace, w.Volume).Status.Phase
}

// BindTarget plays the provisioner: the claim the flow created for the copy
// binds to a persistent volume of the target policy.
func (w *MigrationWorld) BindTarget(t *testing.T, cluster *Cluster, targetClass string) {
	t.Helper()
	claim := cluster.Claim(t, w.Namespace, w.TargetClaim)
	if phase, _, _ := unstructured.NestedString(claim.Object, "status", "phase"); phase == "Bound" {
		return
	}
	cluster.PersistentVolume(t, "pv-"+w.TargetClaim, targetClass, "network.example", "Retain")
	cluster.BindClaim(t, w.Namespace, w.TargetClaim, "pv-"+w.TargetClaim)
}

// CompleteStep plays the step runner: the Job reports that it completed, once.
func (w *MigrationWorld) CompleteStep(t *testing.T, cluster *Cluster, step string) {
	t.Helper()
	name := w.TargetClaim + "-" + step
	if cluster.JobGone(t, w.Namespace, name) {
		return
	}
	job, err := cluster.Direct(t, "batch", "v1", "Job", w.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return
	}
	if _, found, _ := unstructured.NestedSlice(job.Object, "status", "conditions"); found {
		return
	}
	cluster.JobOutcome(t, w.Namespace, name, false, "")
}

// FailStep makes one step Job report the reason the phase fails.
func (w *MigrationWorld) FailStep(t *testing.T, cluster *Cluster, step, message string) {
	t.Helper()
	name := w.TargetClaim + "-" + step
	if cluster.JobGone(t, w.Namespace, name) {
		return
	}
	job, err := cluster.Direct(t, "batch", "v1", "Job", w.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return
	}
	if _, found, _ := unstructured.NestedSlice(job.Object, "status", "conditions"); found {
		return
	}
	cluster.JobOutcome(t, w.Namespace, name, true, message)
}
