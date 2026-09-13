package monitor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
)

type monitorMockTask struct {
	id         string
	kind       string
	priority   UpstreamPriority
	generation int64
	stepFn     func(ctx context.Context) (TaskStepResult, error)
}

func (m *monitorMockTask) ID() string                { return m.id }
func (m *monitorMockTask) Kind() string              { return m.kind }
func (m *monitorMockTask) Priority() UpstreamPriority { return m.priority }
func (m *monitorMockTask) Generation() int64         { return m.generation }
func (m *monitorMockTask) Step(ctx context.Context) (TaskStepResult, error) {
	if m.stepFn != nil {
		return m.stepFn(ctx)
	}
	return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
}

func TestMonitor_UpstreamSchedulerIntegration(t *testing.T) {
	m := New(nil, nil, 5*time.Second, 15*time.Second)
	sched := m.Scheduler()
	if sched == nil {
		t.Fatal("expected non-nil scheduler from monitor")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sched.Start(ctx)
	defer sched.Stop()

	var executed bool
	task := &monitorMockTask{
		id:       "verify_login",
		kind:     "INTERACTIVE_VERIFY",
		priority: PriorityInteractiveVerify,
		stepFn: func(ctx context.Context) (TaskStepResult, error) {
			executed = true
			return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
		},
	}

	if err := sched.Enqueue(task); err != nil {
		t.Fatalf("enqueue task failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !executed {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for task execution")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMonitor_PriorityOrder_InteractiveOverRealtimeOverCatchup(t *testing.T) {
	sched := scheduler.New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var orderMu sync.Mutex
	var order []string

	// Pause worker execution by placing a blocking step
	blockCh := make(chan struct{})
	firstTask := &monitorMockTask{
		id:       "blocker",
		priority: PriorityKeepalive,
		stepFn: func(ctx context.Context) (TaskStepResult, error) {
			<-blockCh
			return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
		},
	}

	sched.Start(ctx)
	defer sched.Stop()

	_ = sched.Enqueue(firstTask)
	time.Sleep(10 * time.Millisecond) // ensure blocker is popped and running

	// Enqueue catch-up, realtime, and interactive verify while blocker is running
	tasks := []*monitorMockTask{
		{id: "catchup_task", priority: PriorityCatchUp},
		{id: "realtime_task", priority: PriorityRealtimePoll},
		{id: "interactive_task", priority: PriorityInteractiveVerify},
	}

	for _, task := range tasks {
		tRef := task
		tRef.stepFn = func(ctx context.Context) (TaskStepResult, error) {
			orderMu.Lock()
			order = append(order, tRef.id)
			orderMu.Unlock()
			return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
		}
		if err := sched.Enqueue(tRef); err != nil {
			t.Fatalf("enqueue %s: %v", tRef.id, err)
		}
	}

	// Unblock
	close(blockCh)

	// Wait for queue to drain
	deadline := time.Now().Add(2 * time.Second)
	for sched.TotalQueueDepth() > 0 || sched.IsBusy() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for tasks")
		}
		time.Sleep(10 * time.Millisecond)
	}

	orderMu.Lock()
	defer orderMu.Unlock()

	expected := []string{"interactive_task", "realtime_task", "catchup_task"}
	if len(order) != len(expected) {
		t.Fatalf("unexpected order length: %v", order)
	}
	for i, exp := range expected {
		if order[i] != exp {
			t.Errorf("at index %d: expected %s, got %s", i, exp, order[i])
		}
	}
}

func TestMonitor_SchedulerShutdownCancellation(t *testing.T) {
	sched := scheduler.New(nil)
	ctx := context.Background()
	sched.Start(ctx)

	stepStarted := make(chan struct{})
	var stepCanceled bool

	longTask := &monitorMockTask{
		id:       "long_quantum",
		priority: PriorityFilterHistory,
		stepFn: func(ctx context.Context) (TaskStepResult, error) {
			close(stepStarted)
			<-ctx.Done()
			stepCanceled = true
			return TaskStepResult{Done: false}, ctx.Err()
		},
	}

	if err := sched.Enqueue(longTask); err != nil {
		t.Fatal(err)
	}

	<-stepStarted

	// Stop scheduler must cancel in-flight step
	if err := sched.Stop(); err != nil {
		t.Fatalf("stop failed: %v", err)
	}

	if !stepCanceled {
		t.Fatal("expected in-flight step context to be canceled on Stop()")
	}

	// Enqueue to stopped scheduler must fail closed
	err := sched.Enqueue(&monitorMockTask{id: "new_after_stop", priority: PriorityRealtimePoll})
	if !errors.Is(err, scheduler.ErrSchedulerStopped) {
		t.Fatalf("expected ErrSchedulerStopped, got %v", err)
	}
}
