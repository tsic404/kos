package identity

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/write"
)

// testClock is the service clock the cases move by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now reads the clock.
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

// newTestClock starts at a fixed instant so lock and session arithmetic is
// exact.
func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

// fakeOwned is the ownership source the cases control.
type fakeOwned struct {
	mu      sync.Mutex
	objects map[string][]OwnedObject
	failure error
}

// ListOwned returns the objects the case put under the name.
func (f *fakeOwned) ListOwned(_ context.Context, username string) ([]OwnedObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure != nil {
		return nil, f.failure
	}
	return f.objects[username], nil
}

// setOwned replaces the objects an account owns.
func (f *fakeOwned) setOwned(username string, objects ...OwnedObject) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[username] = objects
}

// newTestService opens a private database and the service over it.
func newTestService(t *testing.T, owned OwnedObjects) (*Service, *testClock) {
	t.Helper()
	clock := newTestClock()
	store, err := Open(context.Background(), OpenOptions{
		Path: filepath.Join(t.TempDir(), "kos.db"),
		Now:  clock.Now,
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	return New(store, Config{Now: clock.Now, Owned: owned}), clock
}

// bootstrapOwner creates the first Owner and replaces its one-time password.
func bootstrapOwner(t *testing.T, service *Service) (string, string) {
	t.Helper()
	ctx := context.Background()
	bootstrap, err := service.BootstrapOwner(ctx, "owner")
	if err != nil {
		t.Fatalf("bootstrap owner: %v", err)
	}
	if !bootstrap.Created || bootstrap.OneTimePassword == "" {
		t.Fatalf("bootstrap = %+v, want a created account with a one-time password", bootstrap)
	}
	login, err := service.Login(ctx, bootstrap.Username, bootstrap.OneTimePassword)
	if err != nil {
		t.Fatalf("login with one-time password: %v", err)
	}
	if !login.Session.MustChangePassword {
		t.Fatal("the first login did not require a password change")
	}
	password := "owner-password"
	if err := service.ChangePassword(ctx, login.Token, bootstrap.OneTimePassword, password); err != nil {
		t.Fatalf("change first password: %v", err)
	}
	return bootstrap.Username, password
}

// TestLoginUniformFailureAndLock covers the three failures sharing one answer,
// the five-attempt lock, its expiry and both ways the counter clears (C-63).
func TestLoginUniformFailureAndLock(t *testing.T) {
	service, clock := newTestService(t, &fakeOwned{})
	ctx := context.Background()
	username, password := bootstrapOwner(t, service)

	for attempt := 1; attempt <= LoginFailureLimit; attempt++ {
		if _, err := service.Login(ctx, username, "wrong-password"); !errors.Is(err, ErrLoginFailed) {
			t.Fatalf("attempt %d for an existing account = %v, want a plain login failure", attempt, err)
		}
		if _, err := service.Login(ctx, "nobody", "wrong-password"); !errors.Is(err, ErrLoginFailed) {
			t.Fatalf("attempt %d for an unknown name = %v, want a plain login failure", attempt, err)
		}
	}
	_, existingErr := service.Login(ctx, username, "wrong-password")
	_, unknownErr := service.Login(ctx, "nobody", "wrong-password")
	var existingLocked, unknownLocked *LockedError
	if !errors.As(existingErr, &existingLocked) {
		t.Fatalf("the sixth attempt for an existing account = %v, want a lock", existingErr)
	}
	if !errors.As(unknownErr, &unknownLocked) {
		t.Fatalf("the sixth attempt for an unknown name = %v, want a lock", unknownErr)
	}
	if existingLocked.Remaining != LoginLockDuration || unknownLocked.Remaining != LoginLockDuration {
		t.Fatalf("lock remaining = %s and %s, want %s", existingLocked.Remaining, unknownLocked.Remaining, LoginLockDuration)
	}

	clock.Advance(LoginLockDuration + time.Second)
	for attempt := 1; attempt <= LoginFailureLimit; attempt++ {
		if _, err := service.Login(ctx, username, "wrong-password"); !errors.Is(err, ErrLoginFailed) {
			t.Fatalf("attempt %d after the window = %v, want a plain failure", attempt, err)
		}
	}
	if _, err := service.Login(ctx, username, "wrong-password"); !errors.As(err, new(*LockedError)) {
		t.Fatalf("attempt after a full second round = %v, want a lock", err)
	}

	// A successful login clears the counter.
	clock.Advance(LoginLockDuration + time.Second)
	for attempt := range LoginFailureLimit - 1 {
		if _, err := service.Login(ctx, username, "wrong-password"); !errors.Is(err, ErrLoginFailed) {
			t.Fatalf("failure %d before the reset = %v", attempt+1, err)
		}
	}
	login, err := service.Login(ctx, username, password)
	if err != nil {
		t.Fatalf("login with the right password = %v, want the reset to allow it", err)
	}
	if err := service.Logout(ctx, login.Token); err != nil {
		t.Fatalf("logout: %v", err)
	}
	for attempt := range LoginFailureLimit - 1 {
		if _, err := service.Login(ctx, username, "wrong-password"); !errors.Is(err, ErrLoginFailed) {
			t.Fatalf("failure %d after the reset = %v", attempt+1, err)
		}
	}
	if _, err := service.Login(ctx, username, password); err != nil {
		t.Fatalf("a success must reset the counter: %v", err)
	}
}

// TestSessionBounds covers the twelve-hour idle and seven-day absolute bounds,
// whichever ends first (C-63).
func TestSessionBounds(t *testing.T) {
	service, clock := newTestService(t, &fakeOwned{})
	ctx := context.Background()
	username, password := bootstrapOwner(t, service)

	login, err := service.Login(ctx, username, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if login.Session.Remaining != SessionIdleTimeout {
		t.Fatalf("fresh remaining = %s, want the idle bound %s", login.Session.Remaining, SessionIdleTimeout)
	}
	if login.Session.AbsoluteRemaining != SessionAbsoluteTimeout {
		t.Fatalf("absolute remaining = %s, want %s", login.Session.AbsoluteRemaining, SessionAbsoluteTimeout)
	}

	clock.Advance(SessionIdleTimeout - time.Minute)
	if _, err := service.Authenticate(ctx, login.Token); err != nil {
		t.Fatalf("a request inside the idle window = %v", err)
	}
	clock.Advance(SessionIdleTimeout + time.Minute)
	if _, err := service.Authenticate(ctx, login.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a request past the idle window = %v, want an expired session", err)
	}

	// Keep the idle bound fresh until the absolute bound wins.
	absolute, err := service.Login(ctx, username, password)
	if err != nil {
		t.Fatalf("login again: %v", err)
	}
	deadline := absolute.Session.AbsoluteExpiresAt
	for clock.Now().Add(SessionIdleTimeout - time.Minute).Before(deadline) {
		clock.Advance(SessionIdleTimeout - time.Minute)
		if _, err := service.Authenticate(ctx, absolute.Token); err != nil {
			t.Fatalf("a request inside both bounds = %v", err)
		}
	}
	clock.Advance(deadline.Sub(clock.Now()) + time.Minute)
	if _, err := service.Authenticate(ctx, absolute.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a request past the absolute bound = %v, want an expired session", err)
	}
}

// TestRevocation covers logout, password change, disable and delete ending the
// sessions they must end (C-63).
func TestRevocation(t *testing.T) {
	owned := &fakeOwned{objects: map[string][]OwnedObject{}}
	service, _ := newTestService(t, owned)
	ctx := context.Background()
	ownerName, ownerPassword := bootstrapOwner(t, service)
	ownerLogin, err := service.Login(ctx, ownerName, ownerPassword)
	if err != nil {
		t.Fatalf("login owner: %v", err)
	}
	created, err := service.CreateAccount(ctx, ownerLogin.Session, NewAccount{Username: "alice", Role: write.RoleMember})
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	aliceLogin, err := service.Login(ctx, "alice", created.OneTimePassword)
	if err != nil {
		t.Fatalf("login alice: %v", err)
	}
	alicePassword := "alice-password"
	if err := service.ChangePassword(ctx, aliceLogin.Token, created.OneTimePassword, alicePassword); err != nil {
		t.Fatalf("change alice password: %v", err)
	}

	first, err := service.Login(ctx, "alice", alicePassword)
	if err != nil {
		t.Fatalf("login alice again: %v", err)
	}
	second, err := service.Login(ctx, "alice", alicePassword)
	if err != nil {
		t.Fatalf("login alice a third time: %v", err)
	}
	if err := service.Logout(ctx, first.Token); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := service.Authenticate(ctx, first.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("the logged-out session = %v, want it rejected", err)
	}
	if _, err := service.Authenticate(ctx, second.Token); err != nil {
		t.Fatalf("the other session after a logout = %v", err)
	}

	third, err := service.Login(ctx, "alice", alicePassword)
	if err != nil {
		t.Fatalf("login for the password change: %v", err)
	}
	replacement := "alice-password-2"
	if err := service.ChangePassword(ctx, third.Token, alicePassword, replacement); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, err := service.Authenticate(ctx, second.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("another session after a password change = %v, want it rejected", err)
	}
	if _, err := service.Authenticate(ctx, third.Token); err != nil {
		t.Fatalf("the session that changed the password = %v, want it usable", err)
	}
	if _, err := service.Login(ctx, "alice", alicePassword); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("login with the replaced password = %v, want a failure", err)
	}

	disabled := true
	if _, err := service.UpdateAccount(ctx, ownerLogin.Session, "alice", AccountChange{Disabled: &disabled}); err != nil {
		t.Fatalf("disable alice: %v", err)
	}
	if _, err := service.Authenticate(ctx, third.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a session after disable = %v, want it rejected", err)
	}
	if _, err := service.Login(ctx, "alice", replacement); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("login after disable = %v, want the shared failure", err)
	}

	owned.setOwned("alice", OwnedObject{Kind: "Application", Namespace: "default", Name: "web"})
	var hasObjects *HasObjectsError
	if err := service.DeleteAccount(ctx, ownerLogin.Session, "alice"); !errors.As(err, &hasObjects) || len(hasObjects.Objects) != 1 {
		t.Fatalf("delete with an owned object = %v, want the object listed", err)
	}
	owned.setOwned("alice")
	if err := service.DeleteAccount(ctx, ownerLogin.Session, "alice"); err != nil {
		t.Fatalf("delete after transfer: %v", err)
	}
	if _, err := service.Login(ctx, "alice", replacement); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("login after delete = %v, want the shared failure", err)
	}
}

