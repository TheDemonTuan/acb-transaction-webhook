package monitor

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func setupTestStoreAndConnection(t *testing.T, ctx context.Context, dbName string) (*storage.Store, storage.Connection) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), dbName)
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}

	// Pre-populate 7-day coverage so startup recovery is satisfied and does not block polling
	nowLocal := time.Now().In(acb.DefaultLocation)
	today := nowLocal.Format("2006-01-02")
	if err := store.SaveCheckpoint(ctx, storage.Checkpoint{ConnectionID: conn.ID, CoverageTo: today, UpdatedAt: today}); err != nil {
		t.Fatal(err)
	}
	coverageDays := make(map[string]int)
	for i := 0; i < 7; i++ {
		d := nowLocal.AddDate(0, 0, -i).Format("2006-01-02")
		coverageDays[d] = 0
	}
	if err := store.RecordCoveragePerDay(ctx, conn.ID, coverageDays); err != nil {
		t.Fatal(err)
	}

	return store, conn
}

func makeRealtimeSettings(interval time.Duration) storage.MonitorSettings {
	sec := int(interval.Seconds())
	if sec < 3 {
		sec = 3
	}
	return storage.MonitorSettings{
		Revision: 1,
		Enabled:  true,
		Timezone: "Asia/Ho_Chi_Minh",
		DefaultProfile: storage.Profile{
			Mode:       storage.ModeRealtime,
			MinSeconds: sec,
			MaxSeconds: sec,
		},
		Windows: []storage.Window{
			{
				Name:       "AllDayRealtime",
				DaysOfWeek: []int{0, 1, 2, 3, 4, 5, 6},
				StartTime:  "00:00",
				EndTime:    "23:59",
				Profile: storage.Profile{
					Mode:       storage.ModeRealtime,
					MinSeconds: sec,
					MaxSeconds: sec,
				},
			},
		},
	}
}

// TestMonitorRun_MultiplePollingIntervalsGreaterThanRecoveryTicker_Synctest proves that
// polling intervals larger than the 5s recoveryTicker tick repeatedly without being starved.
func TestMonitorRun_MultiplePollingIntervalsGreaterThanRecoveryTicker_Synctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		store, _ := setupTestStoreAndConnection(t, ctx, "test_multi_intervals.db")
		defer store.Close()

		var mu sync.Mutex
		var pollTimestamps []time.Time

		mock := &countingMockClient{
			getFunc: func(ctx context.Context, endpoint string) (acb.Response, error) {
				mu.Lock()
				pollTimestamps = append(pollTimestamps, time.Now())
				mu.Unlock()
				return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHistoryHTML}, nil
			},
		}

		pollInterval := 15 * time.Second
		m := New(store, mock, pollInterval, pollInterval)
		m.nextInterval = func(min, max time.Duration) time.Duration {
			return pollInterval
		}

		settings := makeRealtimeSettings(pollInterval)
		if _, err := store.SaveMonitorSettings(ctx, settings); err != nil {
			t.Fatal(err)
		}
		m.cachedSettings = settings

		go m.Run(ctx)

		// Wait briefly for startup poll to execute
		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		initialCount := len(pollTimestamps)
		mu.Unlock()
		if initialCount != 1 {
			t.Fatalf("expected 1 startup poll, got %d", initialCount)
		}

		// Advance time by 48 seconds:
		// With 15s polling intervals: polls should fire at +15s, +30s, +45s (3 additional polls).
		// During this time, recoveryTicker (5s) fires ~9 times!
		// Pre-fix: recoveryTicker reset timer every 5s, starving all polls > 5s (0 additional polls).
		// Post-fix: deadline is preserved across recovery ticks.
		time.Sleep(48 * time.Second)

		mu.Lock()
		totalPolls := len(pollTimestamps)
		timestampsCopy := append([]time.Time(nil), pollTimestamps...)
		mu.Unlock()

		if totalPolls < 4 {
			t.Fatalf("expected at least 4 total polls (1 initial + 3 periodic), got %d (starvation detected)", totalPolls)
		}

		// Verify interval between periodic polls is ~15s, not delayed by recovery ticks
		for i := 2; i < len(timestampsCopy); i++ {
			gap := timestampsCopy[i].Sub(timestampsCopy[i-1])
			if gap < 14*time.Second || gap > 16*time.Second {
				t.Errorf("poll %d to %d gap %v outside expected 15s interval", i-1, i, gap)
			}
		}
	})
}

