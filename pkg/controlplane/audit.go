package controlplane

import (
	"context"
	"log/slog"
	"time"
	"unicode"

	"github.com/tsic404/kos/pkg/write"
)

// Results an audit record carries: a write either took effect or was refused,
// and both outcomes are recorded (C-14).
const (
	// ResultAccepted marks a write the control plane carried out.
	ResultAccepted = "accepted"
	// ResultRejected marks a write the control plane refused.
	ResultRejected = "rejected"
	// SourceAPI names the control-plane API as where a write came from.
	SourceAPI = "api"
)

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
	// Result is the outcome, ResultAccepted or ResultRejected.
	Result string `json:"result"`
	// Source is where the request came from.
	Source string `json:"source"`
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

// audit records one accepted write.
func (s *Server) audit(ctx context.Context, actor, verb string, ref ObjectRef) {
	if s.auditor == nil {
		return
	}
	RecordAudit(ctx, s.auditor, s.log, AuditRecord{
		Actor:      actor,
		Object:     ref,
		Verb:       verb,
		Result:     ResultAccepted,
		Source:     SourceAPI,
		OccurredAt: s.now().UTC(),
	})
}
