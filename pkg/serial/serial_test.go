package serial

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDoSerializesOneKey proves callers for the same key never overlap and that
// every one of them still runs.
func TestDoSerializesOneKey(t *testing.T) {
	gate := New()
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	var calls atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := gate.Do(context.Background(), "one", func(context.Context) error {
				current := inFlight.Add(1)
				defer inFlight.Add(-1)
				for {
					seen := maxInFlight.Load()
					if current <= seen || maxInFlight.CompareAndSwap(seen, current) {
						break
					}
				}
				calls.Add(1)
				time.Sleep(5 * time.Millisecond)
				return nil
			})
			if err != nil {
				t.Errorf("caller: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := calls.Load(); got != 8 {
		t.Fatalf("%d of 8 calls ran through the gate", got)
	}
	if got := maxInFlight.Load(); got != 1 {
		t.Errorf("%d callers ran at once for one key, want 1", got)
	}
}

// TestDoRunsDifferentKeysTogether proves the gate is per key, not one global
// lock.
func TestDoRunsDifferentKeysTogether(t *testing.T) {
	gate := New()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup

	for _, key := range []string{"a", "b"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			err := gate.Do(context.Background(), key, func(context.Context) error {
				started <- struct{}{}
				<-release
				return nil
			})
			if err != nil {
				t.Errorf("key %s: %v", key, err)
			}
		}(key)
	}

	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("two keys did not run at the same time")
		}
	}
	close(release)
	wg.Wait()
}

// TestDoHonorsCancelledWait proves a waiter that gives up never runs its work
// and leaves no stuck slot.
func TestDoHonorsCancelledWait(t *testing.T) {
	gate := New()
	holding := make(chan struct{})
	release := make(chan struct{})

	go func() {
		_ = gate.Do(context.Background(), "key", func(context.Context) error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := gate.Do(ctx, "key", func(context.Context) error {
		ran = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v, want context.Canceled", err)
	}
	if ran {
		t.Fatal("cancelled waiter ran its work")
	}

	close(release)
	// The slot must be usable after the holder and the cancelled waiter are
	// gone.
	if err := gate.Do(context.Background(), "key", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("gate did not recover after a cancelled wait: %v", err)
	}
}

// TestDoSerializesReadModifyWrite proves the gate turns a lost-update race into
// two applied updates, which is what per-object reconcile serialization buys.
func TestDoSerializesReadModifyWrite(t *testing.T) {
	gate := New()
	var counter int
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := gate.Do(context.Background(), "object", func(context.Context) error {
				read := counter
				time.Sleep(time.Microsecond)
				counter = read + 1
				return nil
			}); err != nil {
				t.Errorf("gated update: %v", err)
			}
		}()
	}
	wg.Wait()
	if counter != 20 {
		t.Fatalf("counter = %d after 20 serialized updates, want 20", counter)
	}
}
