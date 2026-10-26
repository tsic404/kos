package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/write"
)

// TestExportAndRestore covers the daily export and the offline restore: the
// restored database holds the accounts, preferences and audit of the export
// instant and nothing written after it (C-61).
func TestExportAndRestore(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock()
	directory := t.TempDir()
	path := filepath.Join(directory, "kos.db")
	store, err := Open(ctx, OpenOptions{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	service := New(store, Config{Now: clock.Now, Owned: &fakeOwned{objects: map[string][]OwnedObject{}}})
	ownerName, ownerPassword := bootstrapOwner(t, service)
	ownerLogin, err := service.Login(ctx, ownerName, ownerPassword)
	if err != nil {
		t.Fatalf("login owner: %v", err)
	}
	if err := store.SetPreference(ctx, ownerName, "theme", "dark"); err != nil {
		t.Fatalf("write preference: %v", err)
	}
	exportedAt := clock.Now()
	if err := store.AppendAudit(ctx, AuditEntry{
		Actor: ownerName, Object: "Application default/web", Verb: "create",
		Result: "accepted", Source: "api", OccurredAt: exportedAt,
	}); err != nil {
		t.Fatalf("append audit: %v", err)
	}
	exporter := &Exporter{Store: store, Target: filepath.Join(directory, "backup"), Now: clock.Now}
	result, err := exporter.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if result.Skipped || result.Path == "" {
		t.Fatalf("export = %+v, want a file", result)
	}
	info, err := os.Stat(result.Path)
	if err != nil {
		t.Fatalf("stat export: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the export file is empty")
	}
	if status := exporter.Status(); status.LastSuccessAt.IsZero() || status.Path != result.Path {
		t.Fatalf("export status = %+v, want a recorded success", status)
	}

	// Changes after the export must not survive the restore.
	clock.Advance(time.Hour)
	if err := store.SetPreference(ctx, ownerName, "theme", "light"); err != nil {
		t.Fatalf("overwrite preference: %v", err)
	}
	if err := store.AppendAudit(ctx, AuditEntry{
		Actor: ownerName, Object: "Volume default/data", Verb: "delete",
		Result: "accepted", Source: "api", OccurredAt: clock.Now(),
	}); err != nil {
		t.Fatalf("append later audit: %v", err)
	}
	if _, err := service.CreateAccount(ctx, ownerLogin.Session, NewAccount{Username: "late", Role: write.RoleMember}); err != nil {
		t.Fatalf("create a post-export account: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	for _, sidecar := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("remove %s: %v", sidecar, err)
		}
	}
	if err := Restore(ctx, result.Path, path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored, err := Open(ctx, OpenOptions{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("reopen restored database: %v", err)
	}
	defer func() { _ = restored.Close() }()
	restoredService := New(restored, Config{Now: clock.Now, Owned: &fakeOwned{objects: map[string][]OwnedObject{}}})
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
	if len(audit) != 1 || audit[0].Actor != ownerName || !audit[0].OccurredAt.Equal(exportedAt) {
		t.Fatalf("audit after restore = %+v, want the one record from the export", audit)
	}
	if _, err := restored.UserByName(ctx, "late"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a post-export account survived the restore: %v", err)
	}
}

// TestExportWithoutTarget checks the observable hint C-61 requires when no
// backup target is configured.
func TestExportWithoutTarget(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, OpenOptions{Path: filepath.Join(t.TempDir(), "kos.db"), Now: newTestClock().Now})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = store.Close() }()
	exporter := &Exporter{Store: store}
	result, err := exporter.Export(ctx)
	if err != nil {
		t.Fatalf("export without a target: %v", err)
	}
	if !result.Skipped || result.Reason == "" {
		t.Fatalf("export = %+v, want a skip with a reason", result)
	}
	if status := exporter.Status(); !status.Skipped || status.Reason == "" {
		t.Fatalf("status = %+v, want the skip recorded", status)
	}
}

// TestRestoreRejectsDamagedExport keeps a damaged file from replacing the
// database.
func TestRestoreRejectsDamagedExport(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	target := filepath.Join(directory, "kos.db")
	if err := os.WriteFile(target, []byte("current"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	damaged := filepath.Join(directory, "damaged.db")
	if err := os.WriteFile(damaged, []byte("this is not a database"), 0o600); err != nil {
		t.Fatalf("write damaged export: %v", err)
	}
	if err := Restore(ctx, damaged, target); err == nil {
		t.Fatal("a damaged export was accepted")
	}
	current, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(current) != "current" {
		t.Fatalf("the target changed to %q", current)
	}
}

// TestNextDailyRun checks the daily trigger lands on the next occurrence.
func TestNextDailyRun(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	next := nextDailyRun(now, 3*time.Hour)
	want := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next run = %s, want %s", next, want)
	}
	earlier := nextDailyRun(now, 13*time.Hour)
	if want := time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC); !earlier.Equal(want) {
		t.Fatalf("next run = %s, want %s", earlier, want)
	}
}

// TestRunExportsDaily drives the scheduler with the injected clock and timer:
// the first wait lands on the next daily instant, the fire exports, and the
// next wait is a full day (C-61).
func TestRunExportsDaily(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock()
	store, err := Open(ctx, OpenOptions{Path: filepath.Join(t.TempDir(), "kos.db"), Now: clock.Now})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = store.Close() }()
	exporter := &Exporter{
		Store:  store,
		Target: filepath.Join(t.TempDir(), "backup"),
		Now:    clock.Now,
	}
	requested := make(chan time.Duration, 1)
	fire := make(chan time.Time, 1)
	exporter.After = func(_ context.Context, duration time.Duration) <-chan time.Time {
		requested <- duration
		return fire
	}
	runContext, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		exporter.Run(runContext, 3*time.Hour)
		close(done)
	}()
	select {
	case duration := <-requested:
		if duration != 3*time.Hour {
			t.Fatalf("first wait = %s, want 3h to the next 03:00", duration)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the scheduler never asked for its first wait")
	}
	clock.Advance(3 * time.Hour)
	fire <- clock.Now()
	exported := filepath.Join(exporter.Target, "kos-db-20260101.db")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(exported); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the scheduler did not write %s", exported)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case duration := <-requested:
		if duration != 24*time.Hour {
			t.Fatalf("next wait = %s, want 24h", duration)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the scheduler never asked for its next wait")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	if status := exporter.Status(); status.LastSuccessAt.IsZero() {
		t.Fatalf("status = %+v, want the run recorded", status)
	}
}
