package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"
)

// failingTrail refuses every record, so the failure path of the seam runs.
type failingTrail struct{}

// Record implements Auditor.
func (failingTrail) Record(context.Context, AuditRecord) error {
	return errors.New("trail unavailable")
}

// TestRecordAuditSurvivesAFailureWithoutALogger checks the seam a domain calls
// after the write has already happened: a failing trail with no logger
// configured must not panic the caller.
func TestRecordAuditSurvivesAFailureWithoutALogger(t *testing.T) {
	RecordAudit(context.Background(), failingTrail{}, nil, AuditRecord{
		Actor:      "owner",
		Object:     ObjectRef{Group: "kos.tsic.top", Kind: "Setting", Name: "drain-timeout"},
		Verb:       "set",
		Result:     ResultAccepted,
		Source:     SourceAPI,
		OccurredAt: time.Now(),
	})
}
