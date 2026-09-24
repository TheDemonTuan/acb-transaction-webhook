package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
)

type mockTask struct {
	id         string
	kind       string
	priority   UpstreamPriority
	generation int64
	key        string

	stepsRemaining int
	stepFn         func(ctx context.Context) (TaskStepResult, error)
}

func (m *mockTask) ID() string                 { return m.id }
func (m *mockTask) Kind() string               { return m.kind }
func (m *mockTask) Priority() UpstreamPriority { return m.priority }
func (m *mockTask) Generation() int64          { return m.generation }
func (m *mockTask) CoalesceKey() string {
	if m.key != "" {
		return m.key
	}
	return m.id
}

func (m *mockTask) Step(ctx context.Context) (TaskStepResult, error) {
	if m.stepFn != nil {
		return m.stepFn(ctx)
	}
	m.stepsRemaining--
	if m.stepsRemaining <= 0 {
		return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
	}
	return TaskStepResult{Done: false, Outcome: OutcomeSuccess}, nil
}

func TestScheduler_StrictPriorityOrder(t *testing.T) {
	q := NewTaskQueue(100)
	now := time.Now()

	// Enqueue in reverse order of priority: keepalive -> history -> catchup -> manual -> realtime -> auth
	tasks := []UpstreamTask{
		&mockTask{id: "keepalive", priority: PriorityKeepalive},
		&mockTask{id: "history", priority: PriorityHistory},
		&mockTask{id: "catchup", priority: PriorityCatchUp},
		&mockTask{id: "manual", priority: PriorityManualSync},
		&mockTask{id: "realtime", priority: PriorityRealtime},
		&mockTask{id: "auth", priority: PriorityAuth},
	}

	for _, task := range tasks {
		if err := q.Push(task, now); err != nil {
			t.Fatalf("push %s failed: %v", task.ID(), err)
		}
	}

	expectedOrder := []string{"auth", "realtime", "manual", "catchup", "history", "keepalive"}
	for _, expected := range expectedOrder {
		popped := q.Pop()
		if popped == nil {
			t.Fatalf("expected task %s, got nil", expected)
		}
		if popped.ID() != expected {
			t.Fatalf("expected task %s, got %s", expected, popped.ID())
		}
	}
}

func TestScheduler_AgingOnlyWithinClass(t *testing.T) {
	q := NewTaskQueue(100)
	t0 := time.Now()
	t1 := t0.Add(1 * time.Second)
	t2 := t0.Add(2 * time.Second)
	t3 := t0.Add(3 * time.Second)

	// Low-priority task arrived first at t0
	lowOlder := &mockTask{id: "low_older", priority: PriorityHistory}
	if err := q.Push(lowOlder, t0); err != nil {
		t.Fatal(err)
	}

	// Another low-priority task arrived later at t1
	lowNewer := &mockTask{id: "low_newer", priority: PriorityHistory}
	if err := q.Push(lowNewer, t1); err != nil {
		t.Fatal(err)
	}

	// High-priority task arrived much later at t2
	highOlder := &mockTask{id: "high_older", priority: PriorityRealtime}
	if err := q.Push(highOlder, t2); err != nil {
		t.Fatal(err)
	}

	// Another high-priority task arrived even later at t3
	highNewer := &mockTask{id: "high_newer", priority: PriorityRealtime}
	if err := q.Push(highNewer, t3); err != nil {
		t.Fatal(err)
	}

	// Expected pop sequence:
	// 1. high_older (High priority, older within high class)
	// 2. high_newer (High priority, newer within high class)
	// 3. low_older (Low priority, older within low class)
	// 4. low_newer (Low priority, newer within low class)
	// Notice low_older NEVER preempts high tasks despite arriving at t0!
	expected := []string{"high_older", "high_newer", "low_older", "low_newer"}
	for _, id := range expected {
		popped := q.Pop()
		if popped == nil || popped.ID() != id {
			t.Fatalf("expected %s, got %v", id, popped)
		}
	}
}

