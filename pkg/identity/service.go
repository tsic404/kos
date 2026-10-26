package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tsic404/kos/pkg/write"
)

// Session and lock bounds fixed by C-63.
const (
	// SessionIdleTimeout ends a session after twelve hours without a request.
	SessionIdleTimeout = 12 * time.Hour
	// SessionAbsoluteTimeout ends a session seven days after login.
	SessionAbsoluteTimeout = 7 * 24 * time.Hour
	// LoginFailureLimit is the number of consecutive failures that locks a
	// login name.
	LoginFailureLimit = 5
	// LoginLockDuration is how long the name stays locked.
	LoginLockDuration = 5 * time.Minute
	// LoginAttemptRetention is how long an unlocked failure counter may live.
	// C-63 counts consecutive failures; failures further apart than this are not
	// treated as consecutive, and the retention is what keeps the unauthenticated
	// login path from growing the table without bound.
	LoginAttemptRetention = 15 * time.Minute
)

// Input bounds. The contract fixes none of them; they keep a malformed input
// from reaching the database or the hash function.
const (
	// MinPasswordLength is the shortest password a user may set.
	MinPasswordLength = 8
	// MaxPasswordLength is the longest password a user may set.
	MaxPasswordLength = 1024
	// MaxUsernameLength is the longest login name.
	MaxUsernameLength = 253
)

// Session is an authenticated identity for one request (C-63). The role is the
// account's current role, read from the database for every request.
type Session struct {
	// Username is the account's login name.
	Username string
	// Role is the account's current role.
	Role write.Role
	// MustChangePassword marks a one-time password still in use.
	MustChangePassword bool
	// Degraded tells that the database stopped answering and this session was
	// read from memory; reads may proceed, writes may not (C-61).
	Degraded bool
	// ExpiresAt is the earlier of the idle and absolute deadlines.
	ExpiresAt time.Time
	// IdleExpiresAt is the deadline that moves with each request.
	IdleExpiresAt time.Time
	// AbsoluteExpiresAt is the deadline fixed at login.
	AbsoluteExpiresAt time.Time
	// Remaining is how long the session stays usable if untouched.
	Remaining time.Duration
	// IdleRemaining is the idle bound left.
	IdleRemaining time.Duration
	// AbsoluteRemaining is the absolute bound left.
	AbsoluteRemaining time.Duration

	// tokenHash locates the stored session.
	tokenHash string
}

// IsOwner reports whether the session holds the global Owner role.
func (s Session) IsOwner() bool { return s.Role == write.RoleOwner }

// Account is one account as the API and the audit trail see it.
type Account struct {
	// Username is the login name.
	Username string `json:"username"`
	// Role is the current role.
	Role write.Role `json:"role"`
	// Disabled tells whether login is refused.
	Disabled bool `json:"disabled"`
	// MustChangePassword tells whether a one-time password is still in use.
	MustChangePassword bool `json:"mustChangePassword"`
	// CreatedAt is when the account was created.
	CreatedAt time.Time `json:"createdAt"`
}

// Login is the result of a successful sign-in.
type Login struct {
	// Token is the bearer token; it is returned once and never stored in clear.
	Token string
	// Session is the identity the token now carries.
	Session Session
}

// NewAccount is the input that creates an account.
type NewAccount struct {
	// Username is the login name.
	Username string
	// Role is the account's role.
	Role write.Role
	// OneTimePassword is an optional password the installer supplies. When it
	// is empty the service generates one and returns it once.
	OneTimePassword string
}

// CreatedAccount is the result of an account create. OneTimePassword is set
// only when the service generated the password.
type CreatedAccount struct {
	// Account is the stored account.
	Account Account
	// OneTimePassword is the generated first-login password.
	OneTimePassword string
}

// AccountChange rewrites one account. A nil field is left as it is.
type AccountChange struct {
	// Role is the new role.
	Role *write.Role `json:"role,omitempty"`
	// Disabled is the new disabled state.
	Disabled *bool `json:"disabled,omitempty"`
}

