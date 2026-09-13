package maintenance

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockStore struct {
	mu             sync.Mutex
	deleteCalls    int
	lastCutoff     time.Time
	deleteErr      error
	deleteCount    int64
	expireCalls    int
	expireErr      error
	expireCount    int64
	onDeleteHook   func(cutoff time.Time)
	onExpireHook   func()
}

func (m *mockStore) DeleteJournalBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls++
	m.lastCutoff = cutoff
	if m.onDeleteHook != nil {
		m.onDeleteHook(cutoff)
	}
	return m.deleteCount, m.deleteErr
}

func (m *mockStore) ExpireStaleAuthAttempts(ctx context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireCalls++
	if m.onExpireHook != nil {
		m.onExpireHook()
	}
	return m.expireCount, m.expireErr
}

func TestMaintenanceRunnerRunOnce(t *testing.T) {
	fixedNow := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	store := &mockStore{deleteCount: 15, expireCount: 3}
	runner := NewRunner(store,
		WithRetentionPeriod(48*time.Hour),
		WithNowFunc(func() time.Time { return fixedNow }),
	)

	ctx := context.Background()
	err := runner.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if store.deleteCalls != 1 {
		t.Errorf("expected 1 delete call, got %d", store.deleteCalls)
	}
	expectedCutoff := fixedNow.Add(-48 * time.Hour)
	if !store.lastCutoff.Equal(expectedCutoff) {
		t.Errorf("expected cutoff %v, got %v", expectedCutoff, store.lastCutoff)
	}
	if store.expireCalls != 1 {
		t.Errorf("expected 1 expire call, got %d", store.expireCalls)
	}
}

func TestMaintenanceRunnerControllableTriggers(t *testing.T) {
	fixedNow := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	store := &mockStore{deleteCount: 5, expireCount: 1}

	retentionCh := make(chan time.Time, 10)
	staleAuthCh := make(chan time.Time, 10)

	runner := NewRunner(store,
		WithRetentionPeriod(24*time.Hour),
		WithNowFunc(func() time.Time { return fixedNow }),
		WithRetentionTrigger(retentionCh),
		WithStaleAuthTrigger(staleAuthCh),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(runDone)
	}()

	// Trigger retention twice
	retentionCh <- fixedNow
	retentionCh <- fixedNow.Add(1 * time.Hour)

	// Trigger stale-auth reap three times
	staleAuthCh <- fixedNow
	staleAuthCh <- fixedNow
	staleAuthCh <- fixedNow

	// Wait briefly for triggers to be consumed
	time.Sleep(50 * time.Millisecond)

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not exit after context cancel")
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if store.deleteCalls != 2 {
		t.Errorf("expected 2 delete calls, got %d", store.deleteCalls)
	}
	if store.expireCalls != 3 {
		t.Errorf("expected 3 expire calls, got %d", store.expireCalls)
	}
}

func TestMaintenanceRunnerErrorResilience(t *testing.T) {
	store := &mockStore{
		deleteErr: errors.New("db disk I/O error"),
		expireErr: errors.New("db lock timeout"),
	}

	retentionCh := make(chan time.Time, 1)
	staleAuthCh := make(chan time.Time, 1)

	runner := NewRunner(store,
		WithRetentionTrigger(retentionCh),
		WithStaleAuthTrigger(staleAuthCh),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(runDone)
	}()

	retentionCh <- time.Now()
	staleAuthCh <- time.Now()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not exit after context cancel")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.deleteCalls != 1 || store.expireCalls != 1 {
		t.Errorf("expected 1 call each despite errors, got delete=%d, expire=%d",
			store.deleteCalls, store.expireCalls)
	}
}

func TestMaintenanceRunnerConcurrentStarts(t *testing.T) {
	store := &mockStore{}
	runner := NewRunner(store)

	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started.Add(1)
			runner.Run(ctx)
		}()
	}

	time.Sleep(30 * time.Millisecond)
	cancel()
	wg.Wait()

	if started.Load() != 5 {
		t.Errorf("expected 5 goroutines to run, got %d", started.Load())
	}
}
