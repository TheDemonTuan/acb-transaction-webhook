package monitor

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestMonitor_CadenceDeterministicFakeClockTransitions(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cadence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Base setting: daytime window 07:00-23:00 has REALTIME 20-30s
	// default nighttime has KEEPALIVE_ONLY 60-120s
	settings := storage.DefaultMonitorSettings
	_, _ = store.SaveMonitorSettings(ctx, settings)

	m := New(store, &mockBankClient{}, 20*time.Second, 30*time.Second)
	m.cachedSettings = settings

	// Anchor test time at daytime 12:00
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return start }

	// 1. Before activation: base daytime is REALTIME 20-30s
	sched0 := storage.ResolveSchedule(start, &settings)
	boost0 := m.ResolvePollBoost(start)
	eff0 := m.resolveEffective(start, sched0, boost0)
	if eff0.mode != storage.ModeRealtime || eff0.phase != PollBoostIdle {
		t.Fatalf("expected base REALTIME IDLE, got mode=%v phase=%v", eff0.mode, eff0.phase)
	}
	if eff0.minInterval != 20*time.Second || eff0.maxInterval != 30*time.Second {
		t.Fatalf("expected base 20-30s cadence, got min=%v max=%v", eff0.minInterval, eff0.maxInterval)
	}

	// 2. Activate at T+0 -> GRACE 5s
	m.ActivatePaymentWindow()

	boostAt0 := m.ResolvePollBoost(start)
	effAt0 := m.resolveEffective(start, sched0, boostAt0)
	if effAt0.phase != PollBoostGrace || effAt0.graceWait != 5*time.Second {
		t.Fatalf("expected GRACE phase with 5s wait at T+0, got phase=%v wait=%v", effAt0.phase, effAt0.graceWait)
	}

	// 3. At T+5s -> HOT 2-4s
	t5 := start.Add(5 * time.Second)
	boostAt5 := m.ResolvePollBoost(t5)
	effAt5 := m.resolveEffective(t5, sched0, boostAt5)
	if effAt5.phase != PollBoostHot {
		t.Fatalf("expected HOT phase at T+5s, got %v", effAt5.phase)
	}
	if effAt5.minInterval != 2*time.Second || effAt5.maxInterval != 4*time.Second {
		t.Fatalf("expected HOT 2-4s cadence, got %v-%v", effAt5.minInterval, effAt5.maxInterval)
	}

	// 4. At T+60s -> WARM 3-6s
	t60 := start.Add(60 * time.Second)
	boostAt60 := m.ResolvePollBoost(t60)
	effAt60 := m.resolveEffective(t60, sched0, boostAt60)
	if effAt60.phase != PollBoostWarm {
		t.Fatalf("expected WARM phase at T+60s, got %v", effAt60.phase)
	}
	if effAt60.minInterval != 3*time.Second || effAt60.maxInterval != 6*time.Second {
		t.Fatalf("expected WARM 3-6s cadence, got %v-%v", effAt60.minInterval, effAt60.maxInterval)
	}

	// 5. At T+120s -> COOL 6-10s
	t120 := start.Add(120 * time.Second)
	boostAt120 := m.ResolvePollBoost(t120)
	effAt120 := m.resolveEffective(t120, sched0, boostAt120)
	if effAt120.phase != PollBoostCool {
		t.Fatalf("expected COOL phase at T+120s, got %v", effAt120.phase)
	}
	if effAt120.minInterval != 6*time.Second || effAt120.maxInterval != 10*time.Second {
		t.Fatalf("expected COOL 6-10s cadence, got %v-%v", effAt120.minInterval, effAt120.maxInterval)
	}

	// 6. At T+180s -> Expires cleanly back to base 20-30s (NOT 3-10s)
	t180 := start.Add(180 * time.Second)
	boostAt180 := m.ResolvePollBoost(t180)
	effAt180 := m.resolveEffective(t180, sched0, boostAt180)
	if effAt180.phase != PollBoostIdle || effAt180.isBoosted {
		t.Fatalf("expected IDLE unboosted at T+180s, got phase=%v boosted=%v", effAt180.phase, effAt180.isBoosted)
	}
	if effAt180.minInterval != 20*time.Second || effAt180.maxInterval != 30*time.Second {
		t.Fatalf("expected return to base 20-30s cadence, got %v-%v", effAt180.minInterval, effAt180.maxInterval)
	}
}

func TestMonitor_RunLoopScalesTimingAccurately(t *testing.T) {
	// Temporarily scale down durations to run an actual Run loop cadence test under 1 second
	origGrace := paymentGraceDuration
	origHotDuration := paymentHotDuration
	origHotMin := paymentHotMin
	origHotMax := paymentHotMax

	paymentGraceDuration = 30 * time.Millisecond
	paymentHotDuration = 100 * time.Millisecond
	paymentHotMin = 15 * time.Millisecond
	paymentHotMax = 25 * time.Millisecond

	defer func() {
		paymentGraceDuration = origGrace
		paymentHotDuration = origHotDuration
		paymentHotMin = origHotMin
		paymentHotMax = origHotMax
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "run_timing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, _ = store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`)

	var pollTimestamps []time.Time
	var pollCount atomic.Int64

	client := &mockBankClient{
		getResp: acb.Response{
			StatusCode: 200,
			Kind:       acb.HistoryPage,
			Body:       mockHistoryHTML,
		},
		historyResp: acb.Response{
			StatusCode: 200,
			Kind:       acb.HistoryPage,
			Body:       mockHistoryHTML,
		},
	}

	m := New(store, client, 20*time.Second, 30*time.Second)
	realtimeSettings := storage.MonitorSettings{
		Revision: 1,
		Enabled:  true,
		DefaultProfile: storage.Profile{
			Mode:       storage.ModeRealtime,
			MinSeconds: 20,
			MaxSeconds: 30,
		},
	}
	_, _ = store.SaveMonitorSettings(ctx, realtimeSettings)
	m.cachedSettings = realtimeSettings

	m.WithPollNotifier(func(poll storage.PollRun, insertedCount int) {
		pollTimestamps = append(pollTimestamps, time.Now())
		pollCount.Add(1)
	})

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		m.Run(ctx)
	}()

	// Wait for startup poll
	deadline := time.Now().Add(2 * time.Second)
	for pollCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pollCount.Load() != 1 {
		t.Fatalf("expected 1 startup poll, got %d", pollCount.Load())
	}

	// Trigger activation
	activateTime := time.Now()
	m.ActivatePaymentWindow()

	// Wait for grace (30ms) to pass and at least 2 HOT polls to fire
	pollDeadline := time.Now().Add(500 * time.Millisecond)
	for pollCount.Load() < 3 && time.Now().Before(pollDeadline) {
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	<-runDone

	if pollCount.Load() < 2 {
		t.Fatalf("expected at least 2 polls during test, got %d", pollCount.Load())
	}

	// First post-activation poll must not have happened before grace window
	if len(pollTimestamps) >= 2 {
		firstBoostPoll := pollTimestamps[1]
		elapsedSinceActivation := firstBoostPoll.Sub(activateTime)
		if elapsedSinceActivation < 25*time.Millisecond {
			t.Fatalf("poll occurred before grace elapsed: %v", elapsedSinceActivation)
		}
	}
}
