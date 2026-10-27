package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/tsic404/kos/pkg/write"
)

// User is one stored account. Its password field holds the encoded digest,
// never the password (C-63).
type User struct {
	// ID is the internal identity; sessions and preferences reference it.
	ID string
	// Username is the login name and the name audit carries.
	Username string
	// Role is the account's current role (C-59); it is read per request.
	Role write.Role
	// PasswordHash is the one-way digest.
	PasswordHash string
	// Disabled accounts cannot log in and hold no sessions.
	Disabled bool
	// MustChangePassword marks a one-time password that has not been replaced.
	MustChangePassword bool
	// CreatedAt is when the account was created.
	CreatedAt time.Time
	// UpdatedAt is when role, state or password last changed.
	UpdatedAt time.Time
}

// SessionRecord is a stored session bound to one account.
type SessionRecord struct {
	// TokenHash is the stored form of the bearer token.
	TokenHash string
	// UserID is the account the session belongs to.
	UserID string
	// CreatedAt is when login issued it; the absolute bound counts from here.
	CreatedAt time.Time
	// LastSeenAt is the newest authenticated request.
	LastSeenAt time.Time
	// IdleExpiresAt moves with LastSeenAt.
	IdleExpiresAt time.Time
	// AbsoluteExpiresAt never moves.
	AbsoluteExpiresAt time.Time
}

// StoredSession is a session joined with its account's current state.
type StoredSession struct {
	// TokenHash is the stored token form.
	TokenHash string
	// UserID is the account identity.
	UserID string
	// Username is the account's login name.
	Username string
	// Role is the account's current role, not a copy taken at login (C-63).
	Role write.Role
	// Disabled tells whether the account is disabled.
	Disabled bool
	// MustChangePassword tells whether the one-time password is still in use.
	MustChangePassword bool
	// CreatedAt is when the session was issued.
	CreatedAt time.Time
	// LastSeenAt is the newest authenticated request.
	LastSeenAt time.Time
	// IdleExpiresAt is the current idle deadline.
	IdleExpiresAt time.Time
	// AbsoluteExpiresAt is the fixed deadline.
	AbsoluteExpiresAt time.Time
}

// LoginAttempt is the failure counter of one login name.
type LoginAttempt struct {
	// FailureCount is the number of consecutive failures.
	FailureCount int
	// LockedUntil is when the lock window ends; zero means no lock.
	LockedUntil time.Time
}

// Preference is one stored user preference. Preferences are scoped by the
// extension that declared the key set and by the account (C-27); the kernel
// domain is the empty extension.
type Preference struct {
	// Extension is the extension identifier the preference belongs to.
	Extension string
	// Key names the preference.
	Key string
	// Value is the stored encoding the contributing extension owns.
	Value string
	// UpdatedAt is when it last changed.
	UpdatedAt time.Time
}

// PreferenceDomain is the recorded fate of one extension's preference data and
// the component object that declared it (C-27, C-29).
type PreferenceDomain struct {
	// Identifier is the extension identity the domain belongs to.
	Identifier string
	// Component is the system component object that declared the domain.
	Component string
	// Keep reports whether the declaration kept the data when withdrawn.
	Keep bool
}

// AuditEntry is one stored audit record (C-14).
type AuditEntry struct {
	// Actor is the username the write was made as.
	Actor string
	// Object names what was written.
	Object string
	// Verb is the action.
	Verb string
	// Result is the outcome.
	Result string
	// Source names the surface the write came from.
	Source string
	// Extension is the identifier of the extension declaring the object's
	// class; empty for a kernel write (C-14).
	Extension string
	// OccurredAt is when it happened.
	OccurredAt time.Time
}

// OpenOptions configures the control-plane database.
type OpenOptions struct {
	// Path is the database file; required.
	Path string
	// Now supplies the migration clock; defaults to time.Now.
	Now func() time.Time
}

// Store is the control-plane database: users, sessions, preferences and audit
// (C-61). It is not on the reconciliation path.
type Store struct {
	db   *sql.DB
	path string
	now  func() time.Time

	// guardHook runs between reading an account and writing it inside the
	// owner-guard transaction. Tests use it to hold one transaction open; in
	// production it is nil.
	guardHook func()
}

