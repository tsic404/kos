package app

import (
	"encoding/json"
	"fmt"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/write"
)

// User actions an application accepts (C-33). They are submitted through the
// control plane and carried as one-shot action records in the KOS namespace;
// the application reconciler reads and deletes them.
const (
	// ActionRetry leaves Failed and resets the retry budget (C-33).
	ActionRetry = "retry"
	// ActionOrphan gives up the blocked downstream objects and keeps the data
	// (C-33).
	ActionOrphan = "orphan"
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
