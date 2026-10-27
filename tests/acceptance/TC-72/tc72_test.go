// Package tc72 verifies the daily database export and the offline restore:
// the export file exists, and after a restore accounts, preferences and audit
// are back at the exported instant (AC-36, TC-72).
package tc72

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	ownerName     = "owner"
	ownerPassword = "owner-password"
)

// TestTC72DatabaseRestore exports once, changes preferences and audit, then
// restores the file offline and reads the state of the export back.
func TestTC72DatabaseRestore(t *testing.T) {
	ctx := context.Background()
	surface := harness.StartIdentity(t, nil, nil)
	oneTime := surface.BootstrapOwner(t, ownerName)
	token, _ := surface.SignIn(t, ownerName, oneTime)
	surface.ChangePassword(t, token, oneTime, ownerPassword)

	exportedAt := surface.Clock.Now()
	if err := surface.Store.SetPreference(ctx, ownerName, "theme", "dark"); err != nil {
		t.Fatalf("write preference: %v", err)
	}
	if err := surface.Store.AppendAudit(ctx, identity.AuditEntry{
		Actor: ownerName, Object: "Application default/web", Verb: "create",
		Result: "accepted", Source: "api", OccurredAt: exportedAt,
	}); err != nil {
		t.Fatalf("append audit: %v", err)
	}

	exporter := &identity.Exporter{
		Store:  surface.Store,
		Target: filepath.Join(t.TempDir(), "backup"),
		Now:    surface.Clock.Now,
	}
	result, err := exporter.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if result.Skipped || result.Path == "" {
		t.Fatalf("export = %+v, want a written file", result)
	}
	info, err := os.Stat(result.Path)
	if err != nil {
		t.Fatalf("stat export: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the export file is empty")
	}

	// Everything below is written after the export and must not survive.
	surface.Clock.Advance(time.Hour)
	if err := surface.Store.SetPreference(ctx, ownerName, "theme", "light"); err != nil {
		t.Fatalf("overwrite preference: %v", err)
	}
	if err := surface.Store.AppendAudit(ctx, identity.AuditEntry{
		Actor: ownerName, Object: "Volume default/data", Verb: "delete",
		Result: "accepted", Source: "api", OccurredAt: surface.Clock.Now(),
	}); err != nil {
		t.Fatalf("append later audit: %v", err)
	}
	ownerSession, err := surface.Service.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate owner: %v", err)
	}
	if _, err := surface.Service.CreateAccount(ctx, ownerSession, identity.NewAccount{
		Username: "late", Role: write.RoleMember,
	}); err != nil {
		t.Fatalf("create a post-export account: %v", err)
	}

	// The restore is offline: the control plane stops before the file moves.
	if err := surface.Store.Close(); err != nil {
		t.Fatalf("stop the database: %v", err)
	}
	for _, sidecar := range []string{surface.DatabasePath, surface.DatabasePath + "-wal", surface.DatabasePath + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("remove %s: %v", sidecar, err)
		}
	}
	if err := identity.Restore(ctx, result.Path, surface.DatabasePath); err != nil {
		t.Fatalf("restore: %v", err)
	}

	restored, err := identity.Open(ctx, identity.OpenOptions{Path: surface.DatabasePath, Now: surface.Clock.Now})
	if err != nil {
		t.Fatalf("open the restored database: %v", err)
	}
	defer func() { _ = restored.Close() }()
	restoredService := identity.New(restored, identity.Config{Now: surface.Clock.Now})
	if _, err := restoredService.Login(ctx, ownerName, ownerPassword); err != nil {
		t.Fatalf("login after restore: %v", err)
	}
	preferences, err := restored.Preferences(ctx, ownerName)
	if err != nil {
		t.Fatalf("read preferences: %v", err)
	}
	if len(preferences) != 1 || preferences[0].Key != "theme" || preferences[0].Value != "dark" {
		t.Fatalf("preferences after restore = %+v, want the export value", preferences)
	}
	if !preferences[0].UpdatedAt.Equal(exportedAt) {
		t.Fatalf("preference time = %s, want the export instant %s", preferences[0].UpdatedAt, exportedAt)
	}
	audit, err := restored.AuditEntries(ctx, 10)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	// The export holds the probe record and every write the case made before
	// it — the account writes carry audit records of their own (C-14) — and
	// none of the writes after the export survive.
	probe := false
	for _, entry := range audit {
		if entry.Object == "Volume default/data" {
			t.Fatalf("a post-export audit record survived the restore: %+v", audit)
		}
		if entry.Object == "Application default/web" && entry.OccurredAt.Equal(exportedAt) {
			probe = true
		}
		if entry.OccurredAt.After(exportedAt) {
			t.Fatalf("a record written later survived the restore: %+v", entry)
		}
	}
	if !probe {
		t.Fatalf("the audit record of the export instant is missing: %+v", audit)
	}
	if _, err := restored.UserByName(ctx, "late"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("a post-export account survived the restore: %v", err)
	}
}