// Open creates or opens the database and brings its schema to the newest
// version. The file is private to the control plane process.
func Open(ctx context.Context, options OpenOptions) (*Store, error) {
	if options.Path == "" {
		return nil, errors.New("identity: a database path is required")
	}
	path, err := filepath.Abs(options.Path)
	if err != nil {
		return nil, fmt.Errorf("identity: resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("identity: create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", writableDSN(path))
	if err != nil {
		return nil, fmt.Errorf("identity: open database: %w", err)
	}
	// One connection serializes the read-modify-write operations (login
	// counters, session touches) without a second lock.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{
		db:   db,
		path: path,
		now:  options.Now,
	}
	if store.now == nil {
		store.now = time.Now
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("identity: connect database: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("identity: restrict database permissions: %w", err)
	}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// writableDSN builds the SQLite URI. WAL keeps a reader working while the
// exporter snapshots; foreign keys make account deletion drop sessions and
// preferences with it.
func writableDSN(path string) string {
	base := (&url.URL{Scheme: "file", Path: path}).String()
	return base + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
}

// readOnlyDSN opens a file without allowing writes; it is how an export is
// inspected before it replaces a live database.
func readOnlyDSN(path string) string {
	base := (&url.URL{Scheme: "file", Path: path}).String()
	return base + "?mode=ro"
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// Path is the database file this store opened.
func (s *Store) Path() string { return s.path }

// Now is the store's clock.
func (s *Store) Now() time.Time { return s.now() }

// CreateUser inserts an account. The name check and the insert share one
// transaction, so two creates cannot both believe the name was free.
func (s *Store) CreateUser(ctx context.Context, user User) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("identity: begin account create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existing string
	switch err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, user.Username).Scan(&existing); {
	case err == nil:
		return ErrUsernameTaken
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("identity: check login name: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO users
		(id, username, role, password_hash, is_disabled, must_change_password, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		user.ID, user.Username, string(user.Role), user.PasswordHash,
		boolInt(user.Disabled), boolInt(user.MustChangePassword),
		toMillis(user.CreatedAt), toMillis(user.UpdatedAt))
	if err != nil {
		return fmt.Errorf("identity: insert account: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("identity: commit account create: %w", err)
	}
	return nil
}

// UserByName reads one account.
func (s *Store) UserByName(ctx context.Context, username string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, userColumns+` WHERE username = ?`, username))
}

// FirstOwner returns the account that already holds the Owner role; an
// install reuses it instead of creating a second first Owner (C-54).
func (s *Store) FirstOwner(ctx context.Context) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, userColumns+` WHERE role = ? ORDER BY created_at, username LIMIT 1`, string(write.RoleOwner)))
}

// ListUsers returns every account ordered by login name.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, userColumns+` ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("identity: list accounts: %w", err)
	}
	defer rows.Close()
	users := make([]User, 0, 8)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: list accounts: %w", err)
	}
	return users, nil
}