// Bootstrap is the result of preparing the first Owner during install.
type Bootstrap struct {
	// Created tells whether this call created the account.
	Created bool
	// Username is the first Owner's login name.
	Username string
	// OneTimePassword is the generated first-login password; it is set only on
	// create.
	OneTimePassword string
}

// Config wires the account service.
type Config struct {
	// Now supplies the clock; defaults to time.Now.
	Now func() time.Time
	// Log records the detail a user-facing message must not carry; defaults to
	// discarding it.
	Log *slog.Logger
	// Owned finds the objects an account still owns; deletion is refused
	// without it (C-60).
	Owned OwnedObjects
}

// Service is the account and session layer of the control plane (C-63).
type Service struct {
	store *Store
	now   func() time.Time
	log   *slog.Logger
	owned OwnedObjects
	cache *sessionCache
}

// New builds the service over a store.
func New(store *Store, config Config) *Service {
	service := &Service{
		store: store,
		now:   config.Now,
		log:   config.Log,
		owned: config.Owned,
		cache: newSessionCache(),
	}
	if service.now == nil {
		service.now = time.Now
	}
	if service.log == nil {
		service.log = slog.New(slog.DiscardHandler)
	}
	return service
}

// Login checks a login name and password. A missing account, a wrong password
// and a disabled account produce the same result (C-63); the counter is keyed
// by the typed name, so an unknown name locks the same way.
func (s *Service) Login(ctx context.Context, loginName, password string) (Login, error) {
	now := s.now()
	// A name longer than an account name can never match; it also must not reach
	// the counter table as a multi-megabyte key (the login path is unauthenticated).
	if loginName == "" || password == "" || utf8.RuneCountInString(loginName) > MaxUsernameLength {
		return Login{}, ErrLoginFailed
	}
	attempt, err := s.store.LoginAttempt(ctx, loginName)
	if err != nil {
		return Login{}, s.storeFailure("read login attempts", err)
	}
	if !attempt.LockedUntil.IsZero() {
		if now.Before(attempt.LockedUntil) {
			return Login{}, &LockedError{Remaining: attempt.LockedUntil.Sub(now)}
		}
		// The window ended: the name gets its five attempts back (C-63).
		if err := s.store.ResetLoginAttempts(ctx, loginName); err != nil {
			return Login{}, s.storeFailure("reset login attempts", err)
		}
	}
	user, userErr := s.store.UserByName(ctx, loginName)
	if userErr != nil && !errors.Is(userErr, ErrNotFound) {
		return Login{}, s.storeFailure("read account", userErr)
	}
	// An unknown or disabled account still pays for a hash, so the three
	// failures do not diverge on how long they take (C-63).
	matched := false
	if userErr == nil {
		matched = verifyPassword(user.PasswordHash, password) && !user.Disabled
	} else {
		verifyPassword(emptyAccountDigest(), password)
	}
	if matched {
		return s.startSession(ctx, user, now)
	}
	if _, err := s.store.RecordLoginFailure(ctx, loginName, now, LoginFailureLimit, LoginLockDuration); err != nil {
		return Login{}, s.storeFailure("record login failure", err)
	}
	return Login{}, ErrLoginFailed
}

