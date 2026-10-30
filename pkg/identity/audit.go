package identity

import (
	"context"
	"errors"
	"strconv"

	"github.com/tsic404/kos/api"
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
// (C-14): every write attempt is appended to the same store the accounts and
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

// Audit vocabulary of a preference write (C-14).
const (
	// preferenceKind names a preference in the audit trail.
	preferenceKind = "Preference"
	// preferenceVerb names the write that assigns a value.
	preferenceVerb = "set"
)

// validatePreferenceTarget refuses a domain or key the audit record cannot
// hold, so a malformed request cannot grow the trail. A declaration only fixes
// that a key is non-empty, so the bound covers length and format rather than
// imposing a name shape C-27 never required.
func validatePreferenceTarget(identifier, key string) error {
	if !controlplane.RecordableName(identifier) {
		return invalid("偏好域无效或超过 " + strconv.Itoa(controlplane.MaxRecordNameLength) + " 字节")
	}
	if !controlplane.RecordableName(key) {
		return invalid("偏好键无效或超过 " + strconv.Itoa(controlplane.MaxRecordNameLength) + " 字节")
	}
	return nil
}

// recordPreference appends the audit record of one preference write attempt
// (C-14). The account is the operator and the object's scope, the extension
// names the domain and the key names what was written; a refused attempt is
// recorded as well.
func (s *Service) recordPreference(ctx context.Context, session Session, identifier, key string, failure error) {
	controlplane.RecordAudit(ctx, s.auditor, s.log, controlplane.AuditRecord{
		Actor:      session.Username,
		Object:     controlplane.ObjectRef{Group: api.Group, Kind: preferenceKind, Namespace: identifier, Name: key},
		Verb:       preferenceVerb,
		Result:     preferenceResult(failure),
		Source:     controlplane.SourceAPI,
		OccurredAt: s.now().UTC(),
	})
}

// preferenceResult names the outcome of one attempt.
func preferenceResult(failure error) string {
	if failure != nil {
		return controlplane.ResultRejected
	}
	return controlplane.ResultAccepted
}
