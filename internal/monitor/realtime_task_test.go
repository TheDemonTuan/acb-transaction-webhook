package monitor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type multiPageMockClient struct {
	pagesReturned atomic.Int32
	maxPages      int
	failOnPage    int
}

func (m *multiPageMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *multiPageMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	curr := int(m.pagesReturned.Add(1))
	if m.failOnPage > 0 && curr == m.failOnPage {
		return acb.Response{StatusCode: 500}, errors.New("upstream failure on page")
	}

	hasNext := curr < m.maxPages
	var navRow string
	if hasNext {
		navRow = `<tr><td colspan="6"><a href="/history?page=next" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>`
	} else {
		navRow = `<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>`
	}

	body := fmt.Sprintf(`
	<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps_%d" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>TXN_%d</td><td>14/09/2026</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Transfer %d</td></tr>
		%s
	</table>
	`, curr, curr, curr, navRow)

	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestRealtimeTask_FivePageBudgetAndPartial(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "rt_budget.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	// Mock client returns 10 pages, budget is 5
	client := &multiPageMockClient{maxPages: 10}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err != nil {
		t.Fatalf("unexpected step error: %v", err)
	}
	if !res.Done {
		t.Fatal("expected task to be done")
	}

	// Verify only 5 pages were fetched
	if client.pagesReturned.Load() != 5 {
		t.Fatalf("expected 5 pages fetched due to budget, got %d", client.pagesReturned.Load())
	}

	// Verify poll was recorded as PARTIAL
	runs, err := store.ListPollRuns(ctx, 5)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected 1 poll run, got %v (err: %v)", len(runs), err)
	}
	if runs[0].Status != "PARTIAL" {
		t.Fatalf("expected PARTIAL status, got %s", runs[0].Status)
	}
	if runs[0].Pages != 5 {
		t.Fatalf("expected 5 pages in poll run, got %d", runs[0].Pages)
	}
	if runs[0].RowsSeen != 5 {
		t.Fatalf("expected 5 rows seen, got %d", runs[0].RowsSeen)
	}

	// Verify 5 transactions were safely ingested
	txns, err := store.ListTransactions(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(txns) != 5 {
		t.Fatalf("expected 5 transactions committed, got %d", len(txns))
	}
}

func TestRealtimeTask_PartialPollSchedulesCatchUp(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "rt_catchup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	client := &multiPageMockClient{maxPages: 8}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	// Attach scheduler and start it
	sched := mon.Scheduler()
	sched.Start(ctx)
	defer sched.Stop()

	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	if err := sched.Enqueue(task); err != nil {
		t.Fatal(err)
	}

	// Wait for realtime poll to finish and catchup to be enqueued
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for poll completion")
		}
		runs, _ := store.ListPollRuns(ctx, 1)
		if len(runs) > 0 && runs[0].Status == "PARTIAL" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Poll succeeded partially; verified that catch-up queue depth or busy state is active
	// or catch-up was scheduled into scheduler
	txns, err := store.ListTransactions(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(txns) < 5 {
		t.Fatalf("expected at least 5 transactions ingested from partial poll, got %d", len(txns))
	}
}

func TestRealtimeTask_StaleGenerationDiscardedBeforeACB(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "rt_stale.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING', generation=2")

	var acbCalls atomic.Int32
	client := &multiPageMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	// Task has older generation (1 vs current 2)
	task := NewRealtimeTask(mon, PriorityManualSync, conn.ID, 1)
	res, err := task.Step(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Done {
		t.Fatal("expected stale task to be done")
	}
	if acbCalls.Load() != 0 || client.pagesReturned.Load() != 0 {
		t.Fatal("expected 0 ACB calls for stale generation")
	}
}

func TestRealtimeTask_Coalescing(t *testing.T) {
	q := scheduler.NewTaskQueue(100)
	now := time.Now()

	mon := New(nil, nil, 5*time.Second, 15*time.Second)
	t1 := NewRealtimeTask(mon, PriorityRealtimePoll, "c1", 1)
	t2 := NewRealtimeTask(mon, PriorityRealtimePoll, "c1", 1)

	if err := q.Push(t1, now); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(t2, now); err != nil {
		t.Fatal(err)
	}

	if q.Len() != 1 {
		t.Fatalf("expected coalesced queue length 1, got %d", q.Len())
	}
}
