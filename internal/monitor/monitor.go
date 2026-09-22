package monitor

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
)

type BankClient interface {
	Bootstrap(ctx context.Context) (acb.Response, error)
	History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error)
}

type realtimeDateBankClient interface {
	BootstrapForDate(ctx context.Context, date string) (acb.Response, error)
	HistoryForDate(ctx context.Context, endpoint string, fields map[string]string, date string) (acb.Response, error)
}

var (
	ErrSyncUnavailable   = errors.New("sync unavailable")
	ErrCatchUpIncomplete = errors.New("catch-up history sync incomplete")
)

type syncRequest struct {
	connectionID string
	generation   int64
}

func generateSessionID() string {
	b := make([]byte, 16)
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)
}

type PaymentBoost struct {
	SessionID      string
	StartedAt      time.Time
	ExpiresAt      time.Time
	ExpectedAmount int64 // 0 = any amount
}

type PaymentBoostStatus struct {
	Active     bool   `json:"active"`
	SessionID  string `json:"sessionId,omitempty"`
	AmountVnd  int64  `json:"amountVnd"`
	ExpiresIn  int    `json:"expiresIn"`
	Phase      int    `json:"phase"`
	MinSeconds int    `json:"minSeconds"`
	MaxSeconds int    `json:"maxSeconds"`
}

type Monitor struct {
	store           *storage.Store
	client          BankClient
	pollMinInterval time.Duration
	pollMaxInterval time.Duration
	nextInterval    func(time.Duration, time.Duration) time.Duration
	syncMu          sync.Mutex
	syncReq         *syncRequest
	settingsCh      chan struct{}
	boostCh         chan struct{}
	cachedSettings  storage.MonitorSettings
	lastMode        storage.PollMode
	catchUpPending  bool

	boostMu sync.RWMutex
	boost   *PaymentBoost

	configMu       sync.RWMutex
	onNewEvents    func([]storage.EventNotification)
	onPollFinished func(poll storage.PollRun, insertedCount int)
	sessions       *SessionLoader
	scheduler      *scheduler.Scheduler

	backoffMu                sync.RWMutex
	backoffUntil             time.Time
	consecutiveNetworkErrors int
	now                      func() time.Time

	pollWaitersMu sync.Mutex
	pollWaiters   []chan error
}

func (m *Monitor) WithEventNotifier(fn func([]storage.EventNotification)) *Monitor {
	if m != nil {
		m.configMu.Lock()
		m.onNewEvents = fn
		m.configMu.Unlock()
	}
	return m
}

func (m *Monitor) notifyNewEvents(events []storage.EventNotification) {
	if m == nil || len(events) == 0 {
		return
	}
	m.checkAndStopBoost(events)
	m.configMu.RLock()
	fn := m.onNewEvents
	m.configMu.RUnlock()
	if fn != nil {
		fn(events)
	}
}

func (m *Monitor) WithPollNotifier(fn func(poll storage.PollRun, insertedCount int)) *Monitor {
	if m != nil {
		m.configMu.Lock()
		m.onPollFinished = fn
		m.configMu.Unlock()
	}
	return m
}

func (m *Monitor) clearSyncRequest(connectionID string, generation int64) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	if m.syncReq != nil && m.syncReq.connectionID == connectionID && m.syncReq.generation == generation {
		m.syncReq = nil
	}
}

func (m *Monitor) finishPoll(ctx context.Context, poll storage.PollRun, insertedCount int) error {
	finishCtx := ctx
	cancel := func() {}
	if ctx.Err() != nil {
		finishCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	}
	defer cancel()

	if err := m.store.FinishPoll(finishCtx, poll); err != nil {
		slog.Error("finish ACB poll", "poll_id", poll.ID, "generation", poll.Generation, "status", poll.Status, "error", err)
		return err
	}
	m.syncMu.Lock()
	if m.syncReq != nil && m.syncReq.connectionID == poll.ConnectionID && m.syncReq.generation == poll.Generation {
		m.syncReq = nil
	}
	m.syncMu.Unlock()
	m.configMu.RLock()
	fn := m.onPollFinished
	m.configMu.RUnlock()
	if fn != nil {
		fn(poll, insertedCount)
	}
	return nil
}

