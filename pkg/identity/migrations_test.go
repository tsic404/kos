package identity

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigrationsApplyAndRollback checks that Open brings a fresh file to the
// newest version, a second Open applies nothing and Rollback reverses the
// newest script (schema migrations are versioned and reversible).
func TestMigrationsApplyAndRollback(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kos.db")
	clock := newTestClock()
	latest, err := latestSchemaVersion()
	if err != nil {
		t.Fatalf("read newest version: %v", err)
	}
	store, err := Open(ctx, OpenOptions{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != latest {
		t.Fatalf("schema version = %d, want %d", version, latest)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	// Reopening an up-to-date database changes nothing.
	store, err = Open(ctx, OpenOptions{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	if version, err = store.SchemaVersion(ctx); err != nil || version != latest {
		t.Fatalf("reopened schema = %d, %v; want %d", version, err, latest)
	}
	// Every version is reversible: rolling back once per applied script leaves
	// no schema behind, newest first.
	for want := latest; want > 0; want-- {
		rolled, err := store.Rollback(ctx)
		if err != nil {
			t.Fatalf("roll back %d: %v", want, err)
		}
		if rolled != want {
			t.Fatalf("rolled back version = %d, want %d", rolled, want)
		}
	}
	if version, err = store.SchemaVersion(ctx); err != nil || version != 0 {
		t.Fatalf("schema after rollback = %d, %v; want 0", version, err)
	}
	if _, err := store.UserByName(ctx, "nobody"); err == nil {
		t.Fatal("the users table still answers after its rollback")
	}
	if _, err := store.Rollback(ctx); err == nil {
		t.Fatal("a rollback with no migration left to reverse succeeded")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	// Opening the rolled-back file applies the schema again.
	store, err = Open(ctx, OpenOptions{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("reopen after rollback: %v", err)
	}
	defer func() { _ = store.Close() }()
	if version, err = store.SchemaVersion(ctx); err != nil || version != latest {
		t.Fatalf("schema after reopening a rolled-back file = %d, %v; want %d", version, err, latest)
	}
}