// UpdateUserState changes only the fields the caller names and applies the last
// usable Owner rule in the same write transaction (C-63). A nil field is not
// written, so a concurrent change to the other field survives instead of being
// overwritten by a stale snapshot.
//
// becameDisabled reports an enabled-to-disabled transition, which is what ends
// the account's sessions.
func (s *Store) UpdateUserState(ctx context.Context, username string, role *write.Role, disabled *bool) (User, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil) // immediate: the write lock is held from here
	if err != nil {
		return User{}, false, fmt.Errorf("identity: begin account update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanUser(tx.QueryRowContext(ctx, userColumns+` WHERE username = ?`, username))
	if err != nil {
		return User{}, false, err
	}
	s.hitGuardHook()
	newRole := current.Role
	if role != nil {
		newRole = *role
	}
	newDisabled := current.Disabled
	if disabled != nil {
		newDisabled = *disabled
	}
	if isUsableOwner(current.Role, current.Disabled) && !isUsableOwner(newRole, newDisabled) {
		others, err := otherUsableOwners(ctx, tx, username)
		if err != nil {
			return User{}, false, err
		}
		if others == 0 {
			return User{}, false, ErrLastOwner
		}
	}
	if newRole == current.Role && newDisabled == current.Disabled {
		if err := tx.Commit(); err != nil {
			return User{}, false, fmt.Errorf("identity: commit account update: %w", err)
		}
		return current, false, nil
	}
	now := s.now()
	sets := make([]string, 0, 3)
	arguments := make([]any, 0, 4)
	if newRole != current.Role {
		sets = append(sets, "role = ?")
		arguments = append(arguments, string(newRole))
	}
	if newDisabled != current.Disabled {
		sets = append(sets, "is_disabled = ?")
		arguments = append(arguments, boolInt(newDisabled))
	}
	sets = append(sets, "updated_at = ?")
	arguments = append(arguments, toMillis(now), username)
	result, err := tx.ExecContext(ctx, `UPDATE users SET `+strings.Join(sets, ", ")+` WHERE username = ?`, arguments...)
	if err != nil {
		return User{}, false, fmt.Errorf("identity: update account state: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return User{}, false, fmt.Errorf("identity: update account state: %w", err)
	}
	if affected == 0 {
		return User{}, false, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return User{}, false, fmt.Errorf("identity: commit account update: %w", err)
	}
	wasDisabled := current.Disabled
	current.Role = newRole
	current.Disabled = newDisabled
	current.UpdatedAt = now
	return current, newDisabled && !wasDisabled, nil
}

// otherUsableOwners counts enabled Owners other than the named account.
func otherUsableOwners(ctx context.Context, tx *sql.Tx, username string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = ? AND is_disabled = 0 AND username <> ?`,
		string(write.RoleOwner), username).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("identity: count owners: %w", err)
	}
	return count, nil
}

// isUsableOwner reports whether a role and disabled state form an Owner who can
// still manage accounts (C-63).
func isUsableOwner(role write.Role, disabled bool) bool {
	return role == write.RoleOwner && !disabled
}

// hitGuardHook runs the test synchronization point when one is installed.
func (s *Store) hitGuardHook() {
	if s.guardHook != nil {
		s.guardHook()
	}
}

// UpdateUserPassword replaces the digest and the one-time marker.
func (s *Store) UpdateUserPassword(ctx context.Context, username, passwordHash string, mustChange bool) error {
	return s.updateUser(ctx, `UPDATE users SET password_hash = ?, must_change_password = ?, updated_at = ? WHERE username = ?`,
		passwordHash, boolInt(mustChange), toMillis(s.now()), username)
}

// updateUser runs one account update and reports a missing account.
func (s *Store) updateUser(ctx context.Context, statement string, arguments ...any) error {
	result, err := s.db.ExecContext(ctx, statement, arguments...)
	if err != nil {
		return fmt.Errorf("identity: update account: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("identity: update account: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes an account; sessions and preferences fall with it. The
// last usable Owner rule is re-checked inside the same write transaction, so a
// concurrent change cannot slip between the check and the delete (C-63).
func (s *Store) DeleteUser(ctx context.Context, username string) error {
	tx, err := s.db.BeginTx(ctx, nil) // immediate: the write lock is held from here
	if err != nil {
		return fmt.Errorf("identity: begin account delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanUser(tx.QueryRowContext(ctx, userColumns+` WHERE username = ?`, username))
	if err != nil {
		return err
	}
	s.hitGuardHook()
	if isUsableOwner(current.Role, current.Disabled) {
		others, err := otherUsableOwners(ctx, tx, username)
		if err != nil {
			return err
		}
		if others == 0 {
			return ErrLastOwner
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE username = ?`, username); err != nil {
		return fmt.Errorf("identity: delete account: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("identity: commit account delete: %w", err)
	}
	return nil
}

// CreateSession stores a session. Only the token hash is written (C-63).
func (s *Store) CreateSession(ctx context.Context, record SessionRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions
		(token_hash, user_id, created_at, last_seen_at, idle_expires_at, absolute_expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		record.TokenHash, record.UserID, toMillis(record.CreatedAt),
		toMillis(record.LastSeenAt), toMillis(record.IdleExpiresAt), toMillis(record.AbsoluteExpiresAt))
	if err != nil {
		return fmt.Errorf("identity: insert session: %w", err)
	}
	return nil
}

// Session reads one session with its account's current role and state.
func (s *Store) Session(ctx context.Context, tokenHash string) (StoredSession, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		s.token_hash, s.user_id, u.username, u.role, u.is_disabled, u.must_change_password,
		s.created_at, s.last_seen_at, s.idle_expires_at, s.absolute_expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ?`, tokenHash)
	var (
		stored      StoredSession
		role        string
		disabled    int
		mustChange  int
		createdAt   int64
		lastSeenAt  int64
		idleExpires int64
		absolute    int64
	)
	err := row.Scan(&stored.TokenHash, &stored.UserID, &stored.Username, &role, &disabled, &mustChange,
		&createdAt, &lastSeenAt, &idleExpires, &absolute)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredSession{}, ErrNotFound
	}
	if err != nil {
		return StoredSession{}, fmt.Errorf("identity: read session: %w", err)
	}
	stored.Role = write.Role(role)
	stored.Disabled = disabled != 0
	stored.MustChangePassword = mustChange != 0
	stored.CreatedAt = fromMillis(createdAt)
	stored.LastSeenAt = fromMillis(lastSeenAt)
	stored.IdleExpiresAt = fromMillis(idleExpires)
	stored.AbsoluteExpiresAt = fromMillis(absolute)
	return stored, nil
}

// TouchSession moves the idle deadline. The absolute deadline is untouched.
func (s *Store) TouchSession(ctx context.Context, tokenHash string, lastSeen, idleExpires time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, idle_expires_at = ? WHERE token_hash = ?`,
		toMillis(lastSeen), toMillis(idleExpires), tokenHash)
	if err != nil {
		return fmt.Errorf("identity: update session: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("identity: update session: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSession revokes one session.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("identity: delete session: %w", err)
	}
	return nil
}

// DeleteOtherSessions revokes every session of an account except one; a
// password change keeps the session that made it usable (C-63).
func (s *Store) DeleteOtherSessions(ctx context.Context, userID, keepTokenHash string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, keepTokenHash)
	if err != nil {
		return 0, fmt.Errorf("identity: delete other sessions: %w", err)
	}
	return result.RowsAffected()
}

// DeleteUserSessions revokes every session of an account.
func (s *Store) DeleteUserSessions(ctx context.Context, userID string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, fmt.Errorf("identity: delete account sessions: %w", err)
	}
	return result.RowsAffected()
}