// TestRoleChangeAppliesToNextRequest checks that the role is read per request,
// not frozen into the session (C-63).
func TestRoleChangeAppliesToNextRequest(t *testing.T) {
	service, _ := newTestService(t, &fakeOwned{})
	ctx := context.Background()
	ownerName, ownerPassword := bootstrapOwner(t, service)
	ownerLogin, err := service.Login(ctx, ownerName, ownerPassword)
	if err != nil {
		t.Fatalf("login owner: %v", err)
	}
	created, err := service.CreateAccount(ctx, ownerLogin.Session, NewAccount{Username: "alice", Role: write.RoleMember})
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	login, err := service.Login(ctx, "alice", created.OneTimePassword)
	if err != nil {
		t.Fatalf("login alice: %v", err)
	}
	if login.Session.Role != write.RoleMember {
		t.Fatalf("role at login = %q, want Member", login.Session.Role)
	}
	guest := write.RoleGuest
	if _, err := service.UpdateAccount(ctx, ownerLogin.Session, "alice", AccountChange{Role: &guest}); err != nil {
		t.Fatalf("change role: %v", err)
	}
	session, err := service.Authenticate(ctx, login.Token)
	if err != nil {
		t.Fatalf("authenticate after the role change: %v", err)
	}
	if session.Role != write.RoleGuest {
		t.Fatalf("role on the next request = %q, want Guest", session.Role)
	}
}