func New(store *storage.Store, client BankClient, minInterval, maxInterval time.Duration) *Monitor {
	if minInterval < 2*time.Second {
		minInterval = 5 * time.Second
	}
	if maxInterval < minInterval {
		maxInterval = minInterval
	}
	return &Monitor{
		store:           store,
		client:          client,
		pollMinInterval: minInterval,
		pollMaxInterval: maxInterval,
		nextInterval: func(minimum, maximum time.Duration) time.Duration {
			if maximum <= minimum {
				return minimum
			}
			return minimum + time.Duration(rand.Int64N(int64(maximum-minimum)+1))
		},
		settingsCh:     make(chan struct{}, 1),
		boostCh:        make(chan struct{}, 1),
		cachedSettings: storage.DefaultMonitorSettings,
		scheduler:      scheduler.New(&scheduler.Options{Metrics: telemetry.NewSchedulerAdapter(telemetry.Default)}),
		now:            time.Now,
	}
}

// StartPaymentBoost activates an ephemeral payment boost window.
func (m *Monitor) StartPaymentBoost(amount int64) PaymentBoostStatus {
	if m == nil {
		return PaymentBoostStatus{}
	}
	now := m.now()
	m.boostMu.Lock()
	sessionID := generateSessionID()
	if m.boost != nil && now.Before(m.boost.ExpiresAt) {
		// Enforce hard cap: active boost cannot reset or extend 180s expiration.
		m.boost.ExpectedAmount = amount
		m.boost.SessionID = sessionID
	} else {
		m.boost = &PaymentBoost{
			SessionID:      sessionID,
			StartedAt:      now,
			ExpiresAt:      now.Add(180 * time.Second),
			ExpectedAmount: amount,
		}
	}
	status := m.boostStatusLocked(now)
	m.boostMu.Unlock()

	select {
	case m.boostCh <- struct{}{}:
	default:
	}

	return status
}

// StopPaymentBoost clears any active payment boost.
// If sessionID is provided and does not match the active boost session, the stop is ignored.
// If sessionID is empty, the boost is stopped unconditionally.
func (m *Monitor) StopPaymentBoost(sessionID string) {
	if m == nil {
		return
	}
	m.boostMu.Lock()
	defer m.boostMu.Unlock()
	if m.boost == nil {
		return
	}
	if sessionID != "" && m.boost.SessionID != "" && m.boost.SessionID != sessionID {
		return
	}
	m.boost = nil
}

// PaymentBoostStatus reports the current payment boost state.
func (m *Monitor) PaymentBoostStatus() PaymentBoostStatus {
	if m == nil {
		return PaymentBoostStatus{}
	}
	m.boostMu.RLock()
	defer m.boostMu.RUnlock()
	return m.boostStatusLocked(m.now())
}

func (m *Monitor) boostStatusLocked(now time.Time) PaymentBoostStatus {
	if m.boost == nil || !now.Before(m.boost.ExpiresAt) {
		return PaymentBoostStatus{Active: false}
	}
	elapsed := now.Sub(m.boost.StartedAt)
	if elapsed >= 180*time.Second {
		return PaymentBoostStatus{Active: false}
	}
	expiresIn := int(m.boost.ExpiresAt.Sub(now).Seconds())
	if expiresIn < 0 {
		expiresIn = 0
	}

	var phase, minSec, maxSec int
	switch {
	case elapsed < 60*time.Second:
		phase = 1
		minSec = 1
		maxSec = 3
	case elapsed < 120*time.Second:
		phase = 2
		minSec = 3
		maxSec = 6
	case elapsed < 180*time.Second:
		phase = 3
		minSec = 6
		maxSec = 10
	default:
		return PaymentBoostStatus{Active: false}
	}

	return PaymentBoostStatus{
		Active:     true,
		SessionID:  m.boost.SessionID,
		AmountVnd:  m.boost.ExpectedAmount,
		ExpiresIn:  expiresIn,
		Phase:      phase,
		MinSeconds: minSec,
		MaxSeconds: maxSec,
	}
}

type creditPayloadCheck struct {
	Credit     string `json:"credit"`
	DetectedAt string `json:"detectedAt"`
}

