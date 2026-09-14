package monitor

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
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

var (
	ErrSyncUnavailable   = errors.New("sync unavailable")
	ErrCatchUpIncomplete = errors.New("catch-up history sync incomplete")
)

type syncRequest struct {
	connectionID string
	generation   int64
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
	cachedSettings  storage.MonitorSettings
	lastMode        storage.PollMode
	catchUpPending  bool

	configMu       sync.RWMutex
	onNewEvents    func([]storage.EventNotification)
	onPollFinished func(poll storage.PollRun, insertedCount int)
	sessions       *SessionLoader
	scheduler      *scheduler.Scheduler

	backoffMu    sync.RWMutex
	backoffUntil time.Time

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

func (m *Monitor) finishPoll(ctx context.Context, poll storage.PollRun, insertedCount int) error {
	err := m.store.FinishPoll(ctx, poll)
	m.configMu.RLock()
	fn := m.onPollFinished
	m.configMu.RUnlock()
	if fn != nil {
		fn(poll, insertedCount)
	}
	return err
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
		cachedSettings: storage.DefaultMonitorSettings,
		scheduler:      scheduler.New(&scheduler.Options{Metrics: telemetry.NewSchedulerAdapter(telemetry.Default)}),
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
	return sessions.Persist(ctx, conn.ID, conn.Generation)
}

func (m *Monitor) SetBackoff(duration time.Duration) {
	m.backoffMu.Lock()
	m.backoffUntil = time.Now().Add(duration)
	m.backoffMu.Unlock()
	telemetry.Default.SetCircuitBreaker(true)
}

func (m *Monitor) ClearBackoff() {
	m.backoffMu.Lock()
	m.backoffUntil = time.Time{}
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
	return time.Now().Before(m.backoffUntil)
}

func (m *Monitor) registerPollWaiter() chan error {
	m.pollWaitersMu.Lock()
	defer m.pollWaitersMu.Unlock()
	ch := make(chan error, 1)
	m.pollWaiters = append(m.pollWaiters, ch)
	return ch
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
		return ErrSyncUnavailable
	}
	return sched.Enqueue(task)
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
		res, err := task.Step(ctx)
		if err != nil {
			return err
		}
		return res.Error
	}

	waiter := m.registerPollWaiter()
	task := NewRealtimeTask(m, PriorityRealtimePoll, connID, gen)
	if err := sched.Enqueue(task); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
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

func (m *Monitor) ScheduleCatchUp() {
	ctx := context.Background()
	conn, err := m.store.Connection(ctx)
	if err != nil || conn.State != "MONITORING" {
		return
	}
	task := NewCatchUpTask(m, conn.ID, conn.Generation)
	sched := m.Scheduler()
	if sched != nil && sched.IsRunning() {
		_ = sched.Enqueue(task)
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

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()

	for {
		schedule := storage.ResolveSchedule(time.Now(), &m.cachedSettings)

		// Transition check: transitioning into REALTIME triggers catch-up and immediate realtime poll
		if (m.lastMode == storage.ModeKeepaliveOnly || m.lastMode == storage.ModePaused || m.lastMode == "") && schedule.Mode == storage.ModeRealtime {
			conn, err := m.store.Connection(ctx)
			if err == nil && conn.State == "MONITORING" {
				if s := m.Scheduler(); s != nil {
					_ = s.Enqueue(NewCatchUpTask(m, conn.ID, conn.Generation))
					_ = s.Enqueue(NewRealtimeTask(m, PriorityRealtimePoll, conn.ID, conn.Generation))
				}
			}
		}
		m.lastMode = schedule.Mode

		wait := m.nextInterval(schedule.MinInterval, schedule.MaxInterval)
		if schedule.Mode == storage.ModePaused {
			wait = 10 * time.Minute
		}
		untilTrans := time.Until(schedule.NextTransition)
		if untilTrans > 0 && untilTrans < wait {
			wait = untilTrans
		}
		timer.Reset(wait)

		select {
		case <-ctx.Done():
			return

		case <-m.settingsCh:
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
			conn, err := m.store.Connection(ctx)
			if err != nil || conn.State != "MONITORING" {
				continue
			}

			if s := m.Scheduler(); s != nil {
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
