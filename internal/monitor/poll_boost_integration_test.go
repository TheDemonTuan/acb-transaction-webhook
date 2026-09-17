package monitor

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestMonitor_QRActivationInterruptsIdleTimerAndWaitsGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, _ = store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`)

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
		pollCount.Add(1)
	})

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		m.Run(ctx)
	}()

	// Wait for initial startup poll
	deadline := time.Now().Add(2 * time.Second)
	for pollCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if count := pollCount.Load(); count != 1 {
		t.Fatalf("expected initial startup poll (count=1), got %d", count)
	}

	// Trigger activation
	profile := m.ActivatePaymentWindow()
	if profile.Phase != PollBoostGrace {
		t.Fatalf("expected GRACE phase on fresh activation, got %v", profile.Phase)
	}

	// During grace (5 seconds), no new polls must be enqueued
	time.Sleep(300 * time.Millisecond)
	if count := pollCount.Load(); count != 1 {
		t.Fatalf("expected pollCount to remain 1 during 5s GRACE period, got %d", count)
	}

	cancel()
	<-runDone
}

func TestMonitor_ResolveEffectivePriority(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	m := New(store, &mockBankClient{}, 20*time.Second, 30*time.Second)
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return start }

	// 1. Admin PAUSED priority over QR boost
	m.ActivatePaymentWindow()
	pausedBase := storage.ResolvedSchedule{Mode: storage.ModePaused}
	boost := m.ResolvePollBoost(start)
	effPaused := m.resolveEffective(start, pausedBase, boost)
	if effPaused.mode != storage.ModePaused {
		t.Fatalf("expected ModePaused to win over QR boost, got %v", effPaused.mode)
	}

	// 2. KEEPALIVE_ONLY is boosted to REALTIME during boost
	keepaliveBase := storage.ResolvedSchedule{
		Mode:        storage.ModeKeepaliveOnly,
		MinInterval: 60 * time.Second,
		MaxInterval: 120 * time.Second,
	}
	// At T+0 (in GRACE)
	effGrace := m.resolveEffective(start, keepaliveBase, boost)
	if effGrace.mode != storage.ModeRealtime || effGrace.phase != PollBoostGrace {
		t.Fatalf("expected ModeRealtime in GRACE, got mode=%v phase=%v", effGrace.mode, effGrace.phase)
	}
	if effGrace.graceWait != 5*time.Second {
		t.Fatalf("expected graceWait=5s, got %v", effGrace.graceWait)
	}

	// At T+5s (HOT)
	t5 := start.Add(5 * time.Second)
	boostHot := m.ResolvePollBoost(t5)
	effHot := m.resolveEffective(t5, keepaliveBase, boostHot)
	if effHot.mode != storage.ModeRealtime || effHot.phase != PollBoostHot {
		t.Fatalf("expected ModeRealtime in HOT, got mode=%v phase=%v", effHot.mode, effHot.phase)
	}
	if effHot.minInterval != 2*time.Second || effHot.maxInterval != 4*time.Second {
		t.Fatalf("expected 2-4s interval in HOT, got min=%v max=%v", effHot.minInterval, effHot.maxInterval)
	}

	// At T+60s (WARM)
	t60 := start.Add(60 * time.Second)
	boostWarm := m.ResolvePollBoost(t60)
	effWarm := m.resolveEffective(t60, keepaliveBase, boostWarm)
	if effWarm.mode != storage.ModeRealtime || effWarm.phase != PollBoostWarm {
		t.Fatalf("expected ModeRealtime in WARM, got mode=%v phase=%v", effWarm.mode, effWarm.phase)
	}
	if effWarm.minInterval != 3*time.Second || effWarm.maxInterval != 6*time.Second {
		t.Fatalf("expected 3-6s interval in WARM, got min=%v max=%v", effWarm.minInterval, effWarm.maxInterval)
	}

	// At T+120s (COOL)
	t120 := start.Add(120 * time.Second)
	boostCool := m.ResolvePollBoost(t120)
	effCool := m.resolveEffective(t120, keepaliveBase, boostCool)
	if effCool.mode != storage.ModeRealtime || effCool.phase != PollBoostCool {
		t.Fatalf("expected ModeRealtime in COOL, got mode=%v phase=%v", effCool.mode, effCool.phase)
	}
	if effCool.minInterval != 6*time.Second || effCool.maxInterval != 10*time.Second {
		t.Fatalf("expected 6-10s interval in COOL, got min=%v max=%v", effCool.minInterval, effCool.maxInterval)
	}

	// At T+180s (Expired to IDLE -> returns to base KEEPALIVE_ONLY)
	t180 := start.Add(180 * time.Second)
	boostIdle := m.ResolvePollBoost(t180)
	effIdle := m.resolveEffective(t180, keepaliveBase, boostIdle)
	if effIdle.mode != storage.ModeKeepaliveOnly || effIdle.phase != PollBoostIdle {
		t.Fatalf("expected ModeKeepaliveOnly after expiration, got mode=%v phase=%v", effIdle.mode, effIdle.phase)
	}
}

func TestMonitor_BackoffWinsOverBoost(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	m := New(store, &mockBankClient{}, 20*time.Second, 30*time.Second)
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return start }

	m.ActivatePaymentWindow()
	m.SetBackoff(20 * time.Second)

	if !m.IsBackoffActive() {
		t.Fatal("expected backoff to be active")
	}
	until := m.BackoffUntil()
	if until.Sub(start) != 20*time.Second {
		t.Fatalf("expected 20s backoff, got %v", until.Sub(start))
	}
}

func TestMonitor_PausedModeIgnoresQRActivation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, _ = store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`)

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
	pausedSettings := storage.MonitorSettings{
		Revision: 1,
		Enabled:  true,
		DefaultProfile: storage.Profile{
			Mode: storage.ModePaused,
		},
	}
	_, _ = store.SaveMonitorSettings(ctx, pausedSettings)
	m.cachedSettings = pausedSettings
	m.WithPollNotifier(func(poll storage.PollRun, insertedCount int) {
		pollCount.Add(1)
	})

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		m.Run(ctx)
	}()

	time.Sleep(150 * time.Millisecond)

	// Activate boost
	m.ActivatePaymentWindow()

	// Wait 200ms and verify no poll was scheduled or executed
	time.Sleep(200 * time.Millisecond)
	if count := pollCount.Load(); count != 0 {
		t.Fatalf("expected 0 polls in paused mode despite QR activation, got %d", count)
	}

	cancel()
	<-runDone
}

func TestMonitor_ConcurrentActivationsRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, _ = store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`)

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
	m.WithPollNotifier(func(poll storage.PollRun, insertedCount int) {
		pollCount.Add(1)
	})

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		m.Run(ctx)
	}()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.ActivatePaymentWindow()
		}()
	}
	wg.Wait()

	boost := m.ResolvePollBoost(time.Now())
	if !boost.Active {
		t.Fatal("expected boost to remain active after concurrent activations")
	}

	cancel()
	<-runDone
}
