package workerrpc

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestWakeSignalCoalescingAndNonBlocking(t *testing.T) {
	var invocations atomic.Int64
	blockCh := make(chan struct{})

	signal := NewWakeSignal(func(ctx context.Context) error {
		invocations.Add(1)
		select {
		case <-blockCh:
		case <-ctx.Done():
		}
		return nil
	}, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go signal.Run(ctx)

	// First wake initiates the call (blocked on blockCh)
	if err := signal.Wake(context.Background()); err != nil {
		t.Fatalf("first wake failed: %v", err)
	}

	// Wait until runner starts invocation
	for range 50 {
		if invocations.Load() > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Burst 10 wakes while runner is blocked
	for range 10 {
		if err := signal.Wake(context.Background()); err != nil {
			t.Fatalf("burst wake failed: %v", err)
		}
	}

	// Release block
	close(blockCh)

	// Wait for runner to process coalesced hint
	time.Sleep(60 * time.Millisecond)

	count := invocations.Load()
	if count < 1 || count > 3 {
		t.Fatalf("expected 1-2 coalesced invocations, got %d", count)
	}

	// Cancel context and check that Wake returns Canceled
	cancel()
	time.Sleep(10 * time.Millisecond)

	canceledCtx, cancelImmediate := context.WithCancel(context.Background())
	cancelImmediate()
	if err := signal.Wake(canceledCtx); err == nil {
		t.Fatal("expected error on canceled context")
	}
	if err := signal.Wake(context.Background()); err != context.Canceled {
		t.Fatalf("expected context.Canceled on stopped runner, got %v", err)
	}
}
