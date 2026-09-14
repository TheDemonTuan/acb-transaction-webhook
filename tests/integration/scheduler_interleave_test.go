package integration_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
)

// mockPagedACBClient simulates an ACB upstream responding to 31-day history and realtime polls.
type mockPagedACBClient struct {
	mu            sync.Mutex
	pagesServed   int
	realtimePolls int
	stepDelay     time.Duration
}

func (m *mockPagedACBClient) getHistoryPage(day int, page int) acb.HistoryPageResult {
	m.mu.Lock()
	m.pagesServed++
	delay := m.stepDelay
	m.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	hasNext := page < 3
	var nextAction string
	var nextFields map[string]string
	if hasNext {
		nextAction = "/acbib/Request"
		nextFields = map[string]string{
			"dse_operationName":  "ibkacctDetailProc",
			"dse_processorState": fmt.Sprintf("day%d_page%d", day, page+1),
		}
	}

	txA := fmt.Sprintf("TX_D%02d_P%d_A", day, page)
	txB := fmt.Sprintf("TX_D%02d_P%d_B", day, page)

	return acb.HistoryPageResult{
		Transactions: []acb.Transaction{
			{Number: txA, Credit: 100000},
			{Number: txB, Credit: 200000},
		},
		HasNext:    hasNext,
		NextAction: nextAction,
		NextFields: nextFields,
		TotalRows:  6,
	}
}

func (m *mockPagedACBClient) getRealtimePoll(seq int) acb.HistoryPageResult {
	m.mu.Lock()
	m.realtimePolls++
	delay := m.stepDelay
	m.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	return acb.HistoryPageResult{
		Transactions: []acb.Transaction{
			{Number: fmt.Sprintf("TX_RT_%d", seq), Credit: 50000},
		},
		HasNext:   false,
		TotalRows: 1,
	}
}

// history31DayTask implements scheduler.UpstreamTask for a 31-day range (3 pages per day = 93 pages).
type history31DayTask struct {
	client     *mockPagedACBClient
	totalDays  int
	currentDay int
	page       int
	cursor     *acb.PaginationCursor

	mu           sync.Mutex
	ingestedRows []string
	stepSequence []string
	completed    bool
}

func (h *history31DayTask) ID() string                    { return "history_31day_job" }
func (h *history31DayTask) Kind() string                  { return "FILTER_HISTORY" }
func (h *history31DayTask) Priority() scheduler.UpstreamPriority { return scheduler.PriorityHistory }
func (h *history31DayTask) Generation() int64             { return 1 }

