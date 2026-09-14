package notification

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
)

var defaultBackoffs = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
	1 * time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
}

type Dispatcher struct {
	store      *storage.Store
	registry   *Registry
	maxRetries int
	backoffs   []time.Duration
	workers    int
	wakeCh     chan struct{}
	paused     atomic.Bool
}

func NewDispatcher(store *storage.Store, registry *Registry) *Dispatcher {
	return &Dispatcher{
		store:      store,
		registry:   registry,
		maxRetries: len(defaultBackoffs),
		backoffs:   defaultBackoffs,
		workers:    4,
		wakeCh:     make(chan struct{}, 4),
	}
}

func (d *Dispatcher) SetMaxRetries(n int) *Dispatcher {
	if n > 0 {
		d.maxRetries = n
	}
	return d
}

func (d *Dispatcher) SetBackoffs(b []time.Duration) *Dispatcher {
	if len(b) > 0 {
		d.backoffs = b
		d.maxRetries = len(b)
	}
	return d
}

func (d *Dispatcher) SetWorkers(n int) *Dispatcher {
	if n > 0 {
		d.workers = n
	}
	return d
}

func (d *Dispatcher) Pause() {
	d.paused.Store(true)
}

func (d *Dispatcher) Resume() {
	d.paused.Store(false)
	d.Wake()
}

func (d *Dispatcher) IsPaused() bool {
	return d.paused.Load()
}

func (d *Dispatcher) Wake() {
	for range d.workers {
		select {
		case d.wakeCh <- struct{}{}:
		default:
			return
		}
	}
}

func (d *Dispatcher) Start(ctx context.Context) {
	slog.Info("notification dispatcher started", "max_retries", d.maxRetries, "workers", d.workers)
	var wg sync.WaitGroup
	for range d.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.worker(ctx)
		}()
	}
	<-ctx.Done()
	wg.Wait()
	slog.Info("notification dispatcher stopping")
}

func (d *Dispatcher) worker(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.wakeCh:
			d.drain(ctx)
		case <-ticker.C:
			d.drain(ctx)
		}
	}
}

func (d *Dispatcher) drain(ctx context.Context) {
	if d.paused.Load() {
		return
	}
	for {
		processed, err := d.DispatchOne(ctx)
		if err != nil {
			slog.Warn("notification dispatch error", "error", err)
			return
		}
		if !processed {
			return
		}
	}
}

// DispatchOne claims and attempts to dispatch a single pending delivery.
// Returns (true, nil) if a delivery was processed, (false, nil) if none available.
func (d *Dispatcher) DispatchOne(ctx context.Context) (bool, error) {
	now := time.Now().UTC()
	delivery, err := d.store.ClaimDelivery(ctx, now, 30*time.Second)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	target, err := d.store.DeliveryTargetForDelivery(ctx, delivery)
	if err != nil {
		_ = d.store.FailDelivery(ctx, delivery.ID, delivery.ClaimToken, "ENDPOINT_UNAVAILABLE")
		return true, fmt.Errorf("fetch delivery target: %w", err)
	}

	sender, ok := d.registry.Get(target.Provider)
	if !ok {
		_ = d.store.FailDelivery(ctx, delivery.ID, delivery.ClaimToken, "PROVIDER_NOT_REGISTERED")
		return true, fmt.Errorf("provider %s not registered", target.Provider)
	}

	payload, err := d.store.EventPayload(ctx, delivery.EventID)
	if err != nil {
		_ = d.store.FailDelivery(ctx, delivery.ID, delivery.ClaimToken, "EVENT_NOT_FOUND")
		return true, fmt.Errorf("fetch event payload: %w", err)
	}

	req := SendRequest{
		DeliveryID:    delivery.ID,
		EventID:       delivery.EventID,
		EventType:     "bank.transaction.credit",
		Target:        target,
		EventPayload:  payload,
		AttemptNumber: delivery.Attempts,
	}

	start := time.Now()
	res := sender.Send(ctx, req)
	duration := time.Since(start)

	telemetry.Default.RecordNotification(target.Provider, duration, res.Outcome == OutcomeSuccess)
	if target.Provider == ProviderWebhook {
		telemetry.Default.RecordWebhook(duration, res.Outcome == OutcomeSuccess)
	}

	// Calculate effective attempts in current retry cycle
	effectiveAttempts := delivery.Attempts - delivery.RetryCycleStartAttempt
	if effectiveAttempts < 1 {
		effectiveAttempts = 1
	}

	finalOutcome := res.Outcome
	if res.Outcome == OutcomeRetry && effectiveAttempts >= d.maxRetries {
		finalOutcome = OutcomeTerminalFailure
	}

	recordErr := d.store.RecordAttempt(
		ctx,
		delivery.ID,
		delivery.Attempts,
		res.StatusCode,
		res.LatencyMs,
		string(finalOutcome),
		res.SanitizedError,
		target.Provider,
		res.ProviderErrorCode,
	)
	if recordErr != nil {
		slog.Error("failed to record delivery attempt", "delivery_id", delivery.ID, "error", recordErr)
	}

	if res.Outcome == OutcomeSuccess {
		err = d.store.CompleteDelivery(ctx, delivery, true, time.Time{})
		return true, err
	}

	if res.Outcome == OutcomeTerminalFailure || effectiveAttempts >= d.maxRetries {
		reason := res.ProviderErrorCode
		if reason == "" {
			reason = fmt.Sprintf("EXHAUSTED_RETRIES_STATUS_%d", res.StatusCode)
		}
		err = d.store.FailDelivery(ctx, delivery.ID, delivery.ClaimToken, reason)
		return true, err
	}

	// OutcomeRetry
	backoffIdx := effectiveAttempts - 1
	if backoffIdx < 0 {
		backoffIdx = 0
	}
	if backoffIdx >= len(d.backoffs) {
		backoffIdx = len(d.backoffs) - 1
	}
	nextAttempt := now.Add(d.backoffs[backoffIdx])
	err = d.store.CompleteDelivery(ctx, delivery, false, nextAttempt)
	return true, err
}
