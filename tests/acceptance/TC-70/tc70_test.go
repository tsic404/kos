// Package tc70 configures a backup target on the source volume's own failure
// domain: the preflight refuses it with the same-domain reason, no success is
// recorded, and a same-domain copy is never presented as a backup (AC-34,
// TC-70, F12).
package tc70

import (
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/volume"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	namespace   = "apps"
	volumeName  = "data"
	policyName  = "nightly"
	owner       = "tsicliu"
	backupLabel = "backup"
	node        = "node-a"
)

// TestTC70RejectsSameFailureDomain puts the target on the node that holds the
// source volume and requires the preflight to refuse the policy (AC-34, TC-70).
func TestTC70RejectsSameFailureDomain(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, namespace)

	cluster.StorageClass(t, "standard", "standard.example", true, "Retain", nil)
	cluster.CreateIn(t, namespace, harness.KernelVolume(namespace, volumeName,
		string(v1beta1.VolumeMobilityNodeLocal), "1Gi", "standard", owner, backupLabel))
	volumeController := cluster.VolumeController(t, volume.Options{})
	harness.ReconcileVolume(t, volumeController, namespace, volumeName)
	cluster.PersistentVolumeOnNode(t, "pv-data", "standard", "standard.example", "Retain", node)
	cluster.BindClaim(t, namespace, volumeName, "pv-data")

	target := filepath.Join(t.TempDir(), "backups")
	cluster.CreateIn(t, namespace, harness.BackupPolicyManifest(namespace, policyName, map[string]any{
		"owner":       owner,
		"selector":    backupLabel + "=true",
		"schedule":    "0 3 * * *",
		"consistency": string(v1beta1.BackupConsistencyCrash),
		"target":      map[string]any{"path": target, "failureDomain": "node:" + node},
	}))
	backups := cluster.BackupSurface(t, filepath.Join(t.TempDir(), "volumes"))
	backups.Reconcile(t, namespace, policyName)

	policy := cluster.DecodeBackupPolicy(t, namespace, policyName)
	blocked, found := conditionOf(policy.Status.Conditions, api.ConditionReconciled)
	if !found {
		t.Fatalf("the policy carries no %s condition: %+v", api.ConditionReconciled, policy.Status.Conditions)
	}
	if blocked.Status != "False" || blocked.Reason != "SameFailureDomain" {
		t.Fatalf("Reconciled = %s/%s, want False/SameFailureDomain", blocked.Status, blocked.Reason)
	}
	if !strings.Contains(blocked.Message, "node:"+node) {
		t.Fatalf("refusal does not name the shared failure domain: %s", blocked.Message)
	}
	if !strings.Contains(blocked.Message, "快照") || !strings.Contains(blocked.Message, "不是备份") {
		t.Fatalf("refusal does not say a same-domain copy is a snapshot, not a backup: %s", blocked.Message)
	}
	if policy.Status.LastBackup != nil {
		t.Fatalf("a refused preflight recorded a run: %+v", policy.Status.LastBackup)
	}
	if !cluster.MissingCronJob(t, namespace, policyName) {
		t.Fatal("a refused policy materialized its schedule CronJob")
	}
}

// conditionOf finds one condition type in a status.
func conditionOf(conditions []metav1.Condition, conditionType string) (metav1.Condition, bool) {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}