// LoginAttempt reads the counter of one login name; an absent row is zero.
func (s *Store) LoginAttempt(ctx context.Context, loginName string) (LoginAttempt, error) {
	var (
		attempt    LoginAttempt
		lockedTill sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `SELECT failure_count, locked_until FROM login_attempts WHERE login_name = ?`, loginName).
		Scan(&attempt.FailureCount, &lockedTill)
	if errors.Is(err, sql.ErrNoRows) {
		return LoginAttempt{}, nil
	}
	if err != nil {
		return LoginAttempt{}, fmt.Errorf("identity: read login attempt: %w", err)
	}
	if lockedTill.Valid {
		attempt.LockedUntil = fromMillis(lockedTill.Int64)
	}
	return attempt, nil
}

// RecordLoginFailure counts one failure and opens the lock window when the
// limit is reached. An expired lock starts the count over.
func (s *Store) RecordLoginFailure(ctx context.Context, loginName string, now time.Time, limit int, lock time.Duration) (LoginAttempt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LoginAttempt{}, fmt.Errorf("identity: begin login failure: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The login path is unauthenticated, so the counter table must stay bounded:
	// drop unlocked counters older than the retention window and locks whose
	// window already closed in this same transaction. C-63 counts consecutive
	// failures; failures this far apart are no longer consecutive.
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_attempts
		WHERE (locked_until IS NULL AND updated_at < ?)
		   OR (locked_until IS NOT NULL AND locked_until < ?)`,
		toMillis(now.Add(-LoginAttemptRetention)), toMillis(now)); err != nil {
		return LoginAttempt{}, fmt.Errorf("identity: prune login attempts: %w", err)
	}
	attempt := LoginAttempt{}
	var lockedTill sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT failure_count, locked_until FROM login_attempts WHERE login_name = ?`, loginName).
		Scan(&attempt.FailureCount, &lockedTill)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return LoginAttempt{}, fmt.Errorf("identity: read login failure: %w", err)
	}
	if lockedTill.Valid {
		attempt.LockedUntil = fromMillis(lockedTill.Int64)
		if !now.Before(attempt.LockedUntil) {
			attempt = LoginAttempt{}
		}
	}
	attempt.FailureCount++
	var lockUntil any
	if attempt.FailureCount >= limit {
		attempt.LockedUntil = now.Add(lock)
		lockUntil = toMillis(attempt.LockedUntil)
	} else {
		attempt.LockedUntil = time.Time{}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO login_attempts
		(login_name, failure_count, locked_until, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(login_name) DO UPDATE SET
			failure_count = excluded.failure_count,
			locked_until = excluded.locked_until,
			updated_at = excluded.updated_at`,
		loginName, attempt.FailureCount, lockUntil, toMillis(now)); err != nil {
		return LoginAttempt{}, fmt.Errorf("identity: record login failure: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return LoginAttempt{}, fmt.Errorf("identity: commit login failure: %w", err)
	}
	return attempt, nil
}

// ResetLoginAttempts clears the counter after a success or an expired lock.
func (s *Store) ResetLoginAttempts(ctx context.Context, loginName string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM login_attempts WHERE login_name = ?`, loginName); err != nil {
		return fmt.Errorf("identity: reset login attempts: %w", err)
	}
	return nil
}

