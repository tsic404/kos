package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/tsic404/kos/pkg/write"
)

// schemaSet is the declared preference keys one extension contributes.
type schemaSet map[string][]string

// PreferenceKeys implements PreferenceSchemas.
func (s schemaSet) PreferenceKeys(identifier string) ([]string, bool) {
	keys, ok := s[identifier]
	return keys, ok
}

// preferenceService opens a private database with one owner and the given
// declared schemas.
func preferenceService(t *testing.T, schemas schemaSet) *Service {
	t.Helper()
	store, err := Open(context.Background(), OpenOptions{Path: t.TempDir() + "/kos.db"})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := New(store, Config{Preferences: schemas})
	if _, err := service.BootstrapOwner(context.Background(), "owner"); err != nil {
		t.Fatalf("bootstrap owner: %v", err)
	}
	return service
}

// TestPreferenceIsScopedByAccountAndExtension checks that a declared key is
// stored under the extension domain, that an undeclared key is refused and that
// a stored key outside the schema is ignored (C-27).
func TestPreferenceIsScopedByAccountAndExtension(t *testing.T) {
	ctx := context.Background()
	service := preferenceService(t, schemaSet{"demo.dock": {"view"}})
	session := Session{Username: "owner", Role: write.RoleOwner}

	if err := service.SetPreference(ctx, session, "demo.dock", "view", "grid"); err != nil {
		t.Fatalf("write the declared key: %v", err)
	}
	if err := service.SetPreference(ctx, session, "demo.dock", "undeclared", "x"); err == nil {
		t.Fatal("an undeclared key was accepted")
	}
	if err := service.SetPreference(ctx, session, "demo.other", "view", "grid"); !errors.Is(err, ErrPreferenceDomain) {
		t.Fatalf("unknown domain = %v, want ErrPreferenceDomain", err)
	}
	if err := service.store.SetScopedPreference(ctx, "owner", "demo.dock", "stale", "x"); err != nil {
		t.Fatalf("store an unknown key: %v", err)
	}

	values, err := service.Preferences(ctx, session, "demo.dock")
	if err != nil {
		t.Fatalf("read preferences: %v", err)
	}
	if values["view"] != "grid" {
		t.Fatalf("view = %q, want grid", values["view"])
	}
	if _, found := values["stale"]; found {
		t.Fatalf("values = %v, want the unknown key ignored", values)
	}
}

// TestGuestCannotWriteAPreference checks C-59 on the preference surface.
func TestGuestCannotWriteAPreference(t *testing.T) {
	service := preferenceService(t, schemaSet{"demo.dock": {"view"}})
	guest := Session{Username: "owner", Role: write.RoleGuest}
	if err := service.SetPreference(context.Background(), guest, "demo.dock", "view", "grid"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("guest write = %v, want ErrPermissionDenied", err)
	}
}

// TestWithdrawAppliesTheDeclaredFate checks that a withdrawn extension's
// preferences are deleted unless its declaration kept them (C-27, C-29).
func TestWithdrawAppliesTheDeclaredFate(t *testing.T) {
	ctx := context.Background()
	service := preferenceService(t, schemaSet{"demo.keep": {"layout"}, "demo.drop": {"layout"}})
	session := Session{Username: "owner", Role: write.RoleOwner}
	for _, identifier := range []string{"demo.keep", "demo.drop"} {
		if err := service.SetPreference(ctx, session, identifier, "layout", identifier); err != nil {
			t.Fatalf("write %s: %v", identifier, err)
		}
	}

	// Both are installed, and the declarations say what happens when they are
	// not.
	if err := service.Retain(ctx, "keep-component", "demo.keep", true); err != nil {
		t.Fatalf("retain keep: %v", err)
	}
	if err := service.Retain(ctx, "drop-component", "demo.drop", false); err != nil {
		t.Fatalf("retain drop: %v", err)
	}
	// Reinstalling them keeps the store untouched while they stay installed.
	present := map[string]string{"keep-component": "demo.keep", "drop-component": "demo.drop"}
	if err := service.Withdraw(ctx, present); err != nil {
		t.Fatalf("withdraw with both installed: %v", err)
	}

	// Both component objects are gone: only the kept domain survives.
	if err := service.Withdraw(ctx, map[string]string{}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	kept, err := service.store.PreferencesOf(ctx, "owner", "demo.keep")
	if err != nil {
		t.Fatalf("read keep: %v", err)
	}
	if kept["layout"] != "demo.keep" {
		t.Fatalf("kept = %v, want the retained value", kept)
	}
	dropped, err := service.store.PreferencesOf(ctx, "owner", "demo.drop")
	if err != nil {
		t.Fatalf("read drop: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v, want the deleted domain empty", dropped)
	}
}

// TestWithdrawKeepsDomainsOfLiveComponents checks the rule the install pass
// depends on: a component object that still exists keeps its domains, while a
// component that now declares another extension withdraws the old one (C-27,
// C-29).
func TestWithdrawKeepsDomainsOfLiveComponents(t *testing.T) {
	ctx := context.Background()
	service := preferenceService(t, schemaSet{"demo.drop": {"layout"}})
	session := Session{Username: "owner", Role: write.RoleOwner}
	if err := service.SetPreference(ctx, session, "demo.drop", "layout", "kept"); err != nil {
		t.Fatalf("write the preference: %v", err)
	}
	if err := service.Retain(ctx, "drop-component", "demo.drop", false); err != nil {
		t.Fatalf("retain: %v", err)
	}

	// An unreadable declaration reports no identifier: the install pass must not
	// treat that as a withdrawal.
	if err := service.Withdraw(ctx, map[string]string{"drop-component": ""}); err != nil {
		t.Fatalf("withdraw with an unread declaration: %v", err)
	}
	values, err := service.store.PreferencesOf(ctx, "owner", "demo.drop")
	if err != nil || values["layout"] != "kept" {
		t.Fatalf("values after an unread declaration = %v / %v, want the preference kept", values, err)
	}

	// The same object now declares a different extension: the old domain is
	// withdrawn.
	if err := service.Withdraw(ctx, map[string]string{"drop-component": "demo.other"}); err != nil {
		t.Fatalf("withdraw after the identifier changed: %v", err)
	}
	values, err = service.store.PreferencesOf(ctx, "owner", "demo.drop")
	if err != nil || len(values) != 0 {
		t.Fatalf("values after the identifier changed = %v / %v, want the domain withdrawn", values, err)
	}
}
