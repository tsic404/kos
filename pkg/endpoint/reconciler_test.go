package endpoint

import (
	"context"
	"testing"
	"time"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/write"
)

// TestPendingEntryKeepsItsBudget states the replay budget of a failed settle:
// the attempt count survives the retry, the delay doubles from ten seconds, and
// the fifth failure drops the entry instead of retrying forever (C-15, C-06).
func TestPendingEntryKeepsItsBudget(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	reconciler := &Reconciler{pending: map[string]*pendingEntry{}, Now: func() time.Time { return now }}
	ref := entry("first", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "first.test"}).Ref

	reconciler.keepPending(ref)
	if due := reconciler.takeDuePending(); len(due) != 0 {
		t.Fatalf("an entry was due before its delay: %v", due)
	}
	if hint := reconciler.pendingHint(); hint.RequeueAfter != pendingBaseDelay {
		t.Fatalf("requeue hint = %v, want %v", hint.RequeueAfter, pendingBaseDelay)
	}

	// Each failure pushes the next attempt out by twice the previous delay.
	for attempt := 1; attempt < pendingAttempts-1; attempt++ {
		now = now.Add(pendingDelay(attempt))
		due := reconciler.takeDuePending()
		if len(due) != 1 || due[0].Name != "first" {
			t.Fatalf("attempt %d: due = %v, want the entry", attempt, due)
		}
		reconciler.keepPending(ref)
	}

	// The fifth failure exhausts the budget; nothing is left to replay.
	now = now.Add(pendingDelay(pendingAttempts - 1))
	if due := reconciler.takeDuePending(); len(due) != 1 {
		t.Fatalf("the last attempt was not due: %v", due)
	}
	reconciler.keepPending(ref)
	if due := reconciler.takeDuePending(); len(due) != 0 {
		t.Fatalf("an exhausted entry is still scheduled: %v", due)
	}
	if hint := reconciler.pendingHint(); hint.RequeueAfter != 0 {
		t.Fatalf("an empty retry set still asks for a round: %v", hint)
	}
}

// TestSettledEntryLeavesTheRetrySet states the other half: a retry that settles
// the entry clears it, so the next round does not replay it.
func TestSettledEntryLeavesTheRetrySet(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	reconciler := &Reconciler{pending: map[string]*pendingEntry{}, refused: map[string]*refusedEntry{}, Now: func() time.Time { return now }}
	ref := entry("first", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "first.test"}).Ref
	reconciler.keepPending(ref)
	reconciler.clearPending(ref)
	if due := reconciler.takeDuePending(); len(due) != 0 {
		t.Fatalf("a settled entry stayed in the retry set: %v", due)
	}
}

// TestUnsettleableEntryLeavesTheRetrySet states that a retry the ledger can no
// longer settle is dropped instead of replayed every round: the entry left the
// ledger or became read-only, so no round of its own will ever clear it (C-06).
func TestUnsettleableEntryLeavesTheRetrySet(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	reconciler := &Reconciler{
		pending: map[string]*pendingEntry{},
		refused: map[string]*refusedEntry{},
		ledger:  NewLedger(),
		Now:     func() time.Time { return now },
	}
	ref := entry("gone", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "gone.test"}).Ref
	reconciler.keepPending(ref)
	reconciler.markRefused(ref)

	// The entry left the ledger: no stored object names it any more.
	now = now.Add(pendingDelay(1))
	if due := reconciler.takeDuePending(); len(due) != 1 {
		t.Fatalf("the retry was not due: %v", due)
	}
	result, err := reconciler.settle(context.Background(), []write.Ref{ref}, write.Ref{})
	if err != nil {
		t.Fatalf("settle an entry the ledger lost: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("a dropped entry asked for another round: %v", result.RequeueAfter)
	}
	if due := reconciler.takeDuePending(); len(due) != 0 {
		t.Fatalf("an unsettleable entry stayed in the retry set: %v", due)
	}
	if due := reconciler.takeDueRefused(); len(due) != 0 {
		t.Fatalf("an unsettleable entry stayed in the re-observation set: %v", due)
	}
	if hint := reconciler.pendingHint(); hint.RequeueAfter != 0 {
		t.Fatalf("an empty schedule still asks for a round: %v", hint)
	}
}

// TestRefusedEntryIsReobservedOnTheInterval states the recovery source of a
// refused route write: the entry comes due after the fixed interval although
// the round that refused it belongs to another object (C-06, C-07).
func TestRefusedEntryIsReobservedOnTheInterval(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	reconciler := &Reconciler{pending: map[string]*pendingEntry{}, refused: map[string]*refusedEntry{}, Now: func() time.Time { return now }}
	ref := entry("survivor", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "shared.test"}).Ref
	reconciler.markRefused(ref)

	if due := reconciler.takeDueRefused(); len(due) != 0 {
		t.Fatalf("a refused entry was due before its interval: %v", due)
	}
	if hint := reconciler.pendingHint(); hint.RequeueAfter != routeReobserve {
		t.Fatalf("requeue hint = %v, want %v", hint.RequeueAfter, routeReobserve)
	}

	now = now.Add(routeReobserve)
	due := reconciler.takeDueRefused()
	if len(due) != 1 || due[0].Name != "survivor" {
		t.Fatalf("due = %v, want the refused entry", due)
	}
	reconciler.clearRefused(ref)
	if due := reconciler.takeDueRefused(); len(due) != 0 {
		t.Fatalf("a recovered entry stayed in the re-observation set: %v", due)
	}
	if hint := reconciler.pendingHint(); hint.RequeueAfter != 0 {
		t.Fatalf("an empty schedule still asks for a round: %v", hint)
	}
}
