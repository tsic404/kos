package identity

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// loginAttemptRows counts the stored counter rows of one login name.
func loginAttemptRows(t *testing.T, store *Store, name string) int {
	t.Helper()
	db, err := sql.Open("sqlite", writableDSN(store.Path()))
	if err != nil {
		t.Fatalf("open inspection connection: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM login_attempts WHERE login_name = ?`, name).Scan(&count); err != nil {
		t.Fatalf("count login attempts for %q: %v", name, err)
	}
	return count
}

// TestLoginAttemptsAreBounded checks that the unauthenticated counter table
// cannot grow without end: an unlocked counter expires after the retention
// window, a lock row disappears once its window closed, and a name too long to
// be an account never becomes a key (C-63).
func TestLoginAttemptsAreBounded(t *testing.T) {
	service, clock, store := newTestServiceWithStore(t, &fakeOwned{})
	ctx := context.Background()

	for range LoginFailureLimit - 1 {
		if _, err := service.Login(ctx, "stale", "wrong-password"); !errors.Is(err, ErrLoginFailed) {
			t.Fatalf("counted failure = %v, want a plain login failure", err)
		}
	}
	if rows := loginAttemptRows(t, store, "stale"); rows != 1 {
		t.Fatalf("counter rows for an unlocked name = %d, want 1", rows)
	}
	clock.Advance(LoginAttemptRetention + time.Minute)
	if _, err := service.Login(ctx, "fresh", "wrong-password"); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("failure that triggers the prune = %v", err)
	}
	if rows := loginAttemptRows(t, store, "stale"); rows != 0 {
		t.Fatalf("an unlocked counter older than the retention left %d row(s)", rows)
	}

	// An expired lock is pruned on the next failure even though its counter
	// never reached a later login.
	for range LoginFailureLimit {
		if _, err := service.Login(ctx, "locked", "wrong-password"); !errors.Is(err, ErrLoginFailed) {
			t.Fatalf("lockout failure = %v", err)
		}
	}
	if rows := loginAttemptRows(t, store, "locked"); rows != 1 {
		t.Fatalf("counter rows for a locked name = %d, want 1", rows)
	}
	clock.Advance(LoginLockDuration + time.Minute)
	if _, err := service.Login(ctx, "fresh-2", "wrong-password"); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("failure that prunes the expired lock = %v", err)
	}
	if rows := loginAttemptRows(t, store, "locked"); rows != 0 {
		t.Fatalf("an expired lock left %d row(s)", rows)
	}

	// A name longer than any account can have is refused before it reaches the
	// counter, so the row key cannot approach the request-body limit.
	overlong := strings.Repeat("a", MaxUsernameLength+1)
	if _, err := service.Login(ctx, overlong, "wrong-password"); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("login with an overlong name = %v, want the shared failure", err)
	}
	if rows := loginAttemptRows(t, store, overlong); rows != 0 {
		t.Fatalf("an overlong login name stored %d row(s)", rows)
	}
}