// SetPreference stores one preference of an account in the kernel domain.
func (s *Store) SetPreference(ctx context.Context, username, key, value string) error {
	return s.SetScopedPreference(ctx, username, "", key, value)
}

// SetScopedPreference stores one preference of an account inside one
// extension's domain (C-27).
func (s *Store) SetScopedPreference(ctx context.Context, username, identifier, key, value string) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO preferences (user_id, extension, "key", value, updated_at)
		SELECT id, ?, ?, ?, ? FROM users WHERE username = ? AND true
		ON CONFLICT(user_id, extension, "key") DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		identifier, key, value, toMillis(s.now()), username)
	if err != nil {
		return fmt.Errorf("identity: write preference: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("identity: write preference: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Preferences reads an account's preferences in every domain, ordered by
// extension and key.
func (s *Store) Preferences(ctx context.Context, username string) ([]Preference, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.extension, p."key", p.value, p.updated_at
		FROM preferences p JOIN users u ON u.id = p.user_id
		WHERE u.username = ? ORDER BY p.extension, p."key"`, username)
	if err != nil {
		return nil, fmt.Errorf("identity: read preferences: %w", err)
	}
	defer rows.Close()
	preferences := make([]Preference, 0, 8)
	for rows.Next() {
		var (
			preference Preference
			updatedAt  int64
		)
		if err := rows.Scan(&preference.Extension, &preference.Key, &preference.Value, &updatedAt); err != nil {
			return nil, fmt.Errorf("identity: read preference: %w", err)
		}
		preference.UpdatedAt = fromMillis(updatedAt)
		preferences = append(preferences, preference)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: read preferences: %w", err)
	}
	return preferences, nil
}

// PreferencesOf reads one account's preferences inside one extension's domain.
func (s *Store) PreferencesOf(ctx context.Context, username, identifier string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p."key", p.value
		FROM preferences p JOIN users u ON u.id = p.user_id
		WHERE u.username = ? AND p.extension = ? ORDER BY p."key"`, username, identifier)
	if err != nil {
		return nil, fmt.Errorf("identity: read preferences: %w", err)
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("identity: read preference: %w", err)
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: read preferences: %w", err)
	}
	return values, nil
}

// SetPreferenceDomain records the declared fate of one extension's preference
// data and the component object it belongs to (C-27, C-29).
func (s *Store) SetPreferenceDomain(ctx context.Context, component, identifier string, keep bool) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO preference_domains (identifier, component, keep, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(identifier) DO UPDATE SET component = excluded.component, keep = excluded.keep, updated_at = excluded.updated_at`,
		identifier, component, boolInt(keep), toMillis(s.now())); err != nil {
		return fmt.Errorf("identity: record preference domain: %w", err)
	}
	return nil
}

// PreferenceDomains reads the recorded fate of every extension that declared
// preferences.
func (s *Store) PreferenceDomains(ctx context.Context) ([]PreferenceDomain, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT identifier, component, keep FROM preference_domains`)
	if err != nil {
		return nil, fmt.Errorf("identity: read preference domains: %w", err)
	}
	defer rows.Close()
	domains := make([]PreferenceDomain, 0, 8)
	for rows.Next() {
		var (
			domain PreferenceDomain
			keep   int
		)
		if err := rows.Scan(&domain.Identifier, &domain.Component, &keep); err != nil {
			return nil, fmt.Errorf("identity: read preference domain: %w", err)
		}
		domain.Keep = keep != 0
		domains = append(domains, domain)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: read preference domains: %w", err)
	}
	return domains, nil
}

