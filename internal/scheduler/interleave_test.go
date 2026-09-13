package scheduler

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
)

// interleavedPagedClient simulates upstream ACB responses for both History and Realtime endpoints.
type interleavedPagedClient struct {
	mu           sync.Mutex
	historyPages int
	realtimePolls int
}

func (c *interleavedPagedClient) getHistoryPage(page int) acb.HistoryPageResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.historyPages++

	hasNext := page < 4
	var nextAction string
	var nextFields map[string]string
	if hasNext {
		nextAction = "/acbib/Request"
		nextFields = map[string]string{
			"dse_operationName":  "ibkacctDetailProc",
			"dse_processorState": fmt.Sprintf("histPage%d", page+1),
			"dse_nextEventName":  "nextPage",
			"_raw":               "true",
		}
	}

	tx1 := fmt.Sprintf("TX_H_%d_A", page)
	tx2 := fmt.Sprintf("TX_H_%d_B", page)

	return acb.HistoryPageResult{
		Transactions: []acb.Transaction{
			{Number: tx1, Credit: 100000},
			{Number: tx2, Credit: 200000},
		},
		HasNext:    hasNext,
		NextAction: nextAction,
		NextFields: nextFields,
		TotalRows:  8,
	}
}

func (c *interleavedPagedClient) getRealtimePoll(pollSeq int) acb.HistoryPageResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.realtimePolls++

	return acb.HistoryPageResult{
		Transactions: []acb.Transaction{
			{Number: fmt.Sprintf("TX_RT_%d", pollSeq), Credit: 50000},
		},
		HasNext:   false,
		TotalRows: 1,
	}
}

// historyQuantumTask performs exactly one page of history fetch per Step() quantum.
type historyQuantumTask struct {
	client     *interleavedPagedClient
	cursor     *acb.PaginationCursor
	ingested   []string
	ingestedMu sync.Mutex
	done       bool
}

func (h *historyQuantumTask) ID() string                { return "history_durable_job_1" }
func (h *historyQuantumTask) Kind() string              { return "FILTER_HISTORY" }
func (h *historyQuantumTask) Priority() UpstreamPriority { return PriorityHistory }
func (h *historyQuantumTask) Generation() int64         { return 1 }

func (h *historyQuantumTask) Step(ctx context.Context) (TaskStepResult, error) {
	// Execute at most 1 page per quantum
	nextPage := h.cursor.PageNumber + 1
	pageResult := h.client.getHistoryPage(nextPage)

	h.ingestedMu.Lock()
	for _, txn := range pageResult.Transactions {
		h.ingested = append(h.ingested, txn.Number)
	}
	h.ingestedMu.Unlock()

	h.cursor.Step(pageResult, len(pageResult.Transactions))

	if h.cursor.Complete {
		h.done = true
		return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
	}

	// Yield after 1 page so pending higher-priority realtime tasks can execute
	return TaskStepResult{Done: false, Outcome: OutcomeSuccess}, nil
}

// realtimePollTask performs a realtime transaction check.
type realtimePollTask struct {
	client     *interleavedPagedClient
	pollSeq    int
	ingested   *[]string
	ingestedMu *sync.Mutex
}

func (r *realtimePollTask) ID() string                { return fmt.Sprintf("rt_poll_%d", r.pollSeq) }
func (r *realtimePollTask) Kind() string              { return "REALTIME_POLL" }
func (r *realtimePollTask) Priority() UpstreamPriority { return PriorityRealtime }
func (r *realtimePollTask) Generation() int64         { return int64(r.pollSeq) }

func (r *realtimePollTask) Step(ctx context.Context) (TaskStepResult, error) {
	pageResult := r.client.getRealtimePoll(r.pollSeq)

	r.ingestedMu.Lock()
	for _, txn := range pageResult.Transactions {
		*r.ingested = append(*r.ingested, txn.Number)
	}
	r.ingestedMu.Unlock()

	return TaskStepResult{Done: true, Outcome: OutcomeSuccess}, nil
}

func TestScheduler_InterleavedHistoryAndRealtime_NoSkipNoDuplicate(t *testing.T) {
	sched := New(&Options{
		QuantumTimeout: 2 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sched.Start(ctx)
	defer sched.Stop()

	client := &interleavedPagedClient{}
	cursor := acb.NewPaginationCursor("/acbib/Request", map[string]string{
		"dse_operationName": "ibkacctDetailProc",
	})

	histTask := &historyQuantumTask{
		client: client,
		cursor: cursor,
	}

	var rtIngested []string
	var rtIngestedMu sync.Mutex

	// Start history task (4 pages total)
	if err := sched.Enqueue(histTask); err != nil {
		t.Fatalf("enqueue history task failed: %v", err)
	}

	// Concurrently enqueue 3 realtime polls while history is running its quanta
	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		wg.Add(1)
		pollNum := i
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(pollNum*5) * time.Millisecond)
			rtTask := &realtimePollTask{
				client:     client,
				pollSeq:    pollNum,
				ingested:   &rtIngested,
				ingestedMu: &rtIngestedMu,
			}
			_ = sched.Enqueue(rtTask)
		}()
	}

	wg.Wait()

	// Wait for all tasks to complete
	deadline := time.Now().Add(5 * time.Second)
	for sched.TotalQueueDepth() > 0 || sched.IsBusy() || !histTask.done {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for interleaved scheduler execution")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Verify History transactions: exactly 8 transactions, no skips, no duplicates
	expectedHistory := []string{
		"TX_H_1_A", "TX_H_1_B",
		"TX_H_2_A", "TX_H_2_B",
		"TX_H_3_A", "TX_H_3_B",
		"TX_H_4_A", "TX_H_4_B",
	}

	histTask.ingestedMu.Lock()
	actualHistory := histTask.ingested
	histTask.ingestedMu.Unlock()

	if len(actualHistory) != len(expectedHistory) {
		t.Fatalf("history row count mismatch: expected %d, got %d (%v)", len(expectedHistory), len(actualHistory), actualHistory)
	}
	for i, exp := range expectedHistory {
		if actualHistory[i] != exp {
			t.Fatalf("history row %d mismatch: expected %s, got %s", i, exp, actualHistory[i])
		}
	}

	// Verify Realtime transactions: exactly 3 polls completed, all distinct
	rtIngestedMu.Lock()
	defer rtIngestedMu.Unlock()
	if len(rtIngested) != 3 {
		t.Fatalf("realtime count mismatch: expected 3, got %d (%v)", len(rtIngested), rtIngested)
	}
	seenRT := make(map[string]bool)
	for _, tx := range rtIngested {
		if seenRT[tx] {
			t.Fatalf("duplicate realtime transaction found: %s", tx)
		}
		seenRT[tx] = true
	}
}
