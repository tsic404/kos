package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tsic404/kos/pkg/write"
)

// BootstrapWindowLength is how long the first-Owner claim window stays open
// after an installation finishes (C-68). The length is fixed: nothing extends
// it, and a restart does not move the deadline.
const BootstrapWindowLength = 30 * time.Minute

// BootstrapWindow is the claim window one control-plane database recorded.
type BootstrapWindow struct {
	// OpenedAt is when the installation finished.
	OpenedAt time.Time
	// Deadline is the instant after which no claim is served.
	Deadline time.Time
	// ClaimedAt is when the password was delivered; zero while it is unclaimed.
	ClaimedAt time.Time
}

// Open reports whether the window still serves a claim at instant.
func (w BootstrapWindow) Open(at time.Time) bool {
	return w.ClaimedAt.IsZero() && at.Before(w.Deadline)
}

// OpenBootstrapWindow records the claim window an installation opens (C-68). A
// database that already holds one keeps it: a reinstall that preserved the
// control-plane database neither reopens the window nor moves its deadline.
func (s *Store) OpenBootstrapWindow(ctx context.Context, opensAt, deadline time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO bootstrap_window (id, opened_at, deadline) VALUES (1, ?, ?) ON CONFLICT (id) DO NOTHING`,
		toMillis(opensAt), toMillis(deadline))
	if err != nil {
		return fmt.Errorf("identity: open the claim window: %w", err)
	}
	return nil
}

// BootstrapWindow reports the recorded window. ErrNotFound means this database
// never opened one, which is the state of every installation form that hands
// the password out through its own output (C-54).
func (s *Store) BootstrapWindow(ctx context.Context) (BootstrapWindow, error) {
	return scanBootstrapWindow(s.db.QueryRowContext(ctx,
		`SELECT opened_at, deadline, claimed_at FROM bootstrap_window WHERE id = 1`))
}

// ClaimFirstOwner replaces the first Owner's password and closes the claim
// window in one transaction (C-68): a claim that cannot be recorded changes no
// password, and a recorded claim is never delivered twice. Every session of the
// account ends with the old password.
func (s *Store) ClaimFirstOwner(ctx context.Context, at time.Time, passwordHash string) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("identity: begin the first-Owner claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	window, err := scanBootstrapWindow(tx.QueryRowContext(ctx,
		`SELECT opened_at, deadline, claimed_at FROM bootstrap_window WHERE id = 1`))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return User{}, ErrBootstrapClosed
	case err != nil:
		return User{}, err
	}
	if !window.Open(at) {
		return User{}, ErrBootstrapClosed
	}
	owner, err := scanUser(tx.QueryRowContext(ctx,
		userColumns+` WHERE role = ? ORDER BY created_at, username LIMIT 1`, string(write.RoleOwner)))
	if err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, must_change_password = 1, updated_at = ? WHERE id = ?`,
		passwordHash, toMillis(at), owner.ID); err != nil {
		return User{}, fmt.Errorf("identity: set the first Owner's password: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, owner.ID); err != nil {
		return User{}, fmt.Errorf("identity: end the first Owner's sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE bootstrap_window SET claimed_at = ? WHERE id = 1`, toMillis(at)); err != nil {
		return User{}, fmt.Errorf("identity: close the claim window: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("identity: commit the first-Owner claim: %w", err)
	}
	owner.PasswordHash = passwordHash
	owner.MustChangePassword = true
	owner.UpdatedAt = at
	return owner, nil
}

// scanBootstrapWindow reads one stored window row.
func scanBootstrapWindow(row rowScanner) (BootstrapWindow, error) {
	var (
		window    BootstrapWindow
		openedAt  int64
		deadline  int64
		claimedAt sql.NullInt64
	)
	if err := row.Scan(&openedAt, &deadline, &claimedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BootstrapWindow{}, err
		}
		return BootstrapWindow{}, fmt.Errorf("identity: read the claim window: %w", err)
	}
	window.OpenedAt = fromMillis(openedAt)
	window.Deadline = fromMillis(deadline)
	if claimedAt.Valid {
		window.ClaimedAt = fromMillis(claimedAt.Int64)
	}
	return window, nil
}

// PrepareFirstOwner creates the first Owner without handing out a password: the
// external-control-plane form delivers it through the claim window instead of
// the install output (C-54, C-68). An existing first Owner is reused.
func (s *Service) PrepareFirstOwner(ctx context.Context, username string) (Bootstrap, error) {
	return s.bootstrapOwner(ctx, username, false)
}

// OpenBootstrapWindow records the window an installation opens when it
// finishes (C-68). A window already recorded is left as it is, so a reinstall
// that kept the control-plane database does not reopen one.
func (s *Service) OpenBootstrapWindow(ctx context.Context) error {
	now := s.now()
	if err := s.store.OpenBootstrapWindow(ctx, now, now.Add(BootstrapWindowLength)); err != nil {
		return s.storeFailure("open the claim window", err)
	}
	return nil
}

// FirstOwnerName reports the account the claim window delivers to; the name is
// empty when the database holds no first Owner. A refused claim records the
// account it would have changed, so the trail names one account (C-14).
func (s *Service) FirstOwnerName(ctx context.Context) string {
	owner, err := s.store.FirstOwner(ctx)
	if err != nil {
		return ""
	}
	return owner.Username
}

// ClaimFirstOwner delivers the first Owner's one-time password once, inside the
// window (C-68). The password is generated here, never stored in clear, and
// the first login must replace it.
func (s *Service) ClaimFirstOwner(ctx context.Context) (Bootstrap, error) {
	oneTime, err := newOneTimePassword()
	if err != nil {
		s.log.Error("generate one-time password", "error", err)
		return Bootstrap{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	digest, err := hashPassword(oneTime)
	if err != nil {
		s.log.Error("hash password", "error", err)
		return Bootstrap{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	owner, err := s.store.ClaimFirstOwner(ctx, s.now(), digest)
	switch {
	case errors.Is(err, ErrBootstrapClosed), errors.Is(err, ErrNotFound):
		return Bootstrap{}, err
	case err != nil:
		return Bootstrap{}, s.storeFailure("claim the first Owner", err)
	}
	s.cache.removeUser(owner.Username)
	return Bootstrap{Username: owner.Username, OneTimePassword: oneTime}, nil
}
