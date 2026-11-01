package identity

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/controlplane"
)

// TestRecentAuditEntriesForObject checks the per-object read a reader judging an
// object's latest fate depends on: it returns that object's newest records for
// the asked verbs, ignores every other object and verb, and stays answerable
// after far more unrelated writes than one global window would hold (C-14,
// C-65).
func TestRecentAuditEntriesForObject(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, OpenOptions{Path: t.TempDir() + "/kos.db"})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	moment := time.Now().UTC()
	object := api.Group + "/Node/node-a"
	appendRecord := func(entry AuditEntry) {
		t.Helper()
		if entry.Result == "" {
			entry.Result = controlplane.ResultAccepted
		}
		if entry.Source == "" {
			entry.Source = controlplane.SourceAPI
		}
		entry.OccurredAt = moment
		if err := store.AppendAudit(ctx, entry); err != nil {
			t.Fatalf("append record %+v: %v", entry, err)
		}
	}

	appendRecord(AuditEntry{Object: object, Verb: "maintain"})
	appendRecord(AuditEntry{Object: object, Verb: "set"})
	appendRecord(AuditEntry{Object: object, Verb: "restore"})
	appendRecord(AuditEntry{Object: api.Group + "/Node/node-b", Verb: "maintain"})
	// Attempts that wrote nothing: they stay in the trail but never become the
	// accepted answer the origin rule reads.
	appendRecord(AuditEntry{Object: object, Verb: "restore", Result: controlplane.ResultFailed})
	appendRecord(AuditEntry{Object: object, Verb: "maintain", Result: controlplane.ResultRejected})
	// More unrelated writes than one global window would ever hold: they must
	// not affect the answer for node-a.
	for index := range 600 {
		appendRecord(AuditEntry{Object: api.Group + "/Setting/user-" + strconv.Itoa(index), Verb: "set"})
	}

	maintenance, err := store.RecentAuditEntriesForObject(ctx, object, []string{"maintain", "restore"}, nil, 1)
	if err != nil {
		t.Fatalf("read node-a maintenance records: %v", err)
	}
	if len(maintenance) != 1 || maintenance[0].Verb != "maintain" {
		t.Fatalf("node-a maintenance records = %+v, want the newest attempt", maintenance)
	}

	accepted, err := store.RecentAuditEntriesForObject(ctx, object, []string{"maintain", "restore"}, []string{controlplane.ResultAccepted}, 1)
	if err != nil {
		t.Fatalf("read the accepted node-a maintenance records: %v", err)
	}
	if len(accepted) != 1 || accepted[0].Verb != "restore" || accepted[0].Result != controlplane.ResultAccepted {
		t.Fatalf("accepted node-a maintenance records = %+v, want the newest accepted restore", accepted)
	}

	all, err := store.RecentAuditEntriesForObject(ctx, object, []string{"maintain", "restore"}, []string{controlplane.ResultAccepted}, 8)
	if err != nil {
		t.Fatalf("read every accepted node-a maintenance record: %v", err)
	}
	if len(all) != 2 || all[0].Verb != "restore" || all[1].Verb != "maintain" {
		t.Fatalf("accepted node-a maintenance records = %+v, want restore then maintain", all)
	}

	// The oldest-first read the export path uses keeps its order.
	oldest, err := store.AuditEntries(ctx, 1)
	if err != nil {
		t.Fatalf("read the oldest records: %v", err)
	}
	if len(oldest) != 1 || oldest[0].Verb != "maintain" {
		t.Fatalf("oldest records = %+v, want the first maintain", oldest)
	}
}