// TestMonitorRun_DeadlinePreservedAcrossRecoveryTicks_Synctest proves that
// recovery ticks occurring while waiting for a poll deadline do not push out the deadline.
func TestMonitorRun_DeadlinePreservedAcrossRecoveryTicks_Synctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		store, _ := setupTestStoreAndConnection(t, ctx, "test_deadline_preservation.db")
		defer store.Close()

		var pollCount atomic.Int32
		mock := &countingMockClient{
			getFunc: func(ctx context.Context, endpoint string) (acb.Response, error) {
				pollCount.Add(1)
				return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHistoryHTML}, nil
			},
		}

		pollInterval := 14 * time.Second
		m := New(store, mock, pollInterval, pollInterval)
		m.nextInterval = func(min, max time.Duration) time.Duration {
			return pollInterval
		}

		settings := makeRealtimeSettings(pollInterval)
		if _, err := store.SaveMonitorSettings(ctx, settings); err != nil {
			t.Fatal(err)
		}
		m.cachedSettings = settings

		go m.Run(ctx)
		time.Sleep(50 * time.Millisecond)

		if count := pollCount.Load(); count != 1 {
			t.Fatalf("expected 1 startup poll, got %d", count)
		}

		// Sleep 4s: no recovery tick yet (ticker is 5s), no periodic poll yet (14s)
		time.Sleep(4 * time.Second)
		if count := pollCount.Load(); count != 1 {
			t.Fatalf("poll fired prematurely at 4s: count=%d", count)
		}

		// Sleep 2s (total 6s): first recovery tick fired at 5s!
		// Periodic poll should NOT have fired yet.
		time.Sleep(2 * time.Second)
		if count := pollCount.Load(); count != 1 {
			t.Fatalf("unexpected poll at 6s: count=%d", count)
		}

		// Sleep 5s (total 11s): second recovery tick fired at 10s!
		// Periodic poll still should NOT have fired yet.
		time.Sleep(5 * time.Second)
		if count := pollCount.Load(); count != 1 {
			t.Fatalf("unexpected poll at 11s: count=%d", count)
		}

		// Sleep 3s (total 14s): deadline reached!
		// The periodic poll must fire now, proving the 5s and 10s recovery ticks did not push deadline out to 10+14=24s.
		time.Sleep(3 * time.Second)
		if count := pollCount.Load(); count != 2 {
			t.Fatalf("expected periodic poll to fire at 14s deadline, got count=%d", count)
		}
	})
}

