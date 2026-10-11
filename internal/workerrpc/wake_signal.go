package workerrpc

import (
	"context"
	"sync"
	"time"
)

// WakeSignal is a coalescing hint runner that decouples fast callers (such as
// payment webhook handlers) from slower RPCs or background triggers.
type WakeSignal struct {
	fn      func(context.Context) error
	timeout time.Duration
	ch      chan struct{}
	stopped chan struct{}
	once    sync.Once
}

// NewWakeSignal creates a new coalescing wake runner for fn.
// If timeout <= 0, 2 seconds is used by default.
func NewWakeSignal(fn func(context.Context) error, timeout time.Duration) *WakeSignal {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &WakeSignal{
		fn:      fn,
		timeout: timeout,
		ch:      make(chan struct{}, 1),
		stopped: make(chan struct{}),
	}
}

// Wake enqueues a hint without blocking or waiting for fn to complete.
// If the channel is already full, it succeeds immediately because a hint is already pending.
// If ctx is already canceled or the runner is stopped, it returns context.Canceled.
func (w *WakeSignal) Wake(ctx context.Context) error {
	select {
	case <-w.stopped:
		return context.Canceled
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	select {
	case w.ch <- struct{}{}:
	default:
		// Capacity 1 full: coalesced hint already queued.
	}
	return nil
}

// Run executes the runner loop until lifecycle ctx is canceled.
// It invokes fn with a timeout derived from lifecycle ctx without keeping the caller context.
func (w *WakeSignal) Run(ctx context.Context) {
	defer w.once.Do(func() { close(w.stopped) })
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.ch:
			select {
			case <-w.ch:
			default:
			}
			callCtx, cancel := context.WithTimeout(ctx, w.timeout)
			_ = w.fn(callCtx)
			cancel()
		}
	}
}