func (h *history31DayTask) Step(ctx context.Context) (scheduler.TaskStepResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.completed {
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	res := h.client.getHistoryPage(h.currentDay, h.page)
	for _, tx := range res.Transactions {
		h.ingestedRows = append(h.ingestedRows, tx.Number)
	}
	stepTag := fmt.Sprintf("HISTORY_D%02d_P%d", h.currentDay, h.page)
	h.stepSequence = append(h.stepSequence, stepTag)

	if res.HasNext {
		h.page++
		h.cursor = &acb.PaginationCursor{
			Action:     res.NextAction,
			Fields:     res.NextFields,
			PageNumber: h.page,
		}
		// Yield after exactly one ACB page quantum
		return scheduler.TaskStepResult{Done: false, Outcome: scheduler.OutcomeSuccess}, nil
	}

	// Completed all pages for currentDay
	h.page = 1
	h.cursor = nil
	h.currentDay++

	if h.currentDay > h.totalDays {
		h.completed = true
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	// Yield for the next day's work
	return scheduler.TaskStepResult{Done: false, Outcome: scheduler.OutcomeSuccess}, nil
}

// realtimeStepTask implements a high-priority realtime poll quantum.
type realtimeStepTask struct {
	id           string
	seq          int
	client       *mockPagedACBClient
	stepSequence *[]string
	seqMu        *sync.Mutex
	executed     atomic.Bool
}

func (r *realtimeStepTask) ID() string                    { return r.id }
func (r *realtimeStepTask) Kind() string                  { return "REALTIME_POLL" }
func (r *realtimeStepTask) Priority() scheduler.UpstreamPriority { return scheduler.PriorityRealtime }
func (r *realtimeStepTask) Generation() int64             { return 1 }

func (r *realtimeStepTask) Step(ctx context.Context) (scheduler.TaskStepResult, error) {
	_ = r.client.getRealtimePoll(r.seq)
	r.executed.Store(true)

	r.seqMu.Lock()
	*r.stepSequence = append(*r.stepSequence, fmt.Sprintf("REALTIME_%d", r.seq))
	r.seqMu.Unlock()

	return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
}

// catchUpStepTask implements catch-up task with PriorityCatchUp (50).
type catchUpStepTask struct {
	id           string
	stepSequence *[]string
	seqMu        *sync.Mutex
	done         atomic.Bool
}

func (c *catchUpStepTask) ID() string                           { return c.id }
func (c *catchUpStepTask) Kind() string                         { return "CATCHUP" }
func (c *catchUpStepTask) Priority() scheduler.UpstreamPriority { return scheduler.PriorityCatchUp }
func (c *catchUpStepTask) Generation() int64                    { return 1 }

func (c *catchUpStepTask) Step(ctx context.Context) (scheduler.TaskStepResult, error) {
	c.seqMu.Lock()
	*c.stepSequence = append(*c.stepSequence, c.id)
	c.seqMu.Unlock()
	c.done.Store(true)
	return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
}

// TestSchedulerInterleave31DayHistoryAndRealtime proves Gate 1:
// A 31-day history job with multiple pages yields per quantum and allows realtime polls
// to interleave without starvation, completing with exact rows and no duplicates.
func TestSchedulerInterleave31DayHistoryAndRealtime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := &mockPagedACBClient{stepDelay: 1 * time.Millisecond}
	sched := scheduler.New(&scheduler.Options{
		QuantumTimeout: 2 * time.Second,
	})
	sched.Start(ctx)
	defer sched.Stop()

	var seqMu sync.Mutex
	var executionOrder []string

	history := &history31DayTask{
		client:     client,
		totalDays:  31,
		currentDay: 1,
		page:       1,
	}

	if err := sched.Enqueue(history); err != nil {
		t.Fatalf("failed to enqueue history: %v", err)
	}

	// Allow history to begin first quantum
	time.Sleep(10 * time.Millisecond)

	// Enqueue realtime poll while history is actively executing
	rt1 := &realtimeStepTask{
		id:           "rt_interleave_1",
		seq:          1,
		client:       client,
		stepSequence: &executionOrder,
		seqMu:        &seqMu,
	}
	if err := sched.Enqueue(rt1); err != nil {
		t.Fatalf("failed to enqueue realtime: %v", err)
	}

	// Enqueue catch-up task as well
	cu1 := &catchUpStepTask{
		id:           "catchup_1",
		stepSequence: &executionOrder,
		seqMu:        &seqMu,
	}
	if err := sched.Enqueue(cu1); err != nil {
		t.Fatalf("failed to enqueue catchup: %v", err)
	}

	// Poll until rt1 and cu1 have executed
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rt1.executed.Load() && cu1.done.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !rt1.executed.Load() {
		t.Fatal("realtime poll was starved by history job")
	}

	// Wait for history to finish all 31 days (31 * 3 = 93 pages)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		history.mu.Lock()
		done := history.completed
		history.mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	_ = sched.Stop()

	history.mu.Lock()
	defer history.mu.Unlock()

	if !history.completed {
		t.Fatalf("history job failed to complete in time; reached day %d page %d", history.currentDay, history.page)
	}

	expectedPages := 31 * 3
	if len(history.stepSequence) != expectedPages {
		t.Fatalf("expected %d history page steps, got %d", expectedPages, len(history.stepSequence))
	}

	expectedRows := 31 * 3 * 2 // 186 rows
	if len(history.ingestedRows) != expectedRows {
		t.Fatalf("expected %d rows, got %d", expectedRows, len(history.ingestedRows))
	}

	// Verify no duplicates in ingested rows
	seen := make(map[string]bool)
	for _, row := range history.ingestedRows {
		if seen[row] {
			t.Fatalf("duplicate transaction detected: %s", row)
		}
		seen[row] = true
	}

	// Verify priority ordering: realtime must execute before catchup
	seqMu.Lock()
	defer seqMu.Unlock()
	if len(executionOrder) >= 2 {
		if executionOrder[0] != "REALTIME_1" {
			t.Fatalf("expected REALTIME_1 before CATCHUP, got %v", executionOrder)
		}
	}
}