// TestMonitorRun_PaymentBoostSemantics_Synctest proves that activating payment boost
// interrupts long intervals, triggers an immediate poll, and polls at the accelerated boost rate.
func TestMonitorRun_PaymentBoostSemantics_Synctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		store, _ := setupTestStoreAndConnection(t, ctx, "test_boost.db")
		defer store.Close()

		var pollCount atomic.Int32
		mock := &countingMockClient{
			getFunc: func(ctx context.Context, endpoint string) (acb.Response, error) {
				pollCount.Add(1)
				return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHistoryHTML}, nil
			},
		}

		pollInterval := 30 * time.Second
		m := New(store, mock, pollInterval, pollInterval)
		m.nextInterval = func(min, max time.Duration) time.Duration {
			// For boost, return min duration; for normal, return pollInterval
			if min < pollInterval {
				return min
			}
			return pollInterval
		}

		settings := makeRealtimeSettings(pollInterval)
		if _, err := store.SaveMonitorSettings(ctx, settings); err != nil {
			t.Fatal(err)
		}
		m.cachedSettings = settings

		go m.Run(ctx)
		time.Sleep(50 * time.Millisecond)

		if pollCount.Load() != 1 {
			t.Fatalf("expected 1 startup poll, got %d", pollCount.Load())
		}

		// Advance 5s (normal 30s timer is running). Poll count remains 1.
		time.Sleep(5 * time.Second)
		if pollCount.Load() != 1 {
			t.Fatalf("unexpected poll at 5s: got %d", pollCount.Load())
		}

		// Activate payment boost -> must immediately trigger boostCh and enqueue realtime poll
		boostStatus := m.StartPaymentBoost(100_000)
		if !boostStatus.Active {
			t.Fatal("expected boost to be active")
		}

		// Wait briefly for boostCh immediate poll
		time.Sleep(50 * time.Millisecond)
		if pollCount.Load() < 2 {
			t.Fatalf("expected immediate poll on boost activation, got %d", pollCount.Load())
		}

		// Boost phase 1 uses 1s interval (since nextInterval returns min=1s).
		// Advance 5 seconds during boost: should trigger ~5 polls.
		beforeBoost := pollCount.Load()
		time.Sleep(5 * time.Second)
		afterBoost := pollCount.Load()
		if boostPolls := afterBoost - beforeBoost; boostPolls < 4 {
			t.Fatalf("expected at least 4 accelerated polls during 5s boost, got %d", boostPolls)
		}

		// Stop payment boost
		m.StopPaymentBoost("")
		if m.PaymentBoostStatus().Active {
			t.Fatal("expected boost to be inactive after stop")
		}

		// After boost stopped, next poll interval is reverted to 30s.
		// Sleeping 10s should yield at most 1 poll (from the trailing timer reset).
		baseline := pollCount.Load()
		time.Sleep(10 * time.Second)
		subsequent := pollCount.Load() - baseline
		if subsequent > 1 {
			t.Fatalf("expected normal pacing after boost stopped, got %d polls in 10s", subsequent)
		}
	})
}

// TestMonitorRun_HotReloadSettingsAndModes_Synctest proves that mode transitions
// (Realtime -> Paused -> KeepaliveOnly -> Realtime) correctly adapt timers and enqueue tasks.
func TestMonitorRun_HotReloadSettingsAndModes_Synctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		store, _ := setupTestStoreAndConnection(t, ctx, "test_settings_modes.db")
		defer store.Close()

		var mu sync.Mutex
		var realtimePolls int
		var keepalivePolls int

		mock := &countingMockClient{
			getFunc: func(ctx context.Context, endpoint string) (acb.Response, error) {
				return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHistoryHTML}, nil
			},
		}

		m := New(store, mock, 10*time.Second, 10*time.Second)
		m.WithPollNotifier(func(poll storage.PollRun, insertedCount int) {
			mu.Lock()
			defer mu.Unlock()
			if poll.Pages > 0 {
				realtimePolls++
			} else {
				keepalivePolls++
			}
		})
		m.nextInterval = func(min, max time.Duration) time.Duration {
			return min
		}

		// Start with Realtime (10s)
		initSettings := makeRealtimeSettings(10 * time.Second)
		saved, err := store.SaveMonitorSettings(ctx, initSettings)
		if err != nil {
			t.Fatal(err)
		}
		m.cachedSettings = saved

		go m.Run(ctx)
		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		initRT := realtimePolls
		initKA := keepalivePolls
		mu.Unlock()

		if initRT != 1 {
			t.Fatalf("expected 1 startup realtime poll, got %d", initRT)
		}
		if initKA != 0 {
			t.Fatalf("expected 0 keepalive polls on startup, got %d", initKA)
		}

		// 1. Transition to PAUSED mode
		pausedSettings := saved
		pausedSettings.DefaultProfile.Mode = storage.ModePaused
		pausedSettings.Windows = nil
		savedPaused, err := store.SaveMonitorSettings(ctx, pausedSettings)
		if err != nil {
			t.Fatal(err)
		}

		m.NotifySettingsChanged()
		time.Sleep(50 * time.Millisecond)

		// In PAUSED mode, wait 30s. Neither realtime nor keepalive should fire.
		time.Sleep(30 * time.Second)
		mu.Lock()
		pausedRT := realtimePolls
		pausedKA := keepalivePolls
		mu.Unlock()

		if pausedRT != initRT || pausedKA != initKA {
			t.Fatalf("expected no polling during PAUSED mode: rt=%d, ka=%d", pausedRT-initRT, pausedKA-initKA)
		}

		// 2. Transition to KEEPALIVE_ONLY mode (60s min interval)
		keepaliveSettings := savedPaused
		keepaliveSettings.DefaultProfile.Mode = storage.ModeKeepaliveOnly
		keepaliveSettings.DefaultProfile.MinSeconds = 60
		keepaliveSettings.DefaultProfile.MaxSeconds = 60
		savedKeepalive, err := store.SaveMonitorSettings(ctx, keepaliveSettings)
		if err != nil {
			t.Fatal(err)
		}

		m.NotifySettingsChanged()
		time.Sleep(50 * time.Millisecond)

		// Before 60s, keepalive should not have fired yet
		time.Sleep(30 * time.Second)
		mu.Lock()
		midKA := keepalivePolls
		mu.Unlock()
		if midKA != pausedKA {
			t.Fatalf("keepalive fired prematurely at 30s: got %d", midKA)
		}

		// At 60s: keepalive task fires (keepalivePolls increments)
		time.Sleep(31 * time.Second)
		mu.Lock()
		afterKA := keepalivePolls
		afterRT := realtimePolls
		mu.Unlock()

		if afterKA <= pausedKA {
			t.Fatalf("expected keepalive task to execute after 60s, got ka=%d", afterKA)
		}
		if afterRT != pausedRT {
			t.Fatalf("keepalive mode unexpectedly ran realtime poll: rt=%d", afterRT-pausedRT)
		}

		// 3. Transition back to REALTIME mode: schedule resume triggers immediate realtime poll
		realtimeSettings := savedKeepalive
		realtimeSettings.DefaultProfile.Mode = storage.ModeRealtime
		realtimeSettings.DefaultProfile.MinSeconds = 10
		realtimeSettings.DefaultProfile.MaxSeconds = 10
		savedRealtime, err := store.SaveMonitorSettings(ctx, realtimeSettings)
		if err != nil {
			t.Fatal(err)
		}
		_ = savedRealtime

		m.NotifySettingsChanged()
		time.Sleep(50 * time.Millisecond)

		// Realtime poll should resume
		mu.Lock()
		resumedRT := realtimePolls
		mu.Unlock()

		if resumedRT <= afterRT {
			t.Fatalf("expected realtime poll to resume upon transition from KeepaliveOnly to Realtime, got %d", resumedRT)
		}
	})
}

