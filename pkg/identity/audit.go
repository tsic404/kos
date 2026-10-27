package identity

import (
	"context"
	"errors"

	"github.com/tsic404/kos/pkg/controlplane"
)

// Operator implements controlplane.Actor (C-14): the account behind the
// resolved session names the write. No session means no name, and the control
// plane records its process subject instead.
func Operator(ctx context.Context) string {
	session, ok := FromContext(ctx)
	if !ok {
		return ""
	}
	return session.Username
}

// AuditTrail adapts the account database to the control plane's audit seam
// (C-14): every accepted write is appended to the same store the accounts and
// sessions live in, so the trail is exported and restored with them.
type AuditTrail struct {
	// Store is the account database; required.
	Store *Store
}

// Record implements controlplane.Auditor.
func (t AuditTrail) Record(ctx context.Context, record controlplane.AuditRecord) error {
	if t.Store == nil {
		return errors.New("identity: an audit trail needs a store")
	}
	return t.Store.AppendAudit(ctx, AuditEntry{
		Actor:      record.Actor,
		Object:     record.Object.String(),
		Verb:       record.Verb,
		Result:     record.Result,
		Source:     record.Source,
		OccurredAt: record.OccurredAt,
	})
}
