// Package tc113 runs the kernel upgrade preflight against installed extensions:
// the extension that only runs on the removed framework version blocks the
// upgrade and is listed, one that also runs on the target does not, and
// removing the blocking dependency lets the preflight pass (AC-55, TC-113).
package tc113

import (
	"context"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	oldOnly = "kubevirt"
	both    = "kubevirt-next"
)

// declaration names one framework target range.
func declaration(identifier, frameworkVersion string) extension.Declaration {
	return extension.Declaration{
		Identifier:       identifier,
		Version:          "1.0.0",
		FrameworkVersion: frameworkVersion,
		Contributions: extension.Contributions{
			Settings: []extension.Setting{{
				ID: identifier + "-endpoint", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser,
			}},
		},
	}
}

// TestTC113PreflightListsFrameworkDependents checks the blocked and cleared
// preflight.
func TestTC113PreflightListsFrameworkDependents(t *testing.T) {
	cluster := harness.Start(t)
	artifacts := extension.MemoryArtifacts{}
	cluster.InstallPackage(t, oldOnly, declaration(oldOnly, ">=1.0 <2.0"), artifacts)
	cluster.InstallPackage(t, both, declaration(both, ">=1.0"), artifacts)
	installer := cluster.ExtensionInstaller(artifacts)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	preflight, err := installer.Preflight(ctx, []string{"2.0"})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(preflight.Removed) != 1 || preflight.Removed[0] != "1.1" {
		t.Fatalf("removed versions = %v, want the served 1.1", preflight.Removed)
	}
	if !preflight.IsBlocked {
		t.Fatal("the upgrade is not blocked while an extension only runs on the removed version")
	}
	if len(preflight.Dependents) != 1 || preflight.Dependents[0].Component != oldOnly {
		t.Fatalf("dependents = %+v, want only %s", preflight.Dependents, oldOnly)
	}

	// Releasing the dependency lets the same upgrade pass.
	cluster.DeleteComponent(t, oldOnly)
	cluster.ExtensionState(t, artifacts)
	cleared, err := installer.Preflight(ctx, []string{"2.0"})
	if err != nil {
		t.Fatalf("preflight after the dependency left: %v", err)
	}
	if cleared.IsBlocked || len(cleared.Dependents) != 0 {
		t.Fatalf("preflight still blocked: %+v", cleared)
	}
}