// TestMonitorRun_BackoffSemantics_Synctest proves that when backoff is active,
// tasks scheduled by the timer respect backoff and defer upstream calls until backoff elapses.
func TestMonitorRun_BackoffSemantics_Synctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		store, _ := setupTestStoreAndConnection(t, ctx, "test_backoff.db")
		defer store.Close()

		var getCalls atomic.Int32
		mock := &countingMockClient{
			getFunc: func(ctx context.Context, endpoint string) (acb.Response, error) {
				getCalls.Add(1)
				return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHistoryHTML}, nil
			},
		}

		pollInterval := 10 * time.Second
		m := New(store, mock, pollInterval, pollInterval)
		m.nextInterval = func(min, max time.Duration) time.Duration {
			return pollInterval
		}

		settings := makeRealtimeSettings(pollInterval)
		if _, err := store.SaveMonitorSettings(ctx, settings); err != nil {
			t.Fatal(err)
		}
		m.cachedSettings = settings

		// Activate 25s backoff before starting Run
		m.SetBackoff(25 * time.Second)
		if !m.IsBackoffActive() {
			t.Fatal("expected backoff to be active")
		}

		go m.Run(ctx)

		// Startup poll is enqueued but scheduler task step sees backoff active -> requeues at backoff expiry
		time.Sleep(100 * time.Millisecond)
		if calls := getCalls.Load(); calls != 0 {
			t.Fatalf("expected 0 calls during backoff, got %d", calls)
		}

		// At 10s: timer fires second poll, still within 25s backoff -> 0 upstream calls
		time.Sleep(10 * time.Second)
		if calls := getCalls.Load(); calls != 0 {
			t.Fatalf("expected 0 calls during backoff at 10s, got %d", calls)
		}

		// Advance past 25s backoff (sleep 16s, total 26s).
		// Now backoff has elapsed -> task executes upstream request.
		time.Sleep(16 * time.Second)
		if calls := getCalls.Load(); calls == 0 {
			t.Fatal("expected task to execute after backoff window expired")
		}
	})
}