func (m *Monitor) checkAndStopBoost(events []storage.EventNotification) {
	if m == nil || len(events) == 0 {
		return
	}
	m.boostMu.Lock()
	defer m.boostMu.Unlock()
	if m.boost == nil {
		return
	}
	now := m.now()
	if !now.Before(m.boost.ExpiresAt) {
		m.boost = nil
		return
	}

	for _, ev := range events {
		if ev.EventType != "bank.transaction.credit" {
			continue
		}
		var payload creditPayloadCheck
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			continue
		}
		creditVal, err := strconv.ParseInt(payload.Credit, 10, 64)
		if err != nil || creditVal <= 0 {
			continue
		}

		if payload.DetectedAt != "" {
			if detTime, err := time.Parse(time.RFC3339, payload.DetectedAt); err == nil {
				if detTime.Before(m.boost.StartedAt.Add(-2 * time.Second)) {
					continue
				}
			}
		}

		if m.boost.ExpectedAmount == 0 || m.boost.ExpectedAmount == creditVal {
			slog.Info("payment boost matched credit, stopping boost",
				"expectedAmount", m.boost.ExpectedAmount,
				"credit", creditVal,
				"eventId", ev.EventID)
			m.boost = nil
			return
		}
	}
}

func (m *Monitor) NotifySettingsChanged() {
	select {
	case m.settingsCh <- struct{}{}:
	default:
	}
}

func (m *Monitor) WithSessionLoader(loader *SessionLoader) *Monitor {
	if m != nil {
		m.configMu.Lock()
		m.sessions = loader
		m.configMu.Unlock()
	}
	return m
}

// SessionLoader returns the configured session loader.
func (m *Monitor) SessionLoader() *SessionLoader {
	if m == nil {
		return nil
	}
	m.configMu.RLock()
	defer m.configMu.RUnlock()
	return m.sessions
}

// PersistSession persists the freshest live ACB session snapshot to storage using an independent context.
func (m *Monitor) PersistSession(ctx context.Context) error {
	if m == nil || m.store == nil {
		return nil
	}
	sessions := m.SessionLoader()
	if sessions == nil {
		return nil
	}
	conn, err := m.store.Connection(ctx)
	if err != nil {
		return err
	}
	if conn.ID == "" || conn.Generation <= 0 {
		return nil
	}
	if conn.State == "AUTH_REQUIRED" || conn.State == "UNCONFIGURED" || conn.State == "DISCONNECTED" {
		return nil
	}
	if err := sessions.Persist(ctx, conn.ID, conn.Generation); err != nil {
		if storage.IsSessionNotRefreshable(err) {
			return nil
		}
		return err
	}
	return nil
}

func (m *Monitor) SetBackoff(duration time.Duration) {
	if duration <= 0 {
		return
	}
	m.backoffMu.Lock()
	until := m.now().Add(duration)
	if until.After(m.backoffUntil) {
		m.backoffUntil = until
	}
	m.backoffMu.Unlock()
	telemetry.Default.SetCircuitBreaker(true)
}

func (m *Monitor) RecordNetworkFailure(err error) time.Time {
	if err == nil || errors.Is(err, context.Canceled) {
		return m.BackoffUntil()
	}
	if closer, ok := m.client.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	m.backoffMu.Lock()
	m.consecutiveNetworkErrors++
	shift := min(m.consecutiveNetworkErrors-1, 4)
	duration := 5 * time.Second * time.Duration(1<<shift)
	until := m.now().Add(duration)
	if until.After(m.backoffUntil) {
		m.backoffUntil = until
	}
	result := m.backoffUntil
	m.backoffMu.Unlock()
	telemetry.Default.SetCircuitBreaker(true)
	return result
}

func (m *Monitor) ClearBackoff() {
	m.backoffMu.Lock()
	m.backoffUntil = time.Time{}
	m.consecutiveNetworkErrors = 0
	m.backoffMu.Unlock()
	telemetry.Default.SetCircuitBreaker(false)
}

func (m *Monitor) BackoffUntil() time.Time {
	m.backoffMu.RLock()
	defer m.backoffMu.RUnlock()
	return m.backoffUntil
}

func (m *Monitor) IsBackoffActive() bool {
	m.backoffMu.RLock()
	defer m.backoffMu.RUnlock()
	return m.now().Before(m.backoffUntil)
}

func (m *Monitor) registerPollWaiter() chan error {
	m.pollWaitersMu.Lock()
	defer m.pollWaitersMu.Unlock()
	ch := make(chan error, 1)
	m.pollWaiters = append(m.pollWaiters, ch)
	return ch
}

