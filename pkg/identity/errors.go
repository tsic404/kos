package identity

import (
	"errors"
	"fmt"
	"time"
)

// Domain failures. The HTTP layer maps each one to a stable code, so a caller
// never has to parse a message.
var (
	// ErrUnavailable means the control-plane database cannot answer; reads
	// already signed in may still be served from memory (C-61).
	ErrUnavailable = errors.New("identity: the control-plane database is unavailable")
	// ErrLoginFailed is the one answer a missing account, a wrong password and
	// a disabled account share (C-63).
	ErrLoginFailed = errors.New("identity: the login name and password do not match")
	// ErrUnauthenticated means no usable session backs the request.
	ErrUnauthenticated = errors.New("identity: the session is missing, expired or revoked")
	// ErrNotFound means the account does not exist.
	ErrNotFound = errors.New("identity: the account does not exist")
	// ErrUsernameTaken means the login name already belongs to an account.
	ErrUsernameTaken = errors.New("identity: the login name is taken")
	// ErrPermissionDenied means the caller's role does not allow the request.
	ErrPermissionDenied = errors.New("identity: the caller's role does not allow this")
	// ErrLastOwner means a change would leave no usable Owner (C-63).
	ErrLastOwner = errors.New("identity: the last Owner cannot be disabled, demoted or deleted")
	// ErrCurrentPassword means the password a caller offered as proof is wrong.
	ErrCurrentPassword = errors.New("identity: the current password is wrong")
	// ErrPasswordChangeRequired means a one-time password is still in use and
	// only the routes that replace it may run (C-63).
	ErrPasswordChangeRequired = errors.New("identity: the one-time password must be changed first")
	// ErrOwnershipUnavailable means deletion cannot tell whether the account
	// still owns objects; deleting anyway would strand them (C-60).
	ErrOwnershipUnavailable = errors.New("identity: the ownership source is not configured")
)

// ValidationError rejects an input the service cannot accept. The message is
// written for the caller.
type ValidationError struct {
	Message string
}

// Error implements error.
func (e *ValidationError) Error() string { return e.Message }

// invalid builds a validation failure.
func invalid(message string) error { return &ValidationError{Message: message} }

// LockedError reports a login name inside its five-minute lock window (C-63).
// Remaining is what the caller waits before another attempt can be judged.
type LockedError struct {
	// Remaining is the part of the lock window still ahead.
	Remaining time.Duration
}

// Error implements error.
func (e *LockedError) Error() string {
	return fmt.Sprintf("identity: the login name is locked for %s", e.Remaining)
}

// HasObjectsError refuses an account deletion that still owns objects (C-60).
type HasObjectsError struct {
	// Objects are the objects the account still owns.
	Objects []OwnedObject
}

// Error implements error.
func (e *HasObjectsError) Error() string {
	return fmt.Sprintf("identity: the account still owns %d object(s)", len(e.Objects))
}