// startSession clears the counter and issues a session.
func (s *Service) startSession(ctx context.Context, user User, now time.Time) (Login, error) {
	if err := s.store.ResetLoginAttempts(ctx, user.Username); err != nil {
		return Login{}, s.storeFailure("reset login attempts", err)
	}
	token, err := newSessionToken()
	if err != nil {
		s.log.Error("issue session token", "error", err)
		return Login{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	absolute := now.Add(SessionAbsoluteTimeout)
	idle := now.Add(SessionIdleTimeout)
	if idle.After(absolute) {
		idle = absolute
	}
	record := SessionRecord{
		TokenHash:         sessionTokenHash(token),
		UserID:            user.ID,
		CreatedAt:         now,
		LastSeenAt:        now,
		IdleExpiresAt:     idle,
		AbsoluteExpiresAt: absolute,
	}
	if err := s.store.CreateSession(ctx, record); err != nil {
		return Login{}, s.storeFailure("create session", err)
	}
	session := sessionFrom(StoredSession{
		TokenHash:          record.TokenHash,
		UserID:             user.ID,
		Username:           user.Username,
		Role:               user.Role,
		Disabled:           user.Disabled,
		MustChangePassword: user.MustChangePassword,
		CreatedAt:          record.CreatedAt,
		LastSeenAt:         record.LastSeenAt,
		IdleExpiresAt:      record.IdleExpiresAt,
		AbsoluteExpiresAt:  record.AbsoluteExpiresAt,
	}, now)
	s.cache.put(record.TokenHash, session)
	return Login{Token: token, Session: session}, nil
}

// Authenticate resolves a bearer token. The account's role and state are read
// from the database on every request, so a role change applies to the next
// request instead of the next login (C-63).
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrUnauthenticated
	}
	hash := sessionTokenHash(token)
	now := s.now()
	stored, err := s.store.Session(ctx, hash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.cache.remove(hash)
			return Session{}, ErrUnauthenticated
		}
		// The database stopped answering. A session issued before that keeps
		// serving reads from memory; the degraded flag refuses writes (C-61).
		if cached, ok := s.cache.get(hash, now); ok {
			s.log.Warn("session read degraded to memory", "error", err)
			cached.Degraded = true
			return cached, nil
		}
		return Session{}, s.storeFailure("read session", err)
	}
	if stored.Disabled || !now.Before(stored.IdleExpiresAt) || !now.Before(stored.AbsoluteExpiresAt) {
		if err := s.store.DeleteSession(ctx, hash); err != nil {
			s.log.Warn("delete unusable session", "error", err)
		}
		s.cache.remove(hash)
		return Session{}, ErrUnauthenticated
	}
	idle := now.Add(SessionIdleTimeout)
	if idle.After(stored.AbsoluteExpiresAt) {
		idle = stored.AbsoluteExpiresAt
	}
	session := sessionFrom(stored, now)
	if err := s.store.TouchSession(ctx, hash, now, idle); err != nil {
		s.log.Warn("session touch failed", "error", err)
		session.Degraded = true
	} else {
		stored.LastSeenAt = now
		stored.IdleExpiresAt = idle
		session = sessionFrom(stored, now)
	}
	s.cache.put(hash, session)
	return session, nil
}

// Logout revokes one session.
func (s *Service) Logout(ctx context.Context, token string) error {
	hash := sessionTokenHash(token)
	if err := s.store.DeleteSession(ctx, hash); err != nil {
		return s.storeFailure("delete session", err)
	}
	s.cache.remove(hash)
	return nil
}

// ChangePassword replaces the account's password, revokes every other session
// of that account and keeps the current one usable (C-63).
func (s *Service) ChangePassword(ctx context.Context, token, current, next string) error {
	session, err := s.Authenticate(ctx, token)
	if err != nil {
		return err
	}
	if session.Degraded {
		return ErrUnavailable
	}
	if err := validatePassword(next); err != nil {
		return err
	}
	user, err := s.store.UserByName(ctx, session.Username)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrUnauthenticated
		}
		return s.storeFailure("read account", err)
	}
	if !verifyPassword(user.PasswordHash, current) {
		return ErrCurrentPassword
	}
	digest, err := hashPassword(next)
	if err != nil {
		s.log.Error("hash password", "error", err)
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := s.store.UpdateUserPassword(ctx, user.Username, digest, false); err != nil {
		return s.storeFailure("update password", err)
	}
	if _, err := s.store.DeleteOtherSessions(ctx, user.ID, session.tokenHash); err != nil {
		return s.storeFailure("revoke other sessions", err)
	}
	s.cache.removeUser(user.Username)
	session.MustChangePassword = false
	s.cache.put(session.tokenHash, session)
	return nil
}