func (m *Monitor) removePollWaiter(target chan error) {
	m.pollWaitersMu.Lock()
	defer m.pollWaitersMu.Unlock()
	for i, ch := range m.pollWaiters {
		if ch == target {
			m.pollWaiters = append(m.pollWaiters[:i], m.pollWaiters[i+1:]...)
			return
		}
	}
}

func (m *Monitor) notifyPollWaiters(err error) {
	m.pollWaitersMu.Lock()
	defer m.pollWaitersMu.Unlock()
	for _, ch := range m.pollWaiters {
		select {
		case ch <- err:
		default:
		}
	}
	m.pollWaiters = nil
}

// RequestSync enqueues a bounded manual sync request if the connection is currently in MONITORING state.
func (m *Monitor) RequestSync(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	conn, err := m.store.Connection(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrSyncUnavailable
		}
		return err
	}
	if conn.State != "MONITORING" {
		return ErrSyncUnavailable
	}

	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	if m.syncReq != nil && m.syncReq.connectionID == conn.ID && m.syncReq.generation == conn.Generation {
		return nil
	}

	m.syncReq = &syncRequest{
		connectionID: conn.ID,
		generation:   conn.Generation,
	}

	task := NewRealtimeTask(m, PriorityManualSync, conn.ID, conn.Generation)
	sched := m.Scheduler()
	if sched == nil {
		m.syncReq = nil
		return ErrSyncUnavailable
	}
	if err := sched.Enqueue(task); err != nil {
		m.syncReq = nil
		return err
	}
	return nil
}

// PollOnce executes a single poll cycle if the connection is in MONITORING state.
func (m *Monitor) PollOnce(ctx context.Context) error {
	return m.pollOnce(ctx, nil)
}

