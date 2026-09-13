package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type verifierMockTransport struct {
	roundTripFn func(req *http.Request) (*http.Response, error)
}

func (t *verifierMockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.roundTripFn != nil {
		return t.roundTripFn(req)
	}
	return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
}

func TestSessionVerifier_InteractiveOutranksRealtimeAndHistory(t *testing.T) {
	sched := scheduler.New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var orderMu sync.Mutex
	var order []string

	// Blocker to hold the scheduler while we enqueue tasks
	blockCh := make(chan struct{})
	blocker := &monitorMockTask{
		id:       "blocker",
		priority: PriorityKeepalive,
		stepFn: func(ctx context.Context) (TaskStepResult, error) {
			<-blockCh
			return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
		},
	}

	sched.Start(ctx)
	defer sched.Stop()

	_ = sched.Enqueue(blocker)
	time.Sleep(10 * time.Millisecond)

	// Enqueue History (10), Realtime (80), Interactive Verify (100)
	historyTask := &monitorMockTask{
		id:       "history_task",
		priority: PriorityFilterHistory,
		stepFn: func(ctx context.Context) (TaskStepResult, error) {
			orderMu.Lock()
			order = append(order, "history_task")
			orderMu.Unlock()
			return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
		},
	}
	realtimeTask := &monitorMockTask{
		id:       "realtime_task",
		priority: PriorityRealtimePoll,
		stepFn: func(ctx context.Context) (TaskStepResult, error) {
			orderMu.Lock()
			order = append(order, "realtime_task")
			orderMu.Unlock()
			return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
		},
	}
	verifyTask := &verifyTask{
		id:         "verify_task",
		generation: 1,
		stepFn: func(ctx context.Context) error {
			orderMu.Lock()
			order = append(order, "verify_task")
			orderMu.Unlock()
			return nil
		},
		done: make(chan error, 1),
	}

	_ = sched.Enqueue(historyTask)
	_ = sched.Enqueue(realtimeTask)
	_ = sched.Enqueue(verifyTask)

	close(blockCh)

	deadline := time.Now().Add(2 * time.Second)
	for sched.TotalQueueDepth() > 0 || sched.IsBusy() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for execution")
		}
		time.Sleep(10 * time.Millisecond)
	}

	orderMu.Lock()
	defer orderMu.Unlock()

	expected := []string{"verify_task", "realtime_task", "history_task"}
	if len(order) != len(expected) {
		t.Fatalf("expected order %v, got %v", expected, order)
	}
	for i, v := range expected {
		if order[i] != v {
			t.Errorf("at index %d: expected %s, got %s", i, v, order[i])
		}
	}
}

func TestSessionVerifier_NoConcurrentACBRequests(t *testing.T) {
	sched := scheduler.New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sched.Start(ctx)
	defer sched.Stop()

	var activeCalls atomic.Int32
	var maxConcurrent atomic.Int32

	recordConcurrency := func() func() {
		curr := activeCalls.Add(1)
		for {
			oldMax := maxConcurrent.Load()
			if curr <= oldMax || maxConcurrent.CompareAndSwap(oldMax, curr) {
				break
			}
		}
		return func() {
			activeCalls.Add(-1)
		}
	}

	const taskCount = 20
	var wg sync.WaitGroup
	for i := 0; i < taskCount; i++ {
		wg.Add(1)
		idx := i
		p := PriorityRealtimePoll
		if idx%3 == 0 {
			p = PriorityInteractiveVerify
		} else if idx%3 == 1 {
			p = PriorityCatchUp
		}

		go func() {
			defer wg.Done()
			task := &monitorMockTask{
				id:       fmt.Sprintf("task_%d", idx),
				priority: p,
				stepFn: func(ctx context.Context) (TaskStepResult, error) {
					done := recordConcurrency()
					time.Sleep(10 * time.Millisecond)
					done()
					return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
				},
			}
			_ = sched.Enqueue(task)
		}()
	}

	wg.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for sched.TotalQueueDepth() > 0 || sched.IsBusy() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for tasks to drain")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if maxConcurrent.Load() > 1 {
		t.Fatalf("INVARIANT VIOLATION: max concurrent ACB tasks = %d (must NEVER exceed 1)", maxConcurrent.Load())
	}
}

func TestSessionVerifier_GATE04_StoreErrorProducesZeroACBCalls(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "verifier_gate04.db"))
	if err != nil {
		t.Fatal(err)
	}

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET generation=1 WHERE id=?", conn.ID)

	var acbCallCount atomic.Int32
	mockTransport := &verifierMockTransport{
		roundTripFn: func(req *http.Request) (*http.Response, error) {
			acbCallCount.Add(1)
			return nil, errors.New("upstream must not be called")
		},
	}
	client, err := acb.NewClient("https://online.acb.com.vn", mockTransport)
	if err != nil {
		t.Fatal(err)
	}

	loader := NewSessionLoader(store, nil, nil)
	verifier := NewSessionVerifier(loader, client)

	// Close database to force database error during RestoreEnvelope
	store.Close()

	// Attempt verification
	err = verifier.VerifySession(ctx, conn.ID, 1, []byte("some-encrypted-envelope"))
	if err == nil {
		t.Fatal("expected error from failed store, got nil")
	}

	// Invariant GATE-04: zero ACB upstream calls
	if acbCallCount.Load() != 0 {
		t.Fatalf("GATE-04 VIOLATION: expected 0 ACB calls on store failure, got %d", acbCallCount.Load())
	}
}
