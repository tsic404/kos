// Package tc69 restores an application-consistent backup to a new instance:
// after the source volume's data is deleted, the new instance's row counts, file
// count, total bytes and content checksum equal the recorded baseline, and the
// source application object is unchanged (AC-34, TC-69).
package tc69

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/backup"
	"github.com/tsic404/kos/pkg/volume"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	namespace       = "apps"
	applicationName = "media"
	volumeName      = "data"
	policyName      = "nightly"
	targetApp       = "media-restored"
	owner           = "tsicliu"
	backupLabel     = "backup"
)

// applicationTemplate is the smallest render the definition validates; the case
// never renders it, because the backup and restore run on the recorded specs.
const applicationTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
`

// TestTC69RestoresToNewInstance takes one application-consistent backup, deletes
// the source volume's data, restores to a new instance and compares the four
// baseline items (AC-34, TC-69).
func TestTC69RestoresToNewInstance(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, namespace)

	dataRoot := filepath.Join(t.TempDir(), "volumes")
	sourceDirectory := filepath.Join(dataRoot, namespace, volumeName)
	harness.SeedVolumeData(t, sourceDirectory)
	target := filepath.Join(t.TempDir(), "backups")

	cluster.StorageClass(t, "standard", "standard.example", true, "Retain", map[string]string{"fsType": "ext4"})
	cluster.CreateIn(t, namespace, harness.KernelVolume(namespace, volumeName,
		string(v1beta1.VolumeMobilityClusterMovable), "1Gi", "standard", owner, backupLabel))
	volumeController := cluster.VolumeController(t, volume.Options{})
	harness.ReconcileVolume(t, volumeController, namespace, volumeName)
	cluster.PersistentVolumeOnNode(t, "pv-data", "standard", "standard.example", "Retain", "node-a")
	cluster.BindClaim(t, namespace, volumeName, "pv-data")

	cluster.InstallDefinition(t, harness.HookDefinition(t, "media", applicationTemplate,
		v1beta1.Hook{Name: backup.BackupHook, Command: []string{"sh", "-c", "exit 0"}}))
	application := harness.KernelApplication(namespace, applicationName, "media", owner, backupLabel)
	application.Object["spec"].(map[string]any)["volumes"] = []any{volumeName}
	cluster.CreateIn(t, namespace, application)

	cluster.CreateIn(t, namespace, harness.BackupPolicyManifest(namespace, policyName, map[string]any{
		"owner":       owner,
		"selector":    backupLabel + "=true",
		"schedule":    "0 3 * * *",
		"consistency": string(v1beta1.BackupConsistencyHook),
		"target":      map[string]any{"path": target, "failureDomain": "node:node-b"},
		"retention":   map[string]any{"keep": int64(2)},
	}))
	backups := cluster.BackupSurface(t, dataRoot)
	backups.Reconcile(t, namespace, policyName)
	policy := cluster.DecodeBackupPolicy(t, namespace, policyName)
	if policy.Status.LastBackup != nil {
		t.Fatalf("a run happened before the schedule triggered one: %+v", policy.Status.LastBackup)
	}
	if schedule, _, _ := unstructured.NestedString(cluster.CronJob(t, namespace, policyName).Object, "spec", "schedule"); schedule != "0 3 * * *" {
		t.Fatalf("CronJob schedule = %q, want the policy's schedule", schedule)
	}

	// The cluster's cron controller starts the first job; the kernel owes the
	// run that instant names.
	cluster.ScheduleCronJob(t, namespace, policyName, time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC))
	backups.Reconcile(t, namespace, policyName)
	backups.Reconcile(t, namespace, policyName)
	policy = cluster.DecodeBackupPolicy(t, namespace, policyName)
	if policy.Status.LastBackup == nil || !policy.Status.LastBackup.Succeeded {
		t.Fatalf("the backup did not succeed: %+v", policy.Status.LastBackup)
	}
	baseline := policy.Status.LastBackup.Baseline
	if baseline == nil {
		t.Fatal("the backup recorded no baseline")
	}
	if baseline.Files != 3 || len(baseline.Tables) != 2 {
		t.Fatalf("baseline = %d files, %d tables, want 3 files and 2 tables", baseline.Files, len(baseline.Tables))
	}

	before := harness.Flatten(cluster.GetIn(t, namespace, "v1beta1", "Application", applicationName).Object)

	// The source volume's data is deleted, then the volume object itself.
	if err := os.RemoveAll(sourceDirectory); err != nil {
		t.Fatalf("delete the source volume data: %v", err)
	}
	if err := cluster.Direct(t, api.Group, "v1beta1", "Volume", namespace).Delete(context.Background(), volumeName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the source volume: %v", err)
	}
	for range 4 {
		harness.ReconcileVolume(t, volumeController, namespace, volumeName)
	}
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "Volume", namespace).Get(context.Background(), volumeName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the source volume must be gone (err = %v)", err)
	}

	cluster.CreateIn(t, namespace, harness.RestoreJobManifest(namespace, "restore-1", map[string]any{
		"owner":             owner,
		"policy":            policyName,
		"targetApplication": targetApp,
	}))
	restores := cluster.RestoreSurface(t, dataRoot)
	for range 10 {
		restores.Reconcile(t, namespace, "restore-1")
		phase := cluster.DecodeRestoreJob(t, namespace, "restore-1").Status.Phase
		if phase == v1beta1.RestorePhaseSucceeded || phase == v1beta1.RestorePhaseFailed {
			break
		}
	}
	job := cluster.DecodeRestoreJob(t, namespace, "restore-1")
	if job.Status.Phase != v1beta1.RestorePhaseSucceeded {
		t.Fatalf("restore phase = %q, want %s: %s", job.Status.Phase, v1beta1.RestorePhaseSucceeded, failureReason(job))
	}

	// The new instance's four items equal the baseline, measured from the
	// restored volume rather than read back from the status.
	restoredDirectory := filepath.Join(dataRoot, namespace, targetApp+"-"+volumeName)
	observed, err := backup.Measure([]backup.VolumeDir{{Volume: volumeName, Path: restoredDirectory}})
	if err != nil {
		t.Fatalf("measure the restored volume: %v", err)
	}
	if diffs := backup.Compare(*baseline, observed); len(diffs) != 0 {
		t.Fatalf("the restored instance differs from the baseline: %v", backup.DescribeDiffs(diffs))
	}
	if observed.Files != baseline.Files || observed.Bytes != baseline.Bytes || observed.Checksum != baseline.Checksum {
		t.Fatalf("restored = %d files/%d bytes/%s, want %d/%d/%s",
			observed.Files, observed.Bytes, observed.Checksum, baseline.Files, baseline.Bytes, baseline.Checksum)
	}

	after := harness.Flatten(cluster.GetIn(t, namespace, "v1beta1", "Application", applicationName).Object)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the source application changed: before %v, after %v", before, after)
	}
	cluster.GetIn(t, namespace, "v1beta1", "Application", targetApp)
	cluster.GetIn(t, namespace, "v1beta1", "Volume", targetApp+"-"+volumeName)
}

// failureReason renders why a restore stopped, so a failing case shows the cause
// without a second run.
func failureReason(job *v1beta1.RestoreJob) string {
	for _, condition := range job.Status.Conditions {
		if condition.Type == api.ConditionReconciled || condition.Status != metav1.ConditionTrue {
			continue
		}
		return condition.Reason + ": " + condition.Message
	}
	return "no failure condition"
}
