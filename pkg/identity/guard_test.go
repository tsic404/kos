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

// newTestServiceWithStore opens a private database and returns the store as
// well, so a case can drive the guard hook and inspect rows directly.
func newTestServiceWithStore(t *testing.T, owned OwnedObjects) (*Service, *testClock, *Store) {
	t.Helper()
	clock := newTestClock()
	store, err := Open(context.Background(), OpenOptions{
		Path: filepath.Join(t.TempDir(), "kos.db"),
		Now:  clock.Now,
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(store, Config{Now: clock.Now, Owned: owned}), clock, store
}

// holdFirstTransaction installs a hook that holds the first owner-guard
// transaction open. The returned channels signal entry and accept the release.
func holdFirstTransaction(store *Store) (entered <-chan struct{}, release chan<- struct{}) {
	enteredChannel := make(chan struct{})
	releaseChannel := make(chan struct{})
	var once sync.Once
	store.guardHook = func() {
		once.Do(func() {
			close(enteredChannel)
			<-releaseChannel
		})
	}
	return enteredChannel, releaseChannel
}

// usableOwners counts accounts that can still manage the system.
func usableOwners(t *testing.T, store *Store) int {
	t.Helper()
	users, err := store.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	count := 0
	for _, user := range users {
		if isUsableOwner(user.Role, user.Disabled) {
			count++
		}
	}
	return count
}

// TestLastOwnerDemotionsSerialize checks that two concurrent demotions of the
// last two Owners cannot both pass the guard. The first transaction holds the
// write lock; the second cannot even reach the guard until it commits, so one
// demotion is refused and a usable Owner remains (C-63).
func TestLastOwnerDemotionsSerialize(t *testing.T) {
	service, _, store := newTestServiceWithStore(t, &fakeOwned{objects: map[string][]OwnedObject{}})
	ctx := context.Background()
	first, firstPassword := bootstrapOwner(t, service)
	actor, err := service.Login(ctx, first, firstPassword)
	if err != nil {
		t.Fatalf("login first owner: %v", err)
	}
	if _, err := service.CreateAccount(ctx, actor.Session, NewAccount{Username: "second", Role: write.RoleOwner}); err != nil {
		t.Fatalf("create second owner: %v", err)
	}

	entered, release := holdFirstTransaction(store)
	member := write.RoleMember
	type outcome struct {
		name string
		err  error
	}
	results := make(chan outcome, 2)
	go func() {
		_, err := service.UpdateAccount(ctx, actor.Session, first, AccountChange{Role: &member})
		results <- outcome{name: first, err: err}
	}()
	<-entered
	go func() {
		_, err := service.UpdateAccount(ctx, actor.Session, "second", AccountChange{Role: &member})
		results <- outcome{name: "second", err: err}
	}()
	select {
	case got := <-results:
		t.Fatalf("%s's demotion finished while another held the owner-guard transaction: %v", got.name, got.err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	firstOutcome := <-results
	secondOutcome := <-results
	var refused, accepted int
	for _, got := range []outcome{firstOutcome, secondOutcome} {
		switch {
		case got.err == nil:
			accepted++
		case errors.Is(got.err, ErrLastOwner):
			refused++
		default:
			t.Fatalf("%s's demotion = %v, want success or a last-owner refusal", got.name, got.err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("demotions accepted = %d, refused = %d; want one of each", accepted, refused)
	}
	if remaining := usableOwners(t, store); remaining != 1 {
		t.Fatalf("usable Owners after both demotions = %d, want 1", remaining)
	}
}

// TestLastOwnerDeleteAndDisableSerialize checks the delete path: a delete and
// a disable of the two remaining Owners serialize, the delete commits and the
// disable is refused, so a usable Owner remains (C-63).
func TestLastOwnerDeleteAndDisableSerialize(t *testing.T) {
	service, _, store := newTestServiceWithStore(t, &fakeOwned{objects: map[string][]OwnedObject{}})
	ctx := context.Background()
	first, firstPassword := bootstrapOwner(t, service)
	actor, err := service.Login(ctx, first, firstPassword)
	if err != nil {
		t.Fatalf("login first owner: %v", err)
	}
	if _, err := service.CreateAccount(ctx, actor.Session, NewAccount{Username: "second", Role: write.RoleOwner}); err != nil {
		t.Fatalf("create second owner: %v", err)
	}

	entered, release := holdFirstTransaction(store)
	deleteResult := make(chan error, 1)
	disableResult := make(chan error, 1)
	go func() { deleteResult <- service.DeleteAccount(ctx, actor.Session, first) }()
	<-entered
	disabled := true
	go func() {
		_, err := service.UpdateAccount(ctx, actor.Session, "second", AccountChange{Disabled: &disabled})
		disableResult <- err
	}()
	select {
	case err := <-disableResult:
		t.Fatalf("the disable finished while the delete held the owner-guard transaction: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-deleteResult; err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := <-disableResult; !errors.Is(err, ErrLastOwner) {
		t.Fatalf("disable after the delete = %v, want the last-owner refusal", err)
	}
	if remaining := usableOwners(t, store); remaining != 1 {
		t.Fatalf("usable Owners after both changes = %d, want 1", remaining)
	}
	if _, err := store.UserByName(ctx, first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the deleted account = %v, want not found", err)
	}
}