// TestLastOwnerGuard checks that the only usable Owner cannot be disabled,
// demoted or deleted (C-63).
func TestLastOwnerGuard(t *testing.T) {
	owned := &fakeOwned{objects: map[string][]OwnedObject{}}
	service, _ := newTestService(t, owned)
	ctx := context.Background()
	ownerName, ownerPassword := bootstrapOwner(t, service)
	ownerLogin, err := service.Login(ctx, ownerName, ownerPassword)
	if err != nil {
		t.Fatalf("login owner: %v", err)
	}
	disabled := true
	if _, err := service.UpdateAccount(ctx, ownerLogin.Session, ownerName, AccountChange{Disabled: &disabled}); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("disabling the last Owner = %v, want the guard", err)
	}
	member := write.RoleMember
	if _, err := service.UpdateAccount(ctx, ownerLogin.Session, ownerName, AccountChange{Role: &member}); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demoting the last Owner = %v, want the guard", err)
	}
	if err := service.DeleteAccount(ctx, ownerLogin.Session, ownerName); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("deleting the last Owner = %v, want the guard", err)
	}
	if _, err := service.CreateAccount(ctx, ownerLogin.Session, NewAccount{Username: "deputy", Role: write.RoleOwner}); err != nil {
		t.Fatalf("create a second Owner: %v", err)
	}
	if _, err := service.UpdateAccount(ctx, ownerLogin.Session, ownerName, AccountChange{Role: &member}); err != nil {
		t.Fatalf("demote the first Owner with a second one present: %v", err)
	}
}

