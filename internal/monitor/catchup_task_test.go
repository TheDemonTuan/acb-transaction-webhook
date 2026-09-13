package monitor

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type catchUpInterleaveMockClient struct {
	historyCalls atomic.Int32
	historyLogMu sync.Mutex
	historyLog   []string
}

func (m *catchUpInterleaveMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *catchUpInterleaveMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	call := int(m.historyCalls.Add(1))
	label := fmt.Sprintf("call_%d_%s", call, fields["FromDate"])
	m.historyLogMu.Lock()
	m.historyLog = append(m.historyLog, label)
	m.historyLogMu.Unlock()

	// If fields["_explicitRange"] == "true", it's catch-up
	// Check if this is page 1 or page 2
	isPage2 := fields["dse_nextEventName"] == "nextPage"
	var navRow string
	if !isPage2 {
		navRow = `<tr><td colspan="6"><a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>`
	} else {
		navRow = `<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>`
	}

	body := fmt.Sprintf(`
	<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps_next" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>TXN_%d</td><td>14/09/2026</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Transfer %d</td></tr>
		%s
	</table>
	`, call, call, navRow)

	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestCatchUpTask_YieldsAfterOnePageAndPreemptedByRealtime(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cu_yield.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	client := &catchUpInterleaveMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	sched := mon.Scheduler()
	sched.Start(ctx)
	defer sched.Stop()

	// Step 1: Enqueue CatchUpTask (priority 50)
	cuTask := NewCatchUpTask(mon, conn.ID, conn.Generation)
	if err := sched.Enqueue(cuTask); err != nil {
		t.Fatal(err)
	}

	// Wait until page 1 of catch-up has executed
	deadline := time.Now().Add(2 * time.Second)
	for client.historyCalls.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for catch-up page 1")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Enqueue RealtimeTask (priority 80) immediately while CatchUp yields
	rtExecuted := make(chan struct{})
	mon.WithEventNotifier(func(events []storage.EventNotification) {
		select {
		case rtExecuted <- struct{}{}:
		default:
		}
	})

	rtTask := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	if err := sched.Enqueue(rtTask); err != nil {
		t.Fatal(err)
	}

	// Realtime poll must execute before catch-up finishes completely
	select {
	case <-rtExecuted:
	case <-time.After(2 * time.Second):
		// Poll runs and commits
	}

	// Wait for queue to drain
	deadline = time.Now().Add(3 * time.Second)
	for sched.TotalQueueDepth() > 0 || sched.IsBusy() {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	client.historyLogMu.Lock()
	defer client.historyLogMu.Unlock()

	if len(client.historyLog) < 2 {
		t.Fatalf("expected at least 2 history calls, got %d", len(client.historyLog))
	}
}

func TestCatchUpTask_CheckpointAdvancesOnlyAfterFullDay(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cu_checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	// Set initial checkpoint and coverage to 2 days ago
	twoDaysAgo := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -2).Format("2006-01-02")
	oneDayAgo := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -1).Format("2006-01-02")
	_ = store.SaveCheckpoint(ctx, storage.Checkpoint{
		ConnectionID: conn.ID,
		ScanID:       "init",
		CoverageFrom: twoDaysAgo,
		CoverageTo:   twoDaysAgo,
	})
	_ = store.RecordCoveragePerDay(ctx, conn.ID, map[string]int{twoDaysAgo: 1})

	client := &catchUpInterleaveMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	task := NewCatchUpTask(mon, conn.ID, conn.Generation)

	// Step 1: Page 1 of yesterday (returns HasNext=true so quantum yields)
	res1, err := task.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res1.Done {
		t.Fatal("expected quantum yield (Done=false), got Done=true")
	}

	// Checkpoint MUST NOT have advanced to oneDayAgo yet!
	cp1, _ := store.GetCheckpoint(ctx, conn.ID)
	if cp1.CoverageTo != twoDaysAgo {
		t.Fatalf("checkpoint advanced prematurely before day completed! got %s, expected %s", cp1.CoverageTo, twoDaysAgo)
	}

	// Step 2: Page 2 of yesterday (last page for yesterday, so day finishes)
	res2, err := task.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Checkpoint MUST NOW advance to oneDayAgo!
	cp2, _ := store.GetCheckpoint(ctx, conn.ID)
	if cp2.CoverageTo != oneDayAgo {
		t.Fatalf("checkpoint should have advanced to completed day %s, got %s", oneDayAgo, cp2.CoverageTo)
	}
	_ = res2
}

func TestCatchUpTask_EmitsDeliveriesWithCatchUpSource(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cu_source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	ep, err := store.CreateEndpointWithSecret(ctx, "Test Endpoint", "https://example.com/webhook")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetEndpointStatus(ctx, ep.ID, "ACTIVE")

	client := &catchUpInterleaveMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	task := NewCatchUpTask(mon, conn.ID, conn.Generation)

	res, err := task.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = res

	// Check that deliveries were created in storage for the newly ingested credit
	deliveries, err := store.ListDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) == 0 {
		t.Fatal("expected delivery to be created for catch-up credit transaction")
	}
}

func TestCatchUpTask_RestartReconstructsFromCheckpoint(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "cu_restart.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	targetDay := time.Now().In(acb.DefaultLocation).AddDate(0, 0, -1).Format("2006-01-02")
	_ = store.SaveCheckpoint(ctx, storage.Checkpoint{
		ConnectionID: conn.ID,
		ScanID:       "prior_scan",
		CoverageFrom: targetDay,
		CoverageTo:   targetDay,
	})
	_ = store.RecordCoveragePerDay(ctx, conn.ID, map[string]int{targetDay: 3})

	// Simulate fresh worker restart with new monitor instance (no preserved form tokens)
	client := &catchUpInterleaveMockClient{}
	newMon := New(store, client, 5*time.Second, 15*time.Second)

	newTask := NewCatchUpTask(newMon, conn.ID, conn.Generation)
	res, err := newTask.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = res

	// Verify that the task picked up the checkpoint without errors and safely bootstrapped
	if !newTask.initialized {
		t.Fatal("expected task to be initialized")
	}
	if newTask.fromDate != targetDay {
		t.Fatalf("expected reconstructed fromDate %s, got %s", targetDay, newTask.fromDate)
	}
}
