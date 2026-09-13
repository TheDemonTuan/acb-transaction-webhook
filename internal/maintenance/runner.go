package maintenance

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Store defines the storage methods required for singleton maintenance.
type Store interface {
	DeleteJournalBefore(ctx context.Context, cutoff time.Time) (int64, error)
	ExpireStaleAuthAttempts(ctx context.Context) (int64, error)
}

// RunnerOption configures a Runner.
type RunnerOption func(*Runner)

// WithRetentionPeriod sets the age beyond which journal entries are deleted. Default: 24h.
func WithRetentionPeriod(d time.Duration) RunnerOption {
	return func(r *Runner) {
		if d > 0 {
			r.retentionPeriod = d
		}
	}
}

// WithRetentionInterval sets how often journal retention runs. Default: 1h.
func WithRetentionInterval(d time.Duration) RunnerOption {
	return func(r *Runner) {
		if d > 0 {
			r.retentionInterval = d
		}
	}
}

// WithStaleAuthInterval sets how often stale auth attempt expiration runs. Default: 30s.
func WithStaleAuthInterval(d time.Duration) RunnerOption {
	return func(r *Runner) {
		if d > 0 {
			r.staleAuthInterval = d
		}
	}
}

// WithNowFunc overrides the clock for testing.
func WithNowFunc(fn func() time.Time) RunnerOption {
	return func(r *Runner) {
		if fn != nil {
			r.now = fn
		}
	}
}

// WithRetentionTrigger injects an external channel to trigger retention runs.
func WithRetentionTrigger(ch <-chan time.Time) RunnerOption {
	return func(r *Runner) {
		r.retentionTrigger = ch
	}
}

// WithStaleAuthTrigger injects an external channel to trigger stale-auth reap runs.
func WithStaleAuthTrigger(ch <-chan time.Time) RunnerOption {
	return func(r *Runner) {
		r.staleAuthTrigger = ch
	}
}

// WithLogger sets the structured logger for maintenance events.
func WithLogger(logger *slog.Logger) RunnerOption {
	return func(r *Runner) {
		if logger != nil {
			r.logger = logger
		}
	}
}

// Runner performs periodic maintenance tasks owned exclusively by the singleton worker.
type Runner struct {
	store             Store
	retentionPeriod   time.Duration
	retentionInterval time.Duration
	staleAuthInterval time.Duration
	now               func() time.Time
	retentionTrigger  <-chan time.Time
	staleAuthTrigger  <-chan time.Time
	logger            *slog.Logger
	mu                sync.Mutex
	running           bool
}

// NewRunner creates a new maintenance Runner.
func NewRunner(store Store, opts ...RunnerOption) *Runner {
	r := &Runner{
		store:             store,
		retentionPeriod:   24 * time.Hour,
		retentionInterval: 1 * time.Hour,
		staleAuthInterval: 30 * time.Second,
		now:               time.Now,
		logger:            slog.Default().With("component", "maintenance"),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run executes the maintenance loops until the context is canceled.
func (r *Runner) Run(ctx context.Context) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	var retentionTicker *time.Ticker
	retentionCh := r.retentionTrigger
	if retentionCh == nil {
		retentionTicker = time.NewTicker(r.retentionInterval)
		defer retentionTicker.Stop()
		retentionCh = retentionTicker.C
	}

	var staleAuthTicker *time.Ticker
	staleAuthCh := r.staleAuthTrigger
	if staleAuthCh == nil {
		staleAuthTicker = time.NewTicker(r.staleAuthInterval)
		defer staleAuthTicker.Stop()
		staleAuthCh = staleAuthTicker.C
	}

	r.logger.Info("singleton maintenance runner started",
		"retention_period", r.retentionPeriod,
		"retention_interval", r.retentionInterval,
		"stale_auth_interval", r.staleAuthInterval,
	)

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("singleton maintenance runner stopping on context done")
			return
		case t := <-retentionCh:
			r.runRetention(ctx, t)
		case <-staleAuthCh:
			r.runStaleAuthReap(ctx)
		}
	}
}

// RunOnce executes one pass of both retention and stale-auth expiration immediately.
func (r *Runner) RunOnce(ctx context.Context) error {
	var errs []error
	if err := r.runRetention(ctx, r.now()); err != nil {
		errs = append(errs, err)
	}
	if err := r.runStaleAuthReap(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r *Runner) runRetention(ctx context.Context, t time.Time) error {
	if r.store == nil {
		return nil
	}
	if t.IsZero() {
		t = r.now()
	}
	cutoff := t.Add(-r.retentionPeriod)
	deleted, err := r.store.DeleteJournalBefore(ctx, cutoff)
	if err != nil {
		r.logger.Warn("journal retention maintenance failed", "error", err, "cutoff", cutoff)
		return err
	}
	if deleted > 0 {
		r.logger.Info("journal retention purged expired entries", "deleted", deleted, "cutoff", cutoff)
	}
	return nil
}

func (r *Runner) runStaleAuthReap(ctx context.Context) error {
	if r.store == nil {
		return nil
	}
	reaped, err := r.store.ExpireStaleAuthAttempts(ctx)
	if err != nil {
		r.logger.Warn("stale auth attempts maintenance failed", "error", err)
		return err
	}
	if reaped > 0 {
		r.logger.Info("stale auth maintenance reaped expired attempts", "reaped", reaped)
	}
	return nil
}