func TestScheduler_QuantumYielding_PreemptedByRealtime(t *testing.T) {
	sched := New(&Options{
		QuantumTimeout: 5 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sched.Start(ctx)
	defer sched.Stop()

	var orderMu sync.Mutex
	var executionOrder []string

	step1Started := make(chan struct{})
	step1Release := make(chan struct{})

	// Multi-step history task
	historyTask := &mockTask{
		id:       "history_job",
		kind:     "FILTER_HISTORY",
		priority: PriorityHistory,
	}

	stepCount := 0
	historyTask.stepFn = func(ctx context.Context) (TaskStepResult, error) {
		orderMu.Lock()
		stepCount++
		currStep := stepCount
		executionOrder = append(executionOrder, "history_step")
		orderMu.Unlock()

		if currStep == 1 {
			close(step1Started)
			// Wait until test enqueues realtime task
			select {
			case <-step1Release:
			case <-ctx.Done():
				return TaskStepResult{}, ctx.Err()
			}
			// Yield quantum: 1 page complete, more pages remaining!
			return TaskStepResult{Done: false, Outcome: OutcomeSuccess}, nil
		}

		// Step 2 completes
		return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
	}

	if err := sched.Enqueue(historyTask); err != nil {
		t.Fatalf("enqueue history task failed: %v", err)
	}

	// Wait until history step 1 starts executing
	<-step1Started

	// Enqueue realtime task while history step 1 is in-flight
	realtimeDone := make(chan struct{})
	realtimeTask := &mockTask{
		id:       "realtime_poll",
		kind:     "REALTIME_POLL",
		priority: PriorityRealtime,
		stepFn: func(ctx context.Context) (TaskStepResult, error) {
			orderMu.Lock()
			executionOrder = append(executionOrder, "realtime_step")
			orderMu.Unlock()
			close(realtimeDone)
			return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
		},
	}

	if err := sched.Enqueue(realtimeTask); err != nil {
		t.Fatalf("enqueue realtime task failed: %v", err)
	}

	// Allow history step 1 to finish and yield
	close(step1Release)

	// Wait for realtime task to execute
	select {
	case <-realtimeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for realtime step")
	}

	// Wait for scheduler to finish both tasks
	deadline := time.Now().Add(2 * time.Second)
	for sched.TotalQueueDepth() > 0 || sched.IsBusy() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for tasks to drain")
		}
		time.Sleep(10 * time.Millisecond)
	}

	orderMu.Lock()
	defer orderMu.Unlock()

	// Proves realtime executed before history step 2!
	expected := []string{"history_step", "realtime_step", "history_step"}
	if len(executionOrder) != len(expected) {
		t.Fatalf("unexpected execution order length: %v", executionOrder)
	}
	for i, v := range expected {
		if executionOrder[i] != v {
			t.Fatalf("at index %d: expected %s, got %s (full order: %v)", i, v, executionOrder[i], executionOrder)
		}
	}
}