// TestFirstLoginPasswordChange checks the one-time password path (C-63).
func TestFirstLoginPasswordChange(t *testing.T) {
	service, _ := newTestService(t, &fakeOwned{})
	ctx := context.Background()
	ownerName, ownerPassword := bootstrapOwner(t, service)
	ownerLogin, err := service.Login(ctx, ownerName, ownerPassword)
	if err != nil {
		t.Fatalf("login owner: %v", err)
	}
	created, err := service.CreateAccount(ctx, ownerLogin.Session, NewAccount{Username: "alice", Role: write.RoleMember})
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if created.OneTimePassword == "" {
		t.Fatal("a generated account did not return its one-time password")
	}
	login, err := service.Login(ctx, "alice", created.OneTimePassword)
	if err != nil {
		t.Fatalf("login with the one-time password: %v", err)
	}
	if !login.Session.MustChangePassword {
		t.Fatal("the first login did not require a change")
	}
	if err := service.ChangePassword(ctx, login.Token, "not-the-password", "alice-password"); !errors.Is(err, ErrCurrentPassword) {
		t.Fatalf("change with a wrong current password = %v", err)
	}
	if err := service.ChangePassword(ctx, login.Token, created.OneTimePassword, "alice-password"); err != nil {
		t.Fatalf("change the one-time password: %v", err)
	}
	if _, err := service.Login(ctx, "alice", created.OneTimePassword); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("login with the replaced one-time password = %v", err)
	}
	if _, err := service.Login(ctx, "alice", "alice-password"); err != nil {
		t.Fatalf("login with the new password: %v", err)
	}
}

// TestOwnershipUnavailable keeps a deletion from stranding objects when the
// ownership source cannot answer (C-60).
func TestOwnershipUnavailable(t *testing.T) {
	service, _ := newTestService(t, nil)
	ctx := context.Background()
	ownerName, ownerPassword := bootstrapOwner(t, service)
	ownerLogin, err := service.Login(ctx, ownerName, ownerPassword)
	if err != nil {
		t.Fatalf("login owner: %v", err)
	}
	created, err := service.CreateAccount(ctx, ownerLogin.Session, NewAccount{Username: "alice", Role: write.RoleMember})
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if created.Account.Username != "alice" {
		t.Fatal("account not created")
	}
	if err := service.DeleteAccount(ctx, ownerLogin.Session, "alice"); !errors.Is(err, ErrOwnershipUnavailable) {
		t.Fatalf("delete without an ownership source = %v, want a refusal", err)
	}
}

// TestDatabaseUnavailableKeepsReads covers the split C-61 asks for: logins and
// writes stop, an already signed-in read survives from memory.
func TestDatabaseUnavailableKeepsReads(t *testing.T) {
	clock := newTestClock()
	path := filepath.Join(t.TempDir(), "kos.db")
	store, err := Open(context.Background(), OpenOptions{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	service := New(store, Config{Now: clock.Now, Owned: &fakeOwned{}})
	ctx := context.Background()
	username, password := bootstrapOwner(t, service)
	login, err := service.Login(ctx, username, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	session, err := service.Authenticate(ctx, login.Token)
	if err != nil {
		t.Fatalf("a signed-in read after the database stopped = %v", err)
	}
	if !session.Degraded {
		t.Fatal("the fallback session is not marked degraded")
	}
	if _, err := service.Login(ctx, username, password); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("login with the database down = %v, want unavailability", err)
	}
	if _, err := service.Authenticate(ctx, "unknown-token"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an unknown token with the database down = %v, want unavailability", err)
	}
}
