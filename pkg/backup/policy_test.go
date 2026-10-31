package backup

import (
	"testing"
	"time"

	"github.com/tsic404/kos/api/v1beta1"
)

// TestValidatePolicyShape covers the elements C-41 requires: a selector, a
// schedule that can actually run, a consistency level, a target and a
// well-formed failure domain, with the target falling back to the deployment's
// reserved disk.
func TestValidatePolicyShape(t *testing.T) {
	now := time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC)
	valid := v1beta1.BackupPolicySpec{
		Selector:    "backup=true",
		Schedule:    "0 3 * * *",
		Consistency: v1beta1.BackupConsistencyHook,
		Target:      v1beta1.BackupTarget{Path: "/backups", FailureDomain: "node:node-a"},
	}
	if _, _, problem := validatePolicy(valid, "", now); problem != "" {
		t.Fatalf("a complete policy was refused: %s", problem)
	}

	withoutTarget := valid
	withoutTarget.Target = v1beta1.BackupTarget{FailureDomain: "node:node-a"}
	if _, _, problem := validatePolicy(withoutTarget, "/reserved", now); problem != "" {
		t.Fatalf("the deployment default target was not used: %s", problem)
	}
	if _, _, problem := validatePolicy(withoutTarget, "", now); problem == "" {
		t.Fatal("a policy without a policy target or a deployment target was accepted")
	}

	cases := map[string]v1beta1.BackupPolicySpec{
		"no selector":     {Schedule: "0 3 * * *", Consistency: v1beta1.BackupConsistencyCrash, Target: v1beta1.BackupTarget{Path: "/backups"}},
		"bad selector":    {Selector: "a in (", Schedule: "0 3 * * *", Consistency: v1beta1.BackupConsistencyCrash, Target: v1beta1.BackupTarget{Path: "/backups"}},
		"bad schedule":    {Selector: "a=b", Schedule: "noon", Consistency: v1beta1.BackupConsistencyCrash, Target: v1beta1.BackupTarget{Path: "/backups"}},
		"never matches":   {Selector: "a=b", Schedule: "0 0 31 4 *", Consistency: v1beta1.BackupConsistencyCrash, Target: v1beta1.BackupTarget{Path: "/backups"}},
		"no consistency":  {Selector: "a=b", Schedule: "0 3 * * *", Target: v1beta1.BackupTarget{Path: "/backups"}},
		"unknown level":   {Selector: "a=b", Schedule: "0 3 * * *", Consistency: "eventually", Target: v1beta1.BackupTarget{Path: "/backups"}},
		"bare domain":     {Selector: "a=b", Schedule: "0 3 * * *", Consistency: v1beta1.BackupConsistencyCrash, Target: v1beta1.BackupTarget{Path: "/backups", FailureDomain: "node-a"}},
		"empty domain":    {Selector: "a=b", Schedule: "0 3 * * *", Consistency: v1beta1.BackupConsistencyCrash, Target: v1beta1.BackupTarget{Path: "/backups", FailureDomain: "node:"}},
		"negative retain": {Selector: "a=b", Schedule: "0 3 * * *", Consistency: v1beta1.BackupConsistencyCrash, Target: v1beta1.BackupTarget{Path: "/backups"}, Retention: v1beta1.BackupRetention{Keep: -1}},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, problem := validatePolicy(spec, "/reserved", now); problem == "" {
				t.Fatalf("spec %+v was accepted", spec)
			}
		})
	}
}

// TestValidateWorkerImage covers the digest pin the schedule job's component
// reference must carry (C-17, C-41).
func TestValidateWorkerImage(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if problem := validateWorkerImage("example.test/kos-backup@sha256:" + digest); problem != "" {
		t.Fatalf("a digest-pinned image was refused: %s", problem)
	}
	for _, image := range []string{
		"",
		"example.test/kos-backup:latest",
		"example.test/kos-backup@sha256:abc",
		"example.test/kos-backup@sha256:" + digest[:63],
		"example.test/kos-backup@sha256:" + digest[:63] + "z",
	} {
		if problem := validateWorkerImage(image); problem == "" {
			t.Fatalf("image %q was accepted", image)
		}
	}
}