func TestScheduler_Coalescing(t *testing.T) {
	q := NewTaskQueue(10)

	// Enqueue realtime poll task
	t1 := &mockTask{id: "rt-1", key: "REALTIME_POLL", priority: PriorityRealtime, generation: 1}
	if err := q.Push(t1, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Enqueue duplicate realtime poll task
	t2 := &mockTask{id: "rt-2", key: "REALTIME_POLL", priority: PriorityRealtime, generation: 1}
	if err := q.Push(t2, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Should coalesce: length remains 1
	if q.Len() != 1 {
		t.Fatalf("expected queue len 1 after duplicate push, got %d", q.Len())
	}

	// Enqueue newer generation
	t3 := &mockTask{id: "rt-3", key: "REALTIME_POLL", priority: PriorityRealtime, generation: 2}
	if err := q.Push(t3, time.Now()); err != nil {
		t.Fatal(err)
	}

	if q.Len() != 1 {
		t.Fatalf("expected queue len 1 after newer generation push, got %d", q.Len())
	}

	popped := q.Pop()
	if popped.ID() != "rt-3" {
		t.Fatalf("expected updated generation task rt-3, got %s", popped.ID())
	}
}

func TestScheduler_BoundedQueue_Overload(t *testing.T) {
	sched := New(&Options{
		MaxCapacity: 2,
	})

	t1 := &mockTask{id: "t1", priority: PriorityHistory}
	t2 := &mockTask{id: "t2", priority: PriorityHistory}
	t3 := &mockTask{id: "t3", priority: PriorityHistory}

	if err := sched.Enqueue(t1); err != nil {
		t.Fatalf("enqueue t1: %v", err)
	}
	if err := sched.Enqueue(t2); err != nil {
		t.Fatalf("enqueue t2: %v", err)
	}

	// Overload: 3rd task exceeds capacity 2
	err := sched.Enqueue(t3)
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull on queue overload, got %v", err)
	}
}

func TestSchedulerCoalescesTaskWhileOriginalIsDelayed(t *testing.T) {
	sched := New(nil)
	task := &mockTask{id: "first", key: "REALTIME_POLL", priority: PriorityRealtime, generation: 7}
	sched.scheduleDelayed(task, time.Now().Add(time.Minute))
	if err := sched.Enqueue(&mockTask{id: "second", key: "REALTIME_POLL", priority: PriorityRealtime, generation: 7}); err != nil {
		t.Fatal(err)
	}
	if got := sched.TotalQueueDepth(); got != 0 {
		t.Fatalf("duplicate task entered ready queue, depth=%d", got)
	}
	if len(sched.delayed) != 1 {
		t.Fatalf("expected one delayed task, got %d", len(sched.delayed))
	}
}

func TestSchedulerPauseAndDrainWaitsWithoutCancelingActiveStep(t *testing.T) {
	sched := New(&Options{QuantumTimeout: time.Second})
	sched.Start(context.Background())
	defer sched.Stop()

	started := make(chan struct{})
	release := make(chan struct{})
	canceled := atomic.Bool{}
	task := &mockTask{id: "stateful", kind: "REALTIME_POLL", priority: PriorityRealtime, stepFn: func(ctx context.Context) (TaskStepResult, error) {
		close(started)
		select {
		case <-ctx.Done():
			canceled.Store(true)
			return TaskStepResult{Done: true, Error: ctx.Err(), Outcome: OutcomeTransient}, ctx.Err()
		case <-release:
			return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
		}
	}}
	if err := sched.Enqueue(task); err != nil {
		t.Fatal(err)
	}
	<-started

	drained := make(chan error, 1)
	go func() { drained <- sched.PauseAndDrain(context.Background()) }()
	select {
	case err := <-drained:
		t.Fatalf("drain returned before active step finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := sched.Enqueue(&mockTask{id: "rejected", priority: PriorityRealtime}); !errors.Is(err, ErrSchedulerPaused) {
		t.Fatalf("expected paused error, got %v", err)
	}
	close(release)
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if canceled.Load() {
		t.Fatal("pause canceled the active stateful request")
	}
}

func TestSchedulerPauseAndDrainHonorsDeadline(t *testing.T) {
	sched := New(&Options{QuantumTimeout: time.Second})
	sched.Start(context.Background())
	defer sched.Stop()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	if err := sched.Enqueue(&mockTask{id: "blocked", kind: "REALTIME_POLL", priority: PriorityRealtime, stepFn: func(context.Context) (TaskStepResult, error) {
		close(started)
		<-release
		return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := sched.PauseAndDrain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestScheduler_Cancellation(t *testing.T) {
	sched := New(nil)
	ctx := context.Background()
	sched.Start(ctx)
	started := make(chan struct{})
	release := make(chan struct{})
	if err := sched.Enqueue(&mockTask{id: "blocking", priority: PriorityHistory, stepFn: func(context.Context) (TaskStepResult, error) {
		close(started)
		<-release
		return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := sched.Enqueue(&mockTask{id: "task_to_cancel", priority: PriorityHistory}); err != nil {
		t.Fatal(err)
	}
	// Cancel queued task
	if !sched.CancelTask("task_to_cancel") {
		t.Fatal("expected CancelTask to return true for queued task")
	}
	close(release)

	// Stop scheduler cleanly
	if err := sched.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	// Enqueueing to stopped scheduler returns typed error
	err := sched.Enqueue(&mockTask{id: "after_stop", priority: PriorityRealtime})
	if !errors.Is(err, ErrSchedulerStopped) {
		t.Fatalf("expected ErrSchedulerStopped, got %v", err)
	}
}

func TestScheduler_SingleActiveGoroutine_Invariant(t *testing.T) {
	sched := New(nil)
	sched.Start(context.Background())
	defer sched.Stop()

	var maxConcurrent atomic.Int32
	var currentConcurrent atomic.Int32

	const taskCount = 30
	var wg sync.WaitGroup
	wg.Add(taskCount)

	for i := 0; i < taskCount; i++ {
		task := &mockTask{
			id:       fmt.Sprintf("invariant_task_%d", i),
			priority: UpstreamPriority(i % 100),
			stepFn: func(ctx context.Context) (TaskStepResult, error) {
				curr := currentConcurrent.Add(1)
				defer currentConcurrent.Add(-1)

				for {
					old := maxConcurrent.Load()
					if curr > old {
						if maxConcurrent.CompareAndSwap(old, curr) {
							break
						}
					} else {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				wg.Done()
				return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
			},
		}
		_ = sched.Enqueue(task)
	}

	wg.Wait()

	if maxConcurrent.Load() != 1 {
		t.Fatalf("invariant violated: max concurrent tasks was %d, must be strictly 1", maxConcurrent.Load())
	}
}

func TestScheduler_TypedOutcomes(t *testing.T) {
	// Transient error
	tErr := &TransientError{Err: errors.New("timeout")}
	if ClassifyOutcome(tErr, nil) != OutcomeTransient {
		t.Fatalf("expected OutcomeTransient, got %s", ClassifyOutcome(tErr, nil))
	}

	// Auth error
	aErr := &AuthError{Err: errors.New("login expired"), Kind: acb.LoginPage}
	if ClassifyOutcome(aErr, nil) != OutcomeAuth {
		t.Fatalf("expected OutcomeAuth, got %s", ClassifyOutcome(aErr, nil))
	}

	// Fatal error
	fErr := &FatalError{Err: errors.New("unrecoverable schema error")}
	if ClassifyOutcome(fErr, nil) != OutcomeFatal {
		t.Fatalf("expected OutcomeFatal, got %s", ClassifyOutcome(fErr, nil))
	}

	// Response classification
	if ClassifyOutcome(nil, &acb.Response{Kind: acb.LoginPage}) != OutcomeAuth {
		t.Fatal("expected LoginPage to classify as OutcomeAuth")
	}
	if ClassifyOutcome(nil, &acb.Response{Kind: acb.MaintenancePage}) != OutcomeTransient {
		t.Fatal("expected MaintenancePage to classify as OutcomeTransient")
	}
	if ClassifyOutcome(nil, &acb.Response{Kind: acb.UnknownPage}) != OutcomeFatal {
		t.Fatal("expected UnknownPage to classify as OutcomeFatal")
	}
	if ClassifyOutcome(nil, &acb.Response{Kind: acb.HistoryPage}) != OutcomeSuccess {
		t.Fatal("expected HistoryPage to classify as OutcomeSuccess")
	}
}

func TestScheduler_Observability(t *testing.T) {
	metrics := NewDefaultMetrics()
	sched := New(&Options{Metrics: metrics})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)
	defer sched.Stop()

	task := &mockTask{
		id:       "obs_task",
		kind:     "REALTIME_POLL",
		priority: PriorityRealtime,
		stepFn: func(ctx context.Context) (TaskStepResult, error) {
			time.Sleep(10 * time.Millisecond)
			return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
		},
	}

	if err := sched.Enqueue(task); err != nil {
		t.Fatal(err)
	}

	// Wait for task to complete
	deadline := time.Now().Add(1 * time.Second)
	for sched.TotalQueueDepth() > 0 || sched.IsBusy() {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for task")
		}
		time.Sleep(5 * time.Millisecond)
	}

	completed, _ := metrics.Snapshot()
	if completed["REALTIME_POLL"] != 1 {
		t.Fatalf("expected 1 completed REALTIME_POLL in metrics, got %+v", completed)
	}
}