// CreateAccount adds an account with a one-time password the account must
// replace at first login (C-63). Only an Owner may call it.
func (s *Service) CreateAccount(ctx context.Context, actor Session, input NewAccount) (CreatedAccount, error) {
	if !actor.IsOwner() {
		return CreatedAccount{}, ErrPermissionDenied
	}
	if err := validateUsername(input.Username); err != nil {
		return CreatedAccount{}, err
	}
	if !validRole(input.Role) {
		return CreatedAccount{}, invalid("角色必须是 Owner、Member 或 Guest")
	}
	oneTime := input.OneTimePassword
	generated := false
	if oneTime == "" {
		var err error
		oneTime, err = newOneTimePassword()
		if err != nil {
			s.log.Error("generate one-time password", "error", err)
			return CreatedAccount{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		generated = true
	} else if err := validatePassword(oneTime); err != nil {
		return CreatedAccount{}, err
	}
	digest, err := hashPassword(oneTime)
	if err != nil {
		s.log.Error("hash password", "error", err)
		return CreatedAccount{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	id, err := newUserID()
	if err != nil {
		s.log.Error("generate account id", "error", err)
		return CreatedAccount{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	now := s.now()
	user := User{
		ID:                 id,
		Username:           input.Username,
		Role:               input.Role,
		PasswordHash:       digest,
		MustChangePassword: true,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := s.store.CreateUser(ctx, user); err != nil {
		return CreatedAccount{}, err
	}
	created := CreatedAccount{Account: accountFromUser(user)}
	if generated {
		created.OneTimePassword = oneTime
	}
	return created, nil
}

// ListAccounts returns every account. Only an Owner may call it.
func (s *Service) ListAccounts(ctx context.Context, actor Session) ([]Account, error) {
	if !actor.IsOwner() {
		return nil, ErrPermissionDenied
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, s.storeFailure("list accounts", err)
	}
	accounts := make([]Account, 0, len(users))
	for _, user := range users {
		accounts = append(accounts, accountFromUser(user))
	}
	return accounts, nil
}

// UpdateAccount changes an account's role or disabled state. The store writes
// only the named fields and enforces the last usable Owner rule inside the same
// transaction, so concurrent changes cannot both pass the guard or overwrite
// each other (C-63). Disabling revokes the account's sessions at once.
func (s *Service) UpdateAccount(ctx context.Context, actor Session, username string, change AccountChange) (Account, error) {
	if !actor.IsOwner() {
		return Account{}, ErrPermissionDenied
	}
	if change.Role != nil && !validRole(*change.Role) {
		return Account{}, invalid("角色必须是 Owner、Member 或 Guest")
	}
	user, becameDisabled, err := s.store.UpdateUserState(ctx, username, change.Role, change.Disabled)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			return Account{}, ErrNotFound
		case errors.Is(err, ErrLastOwner):
			return Account{}, ErrLastOwner
		default:
			return Account{}, s.storeFailure("update account state", err)
		}
	}
	if becameDisabled {
		if _, err := s.store.DeleteUserSessions(ctx, user.ID); err != nil {
			return Account{}, s.storeFailure("revoke account sessions", err)
		}
		s.cache.removeUser(username)
	}
	return accountFromUser(user), nil
}

// DeleteAccount removes an account after its owned objects were transferred.
// The ownership list runs outside the transaction (it reads the cluster); the
// store then re-checks the last usable Owner rule and deletes in one write
// transaction, so a concurrent change cannot slip into the gap (C-60, C-63).
func (s *Service) DeleteAccount(ctx context.Context, actor Session, username string) error {
	if !actor.IsOwner() {
		return ErrPermissionDenied
	}
	if _, err := s.store.UserByName(ctx, username); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return s.storeFailure("read account", err)
	}
	if s.owned == nil {
		return ErrOwnershipUnavailable
	}
	objects, err := s.owned.ListOwned(ctx, username)
	if err != nil {
		s.log.Error("list owned objects", "username", username, "error", err)
		return fmt.Errorf("%w: %v", ErrOwnershipUnavailable, err)
	}
	if len(objects) > 0 {
		return &HasObjectsError{Objects: objects}
	}
	if err := s.store.DeleteUser(ctx, username); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			return ErrNotFound
		case errors.Is(err, ErrLastOwner):
			return ErrLastOwner
		default:
			return s.storeFailure("delete account", err)
		}
	}
	s.cache.removeUser(username)
	return nil
}

// BootstrapOwner creates the first Owner when the database holds none. An
// existing Owner is reused, never duplicated (C-54, C-63).
func (s *Service) BootstrapOwner(ctx context.Context, username string) (Bootstrap, error) {
	existing, err := s.store.FirstOwner(ctx)
	if err == nil {
		return Bootstrap{Username: existing.Username}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Bootstrap{}, s.storeFailure("read first owner", err)
	}
	if err := validateUsername(username); err != nil {
		return Bootstrap{}, err
	}
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
	id, err := newUserID()
	if err != nil {
		s.log.Error("generate account id", "error", err)
		return Bootstrap{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	now := s.now()
	user := User{
		ID:                 id,
		Username:           username,
		Role:               write.RoleOwner,
		PasswordHash:       digest,
		MustChangePassword: true,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := s.store.CreateUser(ctx, user); err != nil {
		return Bootstrap{}, err
	}
	return Bootstrap{Created: true, Username: username, OneTimePassword: oneTime}, nil
}

// Now is the service clock, so response deadlines use the same instant the
// decisions did.
func (s *Service) Now() time.Time { return s.now() }

// storeFailure logs a database failure and reports it as unavailability. A
// caller maps it to the one stable code that tells a user to retry (C-61).
func (s *Service) storeFailure(operation string, err error) error {
	s.log.Error("control-plane database operation failed", "operation", operation, "error", err)
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

// sessionFrom projects a stored session with the remaining bounds computed
// against now.
func sessionFrom(stored StoredSession, now time.Time) Session {
	effective := stored.IdleExpiresAt
	if stored.AbsoluteExpiresAt.Before(effective) {
		effective = stored.AbsoluteExpiresAt
	}
	return Session{
		Username:           stored.Username,
		Role:               stored.Role,
		MustChangePassword: stored.MustChangePassword,
		ExpiresAt:          effective,
		IdleExpiresAt:      stored.IdleExpiresAt,
		AbsoluteExpiresAt:  stored.AbsoluteExpiresAt,
		Remaining:          positive(effective.Sub(now)),
		IdleRemaining:      positive(stored.IdleExpiresAt.Sub(now)),
		AbsoluteRemaining:  positive(stored.AbsoluteExpiresAt.Sub(now)),
		tokenHash:          stored.TokenHash,
	}
}

// emptyAccountDigest is the digest an unknown login name is checked against.
// It is built once; the password it hashes is not an account password.
var emptyAccountDigest = sync.OnceValue(func() string {
	digest, err := hashPassword("identity-unknown-login-placeholder")
	if err != nil {
		return ""
	}
	return digest
})

// positive clamps a deadline difference at zero.
func positive(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	return value
}

// accountFromUser renders a stored user for the API.
func accountFromUser(user User) Account {
	return Account{
		Username:           user.Username,
		Role:               user.Role,
		Disabled:           user.Disabled,
		MustChangePassword: user.MustChangePassword,
		CreatedAt:          user.CreatedAt,
	}
}

// validRole reports whether role is one of the three roles (C-59).
func validRole(role write.Role) bool {
	switch role {
	case write.RoleOwner, write.RoleMember, write.RoleGuest:
		return true
	default:
		return false
	}
}

// validateUsername rejects a login name the API cannot serve or display.
func validateUsername(username string) error {
	if username == "" {
		return invalid("登录名不能为空")
	}
	if username != strings.TrimSpace(username) {
		return invalid("登录名不能带首尾空白")
	}
	if utf8.RuneCountInString(username) > MaxUsernameLength {
		return invalid("登录名过长")
	}
	for _, character := range username {
		if unicode.IsControl(character) || character == '/' {
			return invalid("登录名不能包含控制字符或斜杠")
		}
	}
	return nil
}

// validatePassword enforces the input bounds on a password a user sets.
func validatePassword(password string) error {
	runes := utf8.RuneCountInString(password)
	if runes < MinPasswordLength {
		return invalid(fmt.Sprintf("口令至少 %d 个字符", MinPasswordLength))
	}
	if runes > MaxPasswordLength {
		return invalid(fmt.Sprintf("口令最长 %d 个字符", MaxPasswordLength))
	}
	return nil
}
