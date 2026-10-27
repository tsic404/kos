package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/write"
)

// User actions an application accepts (C-33, C-66). They are submitted through
// the control plane and carried as one-shot action records in the KOS namespace;
// the application reconciler reads and deletes them.
const (
	// ActionRetry leaves Failed and resets the retry budget (C-33).
	ActionRetry = "retry"
	// ActionOrphan gives up the blocked downstream objects and keeps the data
	// (C-33).
	ActionOrphan = "orphan"
	// ActionConfirm accepts an external change as the new baseline (C-35).
	ActionConfirm = "confirm"
	// ActionRestart rebuilds the rendered workloads once without writing the
	// user spec (C-66).
	ActionRestart = "restart"
)

// Application actions the control plane performs as a spec write (C-66). They
// are not one-shot records: stop writes the pause marker, start clears it, and
// scale writes the replica count.
const (
	// ActionStop writes the pause marker without touching the replica count.
	ActionStop = "stop"
	// ActionStart clears the pause marker.
	ActionStart = "start"
	// ActionScale writes the desired replica count.
	ActionScale = "scale"
)

// Action record and abandonment manifest carriers and labels.
const (
	// ActionLabel names the action a record carries. Only action records carry
	// it: the abandonment manifest is not an action and must never be consumed as
	// one (C-33).
	ActionLabel = write.ActionRecordLabel
	// ActionOwnerLabel binds a record to the application UID, so one
	// application never consumes another's action.
	ActionOwnerLabel = "kos.tsic.top/action-owner"
	// ActionDataKey is the record field naming the target application.
	ActionDataKey = "application"
	// ActionTargetDigestKey is the confirmation record field naming the target
	// digest the user accepts (C-35).
	ActionTargetDigestKey = "targetDigest"
	// ActionSpecDigestKey is the confirmation record field carrying the digest
	// of the spec that was stored when the record was written (C-35).
	ActionSpecDigestKey = "specDigest"
	// AbandonmentKey holds the abandonment manifest in its record.
	AbandonmentKey = "abandonment"
	// actionRecordPrefix and abandonmentPrefix keep both objects inside the KOS
	// namespace and tied to one installation.
	actionRecordPrefix = write.ActionRecordPrefix
	abandonmentPrefix  = "kos-app-abandonment-"
)

// Abandonment dispositions (C-33).
const (
	// DispositionAbandoned marks a downstream object KOS stopped managing.
	DispositionAbandoned = "abandoned"
	// DispositionRetained marks data that stays after the uninstall.
	DispositionRetained = "retained"
)

// Abandonment is one entry of an application's abandonment manifest.
type Abandonment struct {
	// Object is the object the entry names.
	Object v1beta1.ObjectRef `json:"object"`
	// Disposition says whether it was abandoned or retained.
	Disposition string `json:"disposition"`
}

// ActionRecordName is the name of the record carrying one action for one
// application.
func ActionRecordName(applicationUID string, action string) string {
	return actionRecordPrefix + applicationUID + "-" + action
}

// ConfirmRecordName is the name of the record carrying one confirmation of one
// target (C-35). The target is part of the name, so two confirmations of
// different targets are two records while a repeated one is idempotent (C-12).
func ConfirmRecordName(applicationUID, targetDigest string) string {
	sum := sha256.Sum256([]byte(targetDigest))
	return actionRecordPrefix + applicationUID + "-" + ActionConfirm + "-" + hex.EncodeToString(sum[:6])
}

// AbandonmentName is the name of the abandonment manifest of one application.
func AbandonmentName(applicationUID string) string {
	return abandonmentPrefix + applicationUID
}

// EncodeAbandonment renders a manifest into its record value.
func EncodeAbandonment(entries []Abandonment) (string, error) {
	if entries == nil {
		entries = []Abandonment{}
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("encode the abandonment manifest: %w", err)
	}
	return string(encoded), nil
}

// DecodeAbandonment reads a manifest from its record value.
func DecodeAbandonment(text string) ([]Abandonment, error) {
	if text == "" {
		return nil, nil
	}
	var entries []Abandonment
	if err := json.Unmarshal([]byte(text), &entries); err != nil {
		return nil, fmt.Errorf("decode the abandonment manifest: %w", err)
	}
	return entries, nil
}
