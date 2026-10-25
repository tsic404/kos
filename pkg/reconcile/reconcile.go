// Package reconcile is the kernel's generic reconcile loop: a deduplicating
// work queue, one fixed time budget per round, and the metrics that expose
// rounds that exceed it (C-06).
//
// The budget reaches a reconciler as the round context's deadline. Go cannot
// preempt a running function, so the budget is cooperative: a reconciler must
// return when the context is done. One that ignores the context keeps its
// worker blocked until it returns, and its overrun is recorded then.
//
// A Request carries names, never a snapshot: every round reads the current
// object, so a retry after a restart or an out-of-order enqueue sees the same
// facts as any other round (C-06).
package reconcile

import (
	"context"
	"time"
)

// DefaultMaxRoundDuration is the fixed time budget of one round when the
// caller does not set one (C-06).
const DefaultMaxRoundDuration = 30 * time.Second

// Request names one object to reconcile. It is the queue key, so it must stay
// comparable.
type Request struct {
	Kind      string
	Namespace string
	Name      string
}

// Result tells the loop how to continue after a settled round.
type Result struct {
	// RequeueAfter asks for another observation after the delay. A long action
	// is expressed as a Job (or another durable object) and the round returns
	// here instead of waiting inline; the next observation advances it (C-06).
	RequeueAfter time.Duration
}

// Reconciler applies the desired state of one object.
type Reconciler interface {
	// Reconcile runs one round. ctx carries the round's fixed time budget as
	// its deadline: return when it is done. The kernel cannot preempt a
	// reconciler that ignores ctx, so that reconciler blocks the object's
	// worker until it returns. A returned error asks for a rate-limited retry.
	Reconcile(ctx context.Context, request Request) (Result, error)
}
