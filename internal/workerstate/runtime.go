package workerstate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// State represents the lifecycle phase of the worker singleton.
type State string

const (
	StateStarting State = "STARTING"
	StateReady    State = "READY"
	StateDraining State = "DRAINING"
	StateStopping State = "STOPPING"
)

var (
	ErrWorkerDraining = errors.New("worker is draining")
	ErrWorkerStopping = errors.New("worker is stopping")
	ErrInvalidState   = errors.New("invalid state transition")
)

// CoordinatorOption configures the worker runtime coordinator.
type CoordinatorOption func(*Coordinator)

// WithDrainTimeout sets the maximum duration allocated for draining current quanta.
func WithDrainTimeout(d time.Duration) CoordinatorOption {
	return func(c *Coordinator) {
		if d > 0 {
			c.drainTimeout = d
		}
	}
}

// WithShutdownTimeout sets the maximum duration for shutdown tasks (persistence, requeue, unlock).
func WithShutdownTimeout(d time.Duration) CoordinatorOption {
	return func(c *Coordinator) {
		if d > 0 {
			c.shutdownTimeout = d
		}
	}
}

// WithLogger configures a structured logger.
func WithLogger(logger *slog.Logger) CoordinatorOption {
	return func(c *Coordinator) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// Coordinator manages the singleton worker lifecycle transitions:
// STARTING -> READY -> DRAINING -> STOPPING.
type Coordinator struct {
	mu              sync.RWMutex
	state           State
	drainTimeout    time.Duration
	shutdownTimeout time.Duration
	drainHooks      []func(ctx context.Context) error
	stopHooks       []func(ctx context.Context) error
	logger          *slog.Logger
	drainDone       chan struct{}
	drainOnce       sync.Once
	stopDone        chan struct{}
	stopOnce        sync.Once
}

// NewCoordinator creates a runtime Coordinator starting in StateStarting.
func NewCoordinator(opts ...CoordinatorOption) *Coordinator {
	c := &Coordinator{
		state:           StateStarting,
		drainTimeout:    10 * time.Second,
		shutdownTimeout: 10 * time.Second,
		logger:          slog.Default().With("component", "workerstate"),
		drainDone:       make(chan struct{}),
		stopDone:        make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// State returns the current lifecycle state.
func (c *Coordinator) State() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// IsReady reports whether the worker is in StateReady.
func (c *Coordinator) IsReady() bool {
	return c.State() == StateReady
}

// IsDraining reports whether the worker is in StateDraining or StateStopping.
func (c *Coordinator) IsDraining() bool {
	s := c.State()
	return s == StateDraining || s == StateStopping
}

// DrainDone returns a channel that is closed after Drain() completes.
func (c *Coordinator) DrainDone() <-chan struct{} {
	return c.drainDone
}

// SetReady transitions the worker from STARTING to READY.
func (c *Coordinator) SetReady() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != StateStarting {
		return fmt.Errorf("%w: cannot transition from %s to READY", ErrInvalidState, c.state)
	}
	c.state = StateReady
	c.logger.Info("worker transitioned to READY")
	return nil
}

// RegisterDrainHook registers a callback executed when Drain() is invoked.
func (c *Coordinator) RegisterDrainHook(fn func(ctx context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drainHooks = append(c.drainHooks, fn)
}

// RegisterStopHook registers a callback executed when Stop() is invoked.
func (c *Coordinator) RegisterStopHook(fn func(ctx context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopHooks = append(c.stopHooks, fn)
}

// Drain transitions the worker to StateDraining and executes registered drain hooks.
func (c *Coordinator) Drain(ctx context.Context) error {
	c.mu.Lock()
	if c.state == StateDraining {
		c.mu.Unlock()
		<-c.drainDone
		return nil
	}
	if c.state == StateStopping {
		c.mu.Unlock()
		return ErrWorkerStopping
	}
	c.state = StateDraining
	c.logger.Info("worker transitioned to DRAINING")
	hooks := append([]func(ctx context.Context) error(nil), c.drainHooks...)
	c.mu.Unlock()

	defer c.drainOnce.Do(func() { close(c.drainDone) })

	drainCtx := ctx
	var cancel context.CancelFunc
	if _, ok := drainCtx.Deadline(); !ok && c.drainTimeout > 0 {
		drainCtx, cancel = context.WithTimeout(ctx, c.drainTimeout)
		defer cancel()
	}

	for _, hook := range hooks {
		if err := hook(drainCtx); err != nil {
			c.logger.Warn("drain hook error", "error", err)
		}
	}
	return nil
}

// Stop transitions the worker to StateStopping and executes registered stop hooks.
func (c *Coordinator) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.state == StateStopping {
		c.mu.Unlock()
		<-c.stopDone
		return nil
	}
	c.state = StateStopping
	c.logger.Info("worker transitioned to STOPPING")
	hooks := append([]func(ctx context.Context) error(nil), c.stopHooks...)
	c.mu.Unlock()

	defer c.stopOnce.Do(func() { close(c.stopDone) })

	stopCtx := ctx
	var cancel context.CancelFunc
	if _, ok := stopCtx.Deadline(); !ok && c.shutdownTimeout > 0 {
		stopCtx, cancel = context.WithTimeout(ctx, c.shutdownTimeout)
		defer cancel()
	}

	for _, hook := range hooks {
		if err := hook(stopCtx); err != nil {
			c.logger.Warn("stop hook error", "error", err)
		}
	}
	return nil
}

// CheckWorkAllowed verifies that new upstream work is permissible.
// Returns nil only when State is StateReady.
func (c *Coordinator) CheckWorkAllowed() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch c.state {
	case StateReady:
		return nil
	case StateDraining:
		return ErrWorkerDraining
	case StateStopping:
		return ErrWorkerStopping
	default:
		return fmt.Errorf("worker is in %s state", c.state)
	}
}