func (m *Monitor) pollOnce(ctx context.Context, expected *syncRequest) error {
	conn, err := m.store.Connection(ctx)
	if err != nil {
		return err
	}
	if conn.State != "MONITORING" {
		return nil
	}

	gen := conn.Generation
	connID := conn.ID
	if expected != nil {
		gen = expected.generation
		connID = expected.connectionID
	}

	sched := m.Scheduler()
	if sched == nil || !sched.IsRunning() {
		task := NewRealtimeTask(m, PriorityRealtimePoll, connID, gen)
		for {
			res, err := task.Step(ctx)
			if err != nil {
				return err
			}
			if res.Done {
				return res.Error
			}
			if !res.RequeueAt.IsZero() {
				if res.RequeueAt.After(time.Now()) {
					return res.Error
				}
				timer := time.NewTimer(time.Until(res.RequeueAt))
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
	}

	waiter := m.registerPollWaiter()
	task := NewRealtimeTask(m, PriorityRealtimePoll, connID, gen)
	if err := sched.Enqueue(task); err != nil {
		m.removePollWaiter(waiter)
		return err
	}

	select {
	case <-ctx.Done():
		m.removePollWaiter(waiter)
		return ctx.Err()
	case err := <-waiter:
		return err
	}
}

func (m *Monitor) pollKeepalive(ctx context.Context) error {
	conn, err := m.store.Connection(ctx)
	if err != nil {
		return err
	}
	task := NewKeepaliveTask(m, conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err != nil {
		return err
	}
	return res.Error
}

func (m *Monitor) catchUp(ctx context.Context) error {
	conn, err := m.store.Connection(ctx)
	if err != nil {
		return err
	}
	task := NewCatchUpTask(m, conn.ID, conn.Generation)
	for {
		if err := ctx.Err(); err != nil {
			m.catchUpPending = true
			return err
		}
		res, err := task.Step(ctx)
		if err != nil {
			m.catchUpPending = true
			return err
		}
		if res.Done {
			m.catchUpPending = false
			return nil
		}
		if !res.RequeueAt.IsZero() && res.RequeueAt.After(time.Now()) {
			m.catchUpPending = true
			if res.Error != nil {
				return res.Error
			}
			return ErrCatchUpIncomplete
		}
	}
}

// ScheduleRecovery durably admits one recovery intent, then enqueues its task.
func (m *Monitor) recoveryPlan(ctx context.Context, connectionID string, generation int64, reason string) (storage.RecoveryRunPlan, error) {
	nowLocal := m.now().In(acb.DefaultLocation)
	today := nowLocal.Format("2006-01-02")
	from := nowLocal.AddDate(0, 0, -1)
	cp, err := m.store.GetCheckpoint(ctx, connectionID)
	if err != nil {
		return storage.RecoveryRunPlan{}, fmt.Errorf("load recovery checkpoint: %w", err)
	}
	if cp != nil && cp.CoverageTo != "" {
		checkpointTo, parseErr := time.ParseInLocation("2006-01-02", cp.CoverageTo, acb.DefaultLocation)
		if parseErr != nil || checkpointTo.After(nowLocal) {
			slog.Warn("invalid or future recovery checkpoint; using bounded fallback", "connection_id", connectionID, "generation", generation, "coverage_to", cp.CoverageTo)
		} else {
			from = checkpointTo
		}
	}
	oldest := nowLocal.AddDate(0, 0, -(catchUpMaxDays - 1))
	if from.Before(oldest) {
		from = oldest
	}
	if from.After(nowLocal) {
		from = nowLocal
	}
	fromDate := from.Format("2006-01-02")
	return storage.RecoveryRunPlan{Reason: reason, RangeFrom: fromDate, RangeTo: today, NextDay: fromDate}, nil
}

func (m *Monitor) ScheduleRecovery(ctx context.Context, connectionID string, generation int64, eventKey string) error {
	if m == nil || m.store == nil {
		return errors.New("recovery monitor is not initialized")
	}
	if connectionID == "" || generation <= 0 || eventKey == "" {
		return errors.New("invalid recovery parameters")
	}
	conn, err := m.store.Connection(ctx)
	if err != nil {
		return err
	}
	if conn.ID != connectionID || conn.Generation != generation || conn.State != "MONITORING" {
		return fmt.Errorf("stale recovery request: connection is %s at generation %d in state %s", conn.ID, conn.Generation, conn.State)
	}
	plan, err := m.recoveryPlan(ctx, connectionID, generation, "SESSION_AUTHENTICATED")
	if err != nil {
		return err
	}
	run, _, err := m.store.EnsureRecoveryRunWithPlan(ctx, connectionID, generation, eventKey, plan)
	if err != nil {
		return err
	}
	if run.Status == storage.RecoveryRunStatusCompleted || run.Status == storage.RecoveryRunStatusFailed || run.Status == storage.RecoveryRunStatusCanceled {
		return nil
	}
	sched := m.Scheduler()
	if sched == nil || !sched.IsRunning() {
		return errors.New("recovery scheduler is not running")
	}
	return sched.Enqueue(NewRecoveryCatchUpTask(m, connectionID, generation, "SESSION_AUTHENTICATED", run.ID))
}

func (m *Monitor) reconcileRecovery(ctx context.Context) {
	if m == nil || m.store == nil {
		return
	}
	conn, err := m.store.Connection(ctx)
	if err != nil || conn.State != "MONITORING" || conn.Generation <= 0 {
		return
	}
	runs, err := m.store.ListOpenRecoveryRuns(ctx, conn.ID, conn.Generation)
	if err != nil {
		slog.Warn("recovery reconciliation failed", "connection_id", conn.ID, "error", err)
		return
	}
	if len(runs) == 0 {
		today := m.now().In(acb.DefaultLocation).Format("2006-01-02")
		from := m.now().In(acb.DefaultLocation).AddDate(0, 0, -(catchUpMaxDays - 1)).Format("2006-01-02")
		cp, cpErr := m.store.GetCheckpoint(ctx, conn.ID)
		if cpErr != nil {
			slog.Warn("recovery reconciliation checkpoint lookup failed", "connection_id", conn.ID, "generation", conn.Generation, "error", cpErr)
			return
		}
		if cp != nil && cp.CoverageTo != "" {
			from = cp.CoverageTo
		}
		covered, coverageErr := m.store.CheckRangeCoverage(ctx, conn.ID, from, today)
		if coverageErr != nil {
			slog.Warn("recovery reconciliation coverage lookup failed", "connection_id", conn.ID, "generation", conn.Generation, "from", from, "to", today, "error", coverageErr)
			return
		}
		if covered {
			return
		}

		plan, planErr := m.recoveryPlan(ctx, conn.ID, conn.Generation, "WORKER_STARTUP")
		if planErr != nil {
			return
		}
		run, _, err := m.store.EnsureRecoveryRunWithPlan(ctx, conn.ID, conn.Generation, "startup:"+today, plan)

		if err != nil {
			return
		}
		runs = []storage.RecoveryRun{run}
	}
	sched := m.Scheduler()
	if sched == nil || !sched.IsRunning() {
		return
	}
	for _, run := range runs {
		if run.Status == storage.RecoveryRunStatusPending || run.Status == storage.RecoveryRunStatusRunning {
			if err := sched.Enqueue(NewRecoveryCatchUpTask(m, conn.ID, conn.Generation, "WORKER_STARTUP", run.ID)); err != nil {
				slog.Warn("recovery reconciliation enqueue failed", "connection_id", conn.ID, "run_id", run.ID, "error", err)
			}
		}
	}
}

func (m *Monitor) Run(ctx context.Context) {
	sched := m.Scheduler()
	if sched != nil {
		sched.Start(ctx)
		defer sched.Stop()
	}

	// Initial load of settings from database
	if initSettings, err := m.store.GetMonitorSettings(ctx); err == nil {
		m.cachedSettings = initSettings
	}
	m.reconcileRecovery(ctx)

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	recoveryTicker := time.NewTicker(5 * time.Second)
	defer recoveryTicker.Stop()

	for {
		schedule := storage.ResolveSchedule(time.Now(), &m.cachedSettings)

		// Schedule resume is realtime-only. Historical recovery is admitted separately
		// by an authenticated recovery intent, never by the clock mode transition.
		if (m.lastMode == storage.ModeKeepaliveOnly || m.lastMode == storage.ModePaused || m.lastMode == "") && schedule.Mode == storage.ModeRealtime {
			conn, err := m.store.Connection(ctx)
			if err == nil && conn.State == "MONITORING" {
				if s := m.Scheduler(); s != nil {
					_ = s.Enqueue(NewRealtimeTask(m, PriorityRealtimePoll, conn.ID, conn.Generation))
				}
			}
		}
		m.lastMode = schedule.Mode

		wait := m.nextInterval(schedule.MinInterval, schedule.MaxInterval)
		if schedule.Mode == storage.ModePaused {
			wait = 10 * time.Minute
		}

		boostStatus := m.PaymentBoostStatus()
		if boostStatus.Active && schedule.Mode != storage.ModePaused {
			wait = m.nextInterval(time.Duration(boostStatus.MinSeconds)*time.Second, time.Duration(boostStatus.MaxSeconds)*time.Second)
		} else {
			untilTrans := time.Until(schedule.NextTransition)
			if untilTrans > 0 && untilTrans < wait {
				wait = untilTrans
			}
		}
		timer.Reset(wait)

		select {
		case <-ctx.Done():
			return

		case <-recoveryTicker.C:
			m.reconcileRecovery(ctx)

		case <-m.boostCh:
			m.reconcileRecovery(ctx)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			conn, err := m.store.Connection(ctx)
			if err == nil && conn.State == "MONITORING" {
				if s := m.Scheduler(); s != nil {
					_ = s.Enqueue(NewRealtimeTask(m, PriorityRealtimePoll, conn.ID, conn.Generation))
				}
			}
			continue

		case <-m.settingsCh:
			m.reconcileRecovery(ctx)
			// Hot reload settings
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if s, err := m.store.GetMonitorSettings(ctx); err == nil {
				m.cachedSettings = s
				slog.Info("monitor schedule hot-reloaded", "revision", s.Revision, "enabled", s.Enabled)
			}
			continue

		case <-timer.C:
			m.reconcileRecovery(ctx)
			schedule = storage.ResolveSchedule(time.Now(), &m.cachedSettings)
			conn, err := m.store.Connection(ctx)
			if err != nil || conn.State != "MONITORING" {
				continue
			}

			if s := m.Scheduler(); s != nil {
				curBoost := m.PaymentBoostStatus()
				if curBoost.Active && schedule.Mode != storage.ModePaused {
					_ = s.Enqueue(NewRealtimeTask(m, PriorityRealtimePoll, conn.ID, conn.Generation))
				} else {
					switch schedule.Mode {
					case storage.ModeRealtime:
						_ = s.Enqueue(NewRealtimeTask(m, PriorityRealtimePoll, conn.ID, conn.Generation))
					case storage.ModeKeepaliveOnly:
						_ = s.Enqueue(NewKeepaliveTask(m, conn.ID, conn.Generation))
					case storage.ModePaused:
						// No upstream request
					}
				}
			}
		}
	}
}
