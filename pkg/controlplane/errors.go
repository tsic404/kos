package controlplane

import (
	"fmt"
	"net/http"
)

// Stage names the step a request failed at (C-12).
type Stage string

const (
	// StageValidate means the request body or parameters were rejected.
	StageValidate Stage = "validate"
	// StageAuthorize means the caller may not perform the request.
	StageAuthorize Stage = "authorize"
	// StageCapability means the object class needs an extension this control
	// plane has not loaded.
	StageCapability Stage = "capability"
	// StagePlan means computing a plan failed.
	StagePlan Stage = "plan"
	// StageApply means the cluster refused or failed a write.
	StageApply Stage = "apply"
	// StageRead means reading the cluster failed.
	StageRead Stage = "read"
)

// SideEffects reports whether a failed request left objects behind (C-12).
type SideEffects string

const (
	// SideEffectsNone means nothing was written.
	SideEffectsNone SideEffects = "none"
	// SideEffectsApplied means objects were written before the failure; Applied
	// lists them so the caller knows what to inspect.
	SideEffectsApplied SideEffects = "applied"
)

// Error codes are the stable part of a failure: clients branch on the code,
// never on the message (C-12). A code never changes meaning within a major
// version.
const (
	// CodeInvalidRequest marks a malformed request.
	CodeInvalidRequest = "invalid_request"
	// CodeInvalidField marks a field the model does not define or a value it
	// rejects.
	CodeInvalidField = "invalid_field"
	// CodeNotFound marks a missing object or resource.
	CodeNotFound = "not_found"
	// CodePermissionDenied marks a request the caller may not make (C-59).
	CodePermissionDenied = "permission_denied"
	// CodeCapabilityMissing marks an object class whose extension is not
	// installed, so the control plane cannot serve it (C-19, C-21).
	CodeCapabilityMissing = "capability_missing"
	// CodeObjectExists marks a create whose name is taken by a different
	// desired state.
	CodeObjectExists = "object_exists"
	// CodeConflict marks a write whose precondition no longer holds.
	CodeConflict = "conflict"
	// CodeWriteFailed marks a write the cluster refused or failed.
	CodeWriteFailed = "write_failed"
	// CodeInternal marks a failure on this side of the API.
	CodeInternal = "internal"
)

// Error is the failure envelope every non-2xx response carries (C-12). The
// fields answer what failed, where, what it touched, and what the caller can do
// about it.
type Error struct {
	// Code is stable across the major version.
	Code string `json:"code"`
	// Message is written for a user, not for a log.
	Message string `json:"message"`
	// Stage is the step that failed.
	Stage Stage `json:"stage"`
	// Object names the object the failure is about, when there is one.
	Object *ObjectRef `json:"object,omitempty"`
	// Retryable tells whether repeating the same request can succeed.
	Retryable bool `json:"retryable"`
	// RequiresUserAction tells whether the failure waits on a decision or a
	// repair only a user can make.
	RequiresUserAction bool `json:"requiresUserAction"`
	// SideEffects tells whether the failure left objects behind.
	SideEffects SideEffects `json:"sideEffects"`
	// Applied lists the objects written before a failed write stopped.
	Applied []ObjectRef `json:"applied,omitempty"`
	// Current is the object as the cluster holds it now; a conflict carries it so
	// the caller can reconcile instead of guessing (C-05).
	Current map[string]any `json:"current,omitempty"`
}

// Error renders the code and message for wrapping in other errors.
func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// status maps a stable code to the HTTP status the response carries.
func (e *Error) status() int {
	switch e.Code {
	case CodeInvalidRequest, CodeInvalidField:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeCapabilityMissing:
		return http.StatusUnprocessableEntity
	case CodeObjectExists, CodeConflict:
		return http.StatusConflict
	case CodeWriteFailed:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// about attaches the object the failure concerns.
func (e *Error) about(ref ObjectRef) *Error {
	e.Object = &ref
	return e
}

// invalid requests a corrected request body; the caller can fix it alone.
func invalid(message string) *Error {
	return &Error{
		Code:               CodeInvalidRequest,
		Message:            message,
		Stage:              StageValidate,
		RequiresUserAction: true,
		SideEffects:        SideEffectsNone,
	}
}

// invalidField reports a field the model does not define or a value it rejects.
func invalidField(message string) *Error {
	return &Error{
		Code:               CodeInvalidField,
		Message:            message,
		Stage:              StageValidate,
		RequiresUserAction: true,
		SideEffects:        SideEffectsNone,
	}
}

// notFound reports a missing object; repeating the request cannot help.
func notFound(message string) *Error {
	return &Error{
		Code:               CodeNotFound,
		Message:            message,
		Stage:              StageValidate,
		RequiresUserAction: true,
		SideEffects:        SideEffectsNone,
	}
}

// conflict reports a write whose precondition no longer holds and carries the
// object as it stands now (C-05).
func conflict(ref ObjectRef, message string, current map[string]any) *Error {
	return &Error{
		Code:        CodeConflict,
		Message:     message,
		Stage:       StageApply,
		Object:      new(ref),
		Retryable:   true,
		SideEffects: SideEffectsNone,
		Current:     current,
	}
}

// internal reports a failure on this side; repeating the request may work.
func internal(stage Stage, message string) *Error {
	return &Error{
		Code:        CodeInternal,
		Message:     message,
		Stage:       stage,
		Retryable:   true,
		SideEffects: SideEffectsNone,
	}
}