// DeletePreferenceDomain removes every stored preference of one extension and
// forgets its recorded fate (C-27, C-29).
func (s *Store) DeletePreferenceDomain(ctx context.Context, identifier string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM preferences WHERE extension = ?`, identifier); err != nil {
		return fmt.Errorf("identity: delete preferences: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM preference_domains WHERE identifier = ?`, identifier); err != nil {
		return fmt.Errorf("identity: forget preference domain: %w", err)
	}
	return nil
}

// SettingValues reads the stored value of every setting item in one scope
// target (C-23).
func (s *Store) SettingValues(ctx context.Context, scope, target string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT setting_id, value FROM setting_values
		WHERE scope = ? AND target = ? ORDER BY setting_id`, scope, target)
	if err != nil {
		return nil, fmt.Errorf("identity: read setting values: %w", err)
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var id, value string
		if err := rows.Scan(&id, &value); err != nil {
			return nil, fmt.Errorf("identity: read setting value: %w", err)
		}
		values[id] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: read setting values: %w", err)
	}
	return values, nil
}

// SetSettingValue stores the current value of one setting item (C-23).
func (s *Store) SetSettingValue(ctx context.Context, scope, target, identifier, value string) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO setting_values (scope, target, setting_id, value, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(scope, target, setting_id) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		scope, target, identifier, value, toMillis(s.now())); err != nil {
		return fmt.Errorf("identity: write setting value: %w", err)
	}
	return nil
}

// AppendAudit stores one audit record (C-14). The table rejects updates and
// deletes, so this is the only write it accepts.
func (s *Store) AppendAudit(ctx context.Context, entry AuditEntry) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit (actor, object, verb, result, source, extension, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entry.Actor, entry.Object, entry.Verb, entry.Result, entry.Source, entry.Extension, toMillis(entry.OccurredAt))
	if err != nil {
		return fmt.Errorf("identity: append audit: %w", err)
	}
	return nil
}

// AuditEntries reads audit records oldest first, at most limit of them.
func (s *Store) AuditEntries(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT actor, object, verb, result, source, extension, occurred_at
		FROM audit ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("identity: read audit: %w", err)
	}
	defer rows.Close()
	entries := make([]AuditEntry, 0, limit)
	for rows.Next() {
		var (
			entry      AuditEntry
			occurredAt int64
		)
		if err := rows.Scan(&entry.Actor, &entry.Object, &entry.Verb, &entry.Result, &entry.Source, &entry.Extension, &occurredAt); err != nil {
			return nil, fmt.Errorf("identity: read audit: %w", err)
		}
		entry.OccurredAt = fromMillis(occurredAt)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: read audit: %w", err)
	}
	return entries, nil
}

// ExportTo writes a consistent snapshot of the database to path, which must
// not exist. VACUUM INTO reads the live database under one lock, so the file
// matches a single moment even while requests run (C-61).
func (s *Store) ExportTo(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("identity: export database: %w", err)
	}
	return nil
}

// userColumns is the column order scanUser expects.
const userColumns = `SELECT id, username, role, password_hash, is_disabled, must_change_password, created_at, updated_at FROM users`

// rowScanner is what both sql.Row and sql.Rows offer one result.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanUser reads one account row.
func scanUser(row rowScanner) (User, error) {
	var (
		user       User
		role       string
		disabled   int
		mustChange int
		createdAt  int64
		updatedAt  int64
	)
	err := row.Scan(&user.ID, &user.Username, &role, &user.PasswordHash, &disabled, &mustChange, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("identity: read account: %w", err)
	}
	user.Role = write.Role(role)
	user.Disabled = disabled != 0
	user.MustChangePassword = mustChange != 0
	user.CreatedAt = fromMillis(createdAt)
	user.UpdatedAt = fromMillis(updatedAt)
	return user, nil
}

// boolInt stores a boolean as SQLite's integer.
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
