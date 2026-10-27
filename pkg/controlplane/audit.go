package controlplane

import (
	"context"
	"log/slog"
	"time"
	"unicode"

	"github.com/tsic404/kos/pkg/write"
)

// Results an audit record carries: a write either took effect, was refused, or
// was permitted and then failed. Every outcome is recorded, so an attempt is
// never invisible (C-14).
const (
	// ResultAccepted marks a write the control plane carried out.
	ResultAccepted = "accepted"
	// ResultRejected marks a write the control plane refused.
	ResultRejected = "rejected"
	// ResultFailed marks a permitted write that then failed.
	ResultFailed = "failed"
)

// SourceAPI names the control-plane API as where a write came from.
const SourceAPI = "api"

// Actor names the principal behind one request (C-14). The control plane
// carries no session itself: the surface that mounts it resolves the session
// and puts it on the request context, and this hook reads it. An empty name
// means the request cannot be attributed, and the process subject is recorded.
type Actor func(ctx context.Context) string

// MaxRecordNameLength bounds one name an audit record carries as its object, in
// bytes; it matches the object-name limit the control plane already serves.
const MaxRecordNameLength = 253

// RecordableName reports whether one name fits an audit record's object: it must
// be non-empty, within MaxRecordNameLength, and carry no path separator or
// control character, either of which would make the recorded object ambiguous.
// A write surface refuses a target that fails this before the record is written,
// so unbounded input never reaches the trail.
func RecordableName(name string) bool {
	if name == "" || len(name) > MaxRecordNameLength {
		return false
	}
	for _, character := range name {
		if unicode.IsControl(character) || character == '/' {
			return false
		}
	}
	return true
}

// AuditRecord is one write attempt as the audit trail keeps it (C-14): who
// acted, on what, with which verb, with which result, from where, and when.
type AuditRecord struct {
	// Actor names the principal the write is attributed to.
	Actor string `json:"actor"`
	// Object is the object the write concerns.
	Object ObjectRef `json:"object"`
	// Verb is the operation the write performed; application lifecycle actions
	// use stop, start, restart and scale.
	Verb string `json:"verb"`
	// Result is the outcome, one of the Result constants.
	Result string `json:"result"`
	// Source is where the request came from.
	Source string `json:"source"`
	// Extension is the identifier of the extension declaring the object's
	// class; empty for a kernel object (C-14).
	Extension string `json:"extension,omitempty"`
	// OccurredAt is when the attempt happened.
	OccurredAt time.Time `json:"occurredAt"`
}

// Auditor stores the audit trail (C-14). A nil Auditor means no trail is
// configured; the write still happens, because the trail is not on the
// correctness path of the object.
type Auditor interface {
	// Record appends one record. It must never rewrite an existing one.
	Record(ctx context.Context, record AuditRecord) error
}

// actorOf resolves the principal of one request, falling back to the process
// subject when no hook names a person (C-14).
func (s *Server) actorOf(ctx context.Context) string {
	if s.actor != nil {
		if name := s.actor(ctx); name != "" {
			return name
		}
	}
	return string(write.SubjectAPIProcess)
}

// RecordAudit appends one record to a trail. A nil trail records nothing, and a
// nil log discards the failure report; a failing trail never fails the caller,
// because the write the record describes has already happened and C-14 owns the
// remedy, not this call.
func RecordAudit(ctx context.Context, auditor Auditor, log *slog.Logger, record AuditRecord) {
	if auditor == nil {
		return
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := auditor.Record(ctx, record); err != nil {
		log.Error("audit record failed", "object", record.Object.String(), "verb", record.Verb, "error", err)
	}
}

// actorOfGrant resolves the principal of one granted request while its context
// is still alive, so the record of a write that outlives the client keeps the
// account it was made by (C-12, C-14).
func (s *Server) actorOfGrant(ctx context.Context, grant Grant) string {
	if grant.Actor != "" {
		return grant.Actor
	}
	return s.actorOf(ctx)
}

// audit records one write attempt under the session that made it (C-14). The
// actor is resolved by the caller before its request ends, and ctx is the
// context the write itself runs on: an accepted write outlives its client
// (C-12), so the record must not be bound to the request context.
func (s *Server) audit(ctx context.Context, actor, extension string, ref ObjectRef, verb, result string) {
	if verb == "" {
		return
	}
	RecordAudit(ctx, s.auditor, s.log, AuditRecord{
		Actor:      actor,
		Object:     ref,
		Verb:       verb,
		Result:     result,
		Source:     SourceAPI,
		Extension:  extension,
		OccurredAt: s.now().UTC(),
	})
}

// auditFailure records a permitted write that failed and returns the failure,
// so a handler writes the envelope and the record in one step (C-14).
func (s *Server) auditFailure(ctx context.Context, actor, extension string, ref ObjectRef, verb string, failure *Error) *Error {
	s.audit(ctx, actor, extension, ref, verb, ResultFailed)
	return failure
}
