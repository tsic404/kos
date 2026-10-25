// Package serial runs work under per-object exclusion: two multi-step actions
// for the same object never interleave, while work on other objects proceeds
// concurrently (C-05).
//
// Reconcile rounds are serialized by pkg/reconcile's deduplicating queue; this
// gate covers the read-modify-write actions outside that loop (adoption,
// release, decline). The two layers share no lock; a write racing a round is
// settled by the API server's own conflict judgement -- field-set ownership on
// the server-side apply path, the read object's resourceVersion on the update
// path (C-05) -- not by a second object gate.
package serial

import (
	"context"
	"sync"
)

// Gate holds one slot per object key.
//
// The zero value is not usable; build gates with New.
type Gate struct {
	mu   sync.Mutex
	held map[string]*slot
}

// slot is one object's exclusion: a token channel of capacity one plus the
// number of callers holding or waiting for it.
type slot struct {
	token chan struct{}
	refs  int
}

// New returns an empty gate.
func New() *Gate {
	return &Gate{held: make(map[string]*slot)}
}

// Do runs fn while holding key. A second call for the same key waits until the
// first returns; a caller whose context ends while waiting returns ctx.Err()
// without running fn.
func (g *Gate) Do(ctx context.Context, key string, fn func(context.Context) error) error {
	entry := g.acquire(key)
	defer g.release(key, entry)

	select {
	case entry.token <- struct{}{}:
		defer func() { <-entry.token }()
		return fn(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// acquire takes a reference on the key's slot, creating it when absent.
func (g *Gate) acquire(key string) *slot {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.held[key]
	if !ok {
		entry = &slot{token: make(chan struct{}, 1)}
		g.held[key] = entry
	}
	entry.refs++
	return entry
}

// release drops a reference and forgets slots nobody holds or waits for.
func (g *Gate) release(key string, entry *slot) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry.refs--
	if entry.refs == 0 {
		delete(g.held, key)
	}
}
