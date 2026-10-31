// Package tc71 drills a backup whose restored data was tampered with: writing
// one extra file into the drill destination fails the drill, and the backup
// status shows the failure with the differing items listed (AC-35, TC-71).
package tc71

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	owner           = "tsicliu"
	backupLabel     = "backup"
)

// applicationTemplate is the smallest render the definition validates; the case
// never renders it, because the backup and drill run on the recorded specs.
const applicationTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
`

// TestTC71TamperedDrillFailsTheBackup writes an extra file into the drill
// destination and requires the drill and the backup it verified to fail with the
// differences listed (AC-35, TC-71).
func TestTC71TamperedDrillFailsTheBackup(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, namespace)

	dataRoot := filepath.Join(t.TempDir(), "volumes")
	harness.SeedVolumeData(t, filepath.Join(dataRoot, namespace, volumeName))
	target := filepath.Join(t.TempDir(), "backups")

	cluster.StorageClass(t, "standard", "standard.example", true, "Retain", nil)
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
		"drill":       map[string]any{"enabled": true},
	}))
	backups := cluster.BackupSurface(t, dataRoot)
	backups.Reconcile(t, namespace, policyName)

	cluster.ScheduleCronJob(t, namespace, policyName, time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC))
	backups.Reconcile(t, namespace, policyName)
	backups.Reconcile(t, namespace, policyName)
	policy := cluster.DecodeBackupPolicy(t, namespace, policyName)
	if policy.Status.LastBackup == nil || !policy.Status.LastBackup.Succeeded {
		t.Fatalf("the backup did not succeed: %+v", policy.Status.LastBackup)
	}
	if policy.Status.LastDrill == nil || policy.Status.LastDrill.Phase != v1beta1.DrillPhaseRestoring {
		t.Fatalf("the drill did not start: %+v", policy.Status.LastDrill)
	}

	// The drill copied the run into its scratch directory before this round
	// returned; writing into that target is what the next verification reads.
	run := policy.Status.LastBackup.Path
	harness.WriteFixtureFile(t, filepath.Join(run, backup.DrillDir, volumeName, "extra.txt"), "tampered")
	backups.Reconcile(t, namespace, policyName)
	policy = cluster.DecodeBackupPolicy(t, namespace, policyName)
	if policy.Status.LastDrill.Phase != v1beta1.DrillPhaseVerifying {
		t.Fatalf("drill phase = %q, want %s", policy.Status.LastDrill.Phase, v1beta1.DrillPhaseVerifying)
	}

	// The verification measures the tampered copy against the baseline.
	backups.Reconcile(t, namespace, policyName)
	policy = cluster.DecodeBackupPolicy(t, namespace, policyName)
	drill := policy.Status.LastDrill
	if drill == nil || drill.Phase != v1beta1.DrillPhaseFailed || drill.Succeeded {
		t.Fatalf("drill = %+v, want a failed drill", drill)
	}
	if !strings.Contains(drill.Reason, "演练") || !strings.Contains(drill.Reason, "不一致") {
		t.Fatalf("drill reason %q does not point at the drill inconsistency", drill.Reason)
	}
	if policy.Status.LastBackup.Succeeded {
		t.Fatal("a failed drill left the backup reported as a success")
	}
	if !strings.Contains(policy.Status.LastBackup.Reason, "演练") {
		t.Fatalf("backup reason %q does not point at the drill", policy.Status.LastBackup.Reason)
	}
	items := make(map[string]bool, len(policy.Status.LastBackup.Diffs))
	for _, diff := range policy.Status.LastBackup.Diffs {
		items[diff.Item] = true
	}
	if !items["files"] && !items["bytes"] {
		t.Fatalf("the differences do not name the file count or the bytes: %+v", policy.Status.LastBackup.Diffs)
	}
	condition := conditionOf(policy.Status.Conditions, "DrillMismatch")
	if condition == nil {
		t.Fatalf("the policy carries no DrillMismatch condition: %+v", policy.Status.Conditions)
	}
	if condition.Status != metav1.ConditionTrue {
		t.Fatalf("DrillMismatch = %s, want True", condition.Status)
	}
}

// conditionOf returns the condition of one type; a missing one is nil.
func conditionOf(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for index, condition := range conditions {
		if condition.Type == conditionType {
			return &conditions[index]
		}
	}
	return nil
}
