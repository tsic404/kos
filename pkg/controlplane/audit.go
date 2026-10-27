package controlplane

import (
	"context"
	"time"

	"github.com/tsic404/kos/pkg/write"
)

// Actor names the principal behind one request (C-14). The control plane
// carries no session itself: the surface that mounts it resolves the session
// and puts it on the request context, and this hook reads it. An empty name
// means the request cannot be attributed, and the process subject is recorded.
type Actor func(ctx context.Context) string

// AuditRecord is one accepted write as the audit trail keeps it (C-14): who
// acted, on what, with which verb, with which result, from where, and when.
type AuditRecord struct {
	// Actor names the principal the write is attributed to.
	Actor string `json:"actor"`
	// Object is the object the write concerns.
	Object ObjectRef `json:"object"`
	// Verb is the operation the write performed; application lifecycle actions
	// use stop, start, restart and scale.
	Verb string `json:"verb"`
	// Result is the outcome, "accepted" for a write this API carried out.
	Result string `json:"result"`
	// Source is where the request came from.
	Source string `json:"source"`
	// OccurredAt is when the write was accepted.
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

// audit records one accepted write. A failing trail is logged and does not fail
// the request: the write already happened, and C-14 owns the remedy, not this
// call.
func (s *Server) audit(ctx context.Context, actor, verb string, ref ObjectRef) {
	if s.auditor == nil {
		return
	}
	record := AuditRecord{
		Actor:      actor,
		Object:     ref,
		Verb:       verb,
		Result:     "accepted",
		Source:     "api",
		OccurredAt: s.now().UTC(),
	}
	if err := s.auditor.Record(ctx, record); err != nil {
		s.log.Error("audit record failed", "object", ref.String(), "verb", verb, "error", err)
	}
}
