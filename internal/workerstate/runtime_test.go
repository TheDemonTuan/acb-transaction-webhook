package workerstate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorQuiesceHookFailureReturnsToReady(t *testing.T) {
	c := NewCoordinator()
	if err := c.SetReady(); err != nil {
		t.Fatal(err)
	}
	want := errors.New("persist failed")
	c.RegisterQuiesceHook(func(context.Context) error { return want })
	if err := c.Quiesce(context.Background()); !errors.Is(err, want) {
		t.Fatalf("expected hook failure, got %v", err)
	}
	if c.State() != StateReady {
		t.Fatalf("expected READY after failed quiesce, got %s", c.State())
	}
}

func TestCoordinatorLifecycle(t *testing.T) {
	c := NewCoordinator(
		WithDrainTimeout(100*time.Millisecond),
		WithShutdownTimeout(100*time.Millisecond),
	)

	// Initial state is STARTING
	if c.State() != StateStarting {
		t.Fatalf("expected STARTING, got %s", c.State())
	}
	if c.IsReady() {
		t.Fatal("expected IsReady() to be false while STARTING")
	}
	if err := c.CheckWorkAllowed(); err == nil {
		t.Fatal("expected CheckWorkAllowed() to fail while STARTING")
	}

	// Transition to READY
	if err := c.SetReady(); err != nil {
		t.Fatalf("SetReady failed: %v", err)
	}
	if c.State() != StateReady {
		t.Fatalf("expected READY, got %s", c.State())
	}
	if !c.IsReady() {
		t.Fatal("expected IsReady() to be true")
	}
	if err := c.CheckWorkAllowed(); err != nil {
		t.Fatalf("expected CheckWorkAllowed() to succeed while READY, got %v", err)
	}

	// Transitioning to READY again must fail
	if err := c.SetReady(); err == nil {
		t.Fatal("expected second SetReady() to fail")
	}

	// Register hooks
	var drainHookCalled, stopHookCalled atomic.Bool
	c.RegisterDrainHook(func(ctx context.Context) error {
		drainHookCalled.Store(true)
		return nil
	})
	c.RegisterStopHook(func(ctx context.Context) error {
		stopHookCalled.Store(true)
		return nil
	})

	// Transition to DRAINING
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("Drain failed: %v", err)
	}
	if c.State() != StateDraining {
		t.Fatalf("expected DRAINING, got %s", c.State())
	}
	if c.IsReady() {
		t.Fatal("expected IsReady() to be false while DRAINING")
	}
	if !c.IsDraining() {
		t.Fatal("expected IsDraining() to be true")
	}
	if !drainHookCalled.Load() {
		t.Fatal("expected drain hook to have been executed")
	}
	if err := c.CheckWorkAllowed(); !errors.Is(err, ErrWorkerDraining) {
		t.Fatalf("expected ErrWorkerDraining, got %v", err)
	}

	// Drain is idempotent
	if err := c.Drain(context.Background()); err != nil {
		t.Fatalf("second Drain failed: %v", err)
	}

	// Transition to STOPPING
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if c.State() != StateStopping {
		t.Fatalf("expected STOPPING, got %s", c.State())
	}
	if !stopHookCalled.Load() {
		t.Fatal("expected stop hook to have been executed")
	}
	if err := c.CheckWorkAllowed(); !errors.Is(err, ErrWorkerStopping) {
		t.Fatalf("expected ErrWorkerStopping, got %v", err)
	}

	// Drain after Stop returns ErrWorkerStopping
	if err := c.Drain(context.Background()); !errors.Is(err, ErrWorkerStopping) {
		t.Fatalf("expected ErrWorkerStopping when draining stopped worker, got %v", err)
	}
}

func TestCoordinatorDrainTimeout(t *testing.T) {
	c := NewCoordinator(WithDrainTimeout(30 * time.Millisecond))
	_ = c.SetReady()

	var hookExited atomic.Bool
	c.RegisterDrainHook(func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			hookExited.Store(true)
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
			return nil
		}
	})

	start := time.Now()
	err := c.Drain(context.Background())
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("Drain returned error: %v", err)
	}
	if duration > 150*time.Millisecond {
		t.Fatalf("Drain did not obey drain timeout: took %v", duration)
	}
	if !hookExited.Load() {
		t.Fatal("expected drain hook to observe context deadline")
	}
}

func TestCoordinatorConcurrentQueriesAndTransitions(t *testing.T) {
	c := NewCoordinator(
		WithDrainTimeout(50*time.Millisecond),
		WithShutdownTimeout(50*time.Millisecond),
	)

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					_ = c.State()
					_ = c.IsReady()
					_ = c.IsDraining()
					_ = c.CheckWorkAllowed()
					time.Sleep(1 * time.Millisecond)
				}
			}
		}()
	}

	time.Sleep(10 * time.Millisecond)
	_ = c.SetReady()
	time.Sleep(10 * time.Millisecond)
	_ = c.Drain(context.Background())
	time.Sleep(10 * time.Millisecond)
	_ = c.Stop(context.Background())

	cancel()
	wg.Wait()
}
