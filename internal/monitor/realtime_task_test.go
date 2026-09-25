package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
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

func TestRealtimeTask_ContinuesBeyondForegroundPageBudget(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "rt_budget.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	client := &multiPageMockClient{maxPages: 10}
	mon := New(store, client, 5*time.Second, 15*time.Second)
	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err != nil || res.Done {
		t.Fatalf("expected foreground quantum to yield, result=%+v err=%v", res, err)
	}
	for i := 0; i < 10 && !res.Done; i++ {
		res, err = task.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !res.Done || client.pagesReturned.Load() != 10 {
		t.Fatalf("expected continuation through 10 pages, done=%v pages=%d", res.Done, client.pagesReturned.Load())
	}
	runs, err := store.ListPollRuns(ctx, 5)
	if err != nil || len(runs) != 1 || runs[0].Status != "SUCCEEDED" || runs[0].Pages != 10 {
		t.Fatalf("unexpected completed poll: %+v %v", runs, err)
	}
	txns, err := store.ListTransactions(ctx, 20)
	if err != nil || len(txns) != 10 {
		t.Fatalf("expected 10 transactions committed, got %d (%v)", len(txns), err)
	}
}

func TestRealtimeTask_NoHistoricalCatchUpAfterPageBudget(t *testing.T) {
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

	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err != nil || res.Done {
		t.Fatalf("expected first quantum to yield, result=%+v err=%v", res, err)
	}
	for i := 0; i < 8 && !res.Done; i++ {
		res, err = task.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !res.Done {
		t.Fatal("expected realtime continuation to finish")
	}
	runs, err := store.ListPollRuns(ctx, 1)
	if err != nil || len(runs) != 1 || runs[0].Status != "SUCCEEDED" {
		t.Fatalf("expected one successful poll, got %+v (%v)", runs, err)
	}
	txns, err := store.ListTransactions(ctx, 20)
	if err != nil || len(txns) != 8 {
		t.Fatalf("expected 8 realtime transactions, got %d (%v)", len(txns), err)
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

type blockingMultiPageMockClient struct {
	historyCalls atomic.Int32
	page2Started chan struct{}
	unblockPage2 chan struct{}
}

func (m *blockingMultiPageMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *blockingMultiPageMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	call := m.historyCalls.Add(1)
	if call == 1 {
		body := `
		<form action="/history" method="POST">
			<input type="hidden" name="dse_operationName" value="op1" />
			<input type="hidden" name="dse_processorState" value="ps2" />
			<input type="hidden" name="AccountNbr" value="123456" />
		</form>
		<table>
			<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
			<tr><td>TXN_PAGE1</td><td>14/09/2026</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Transfer 1</td></tr>
			<tr><td colspan="6"><a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
		</table>`
		return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
	}

	if m.page2Started != nil {
		select {
		case <-m.page2Started:
		default:
			close(m.page2Started)
		}
	}
	if m.unblockPage2 != nil {
		select {
		case <-m.unblockPage2:
		case <-ctx.Done():
			return acb.Response{StatusCode: 500}, ctx.Err()
		}
	}

	body := `
	<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="last" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>TXN_PAGE2</td><td>14/09/2026</td><td>0</td><td>200,000</td><td>1,200,000</td><td>Transfer 2</td></tr>
		<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>
	</table>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestRealtimeTask_Page1EventBeforeBlockedPage2(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "rt_p1_before_p2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}

	page2Started := make(chan struct{})
	unblockPage2 := make(chan struct{})

	client := &blockingMultiPageMockClient{
		page2Started: page2Started,
		unblockPage2: unblockPage2,
	}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	page1EventReceived := make(chan struct{})
	mon.WithEventNotifier(func(events []storage.EventNotification) {
		for _, ev := range events {
			if strings.Contains(string(ev.Payload), "TXN_PAGE1") {
				select {
				case <-page1EventReceived:
				default:
					close(page1EventReceived)
				}
			}
		}
	})

	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	errCh := make(chan error, 1)
	go func() {
		res, err := task.Step(ctx)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- res.Error
	}()

	// Wait until page 2 fetch has actually started
	select {
	case <-page2Started:
	case <-ctx.Done():
		t.Fatal("timed out waiting for page 2 fetch to start")
	}

	// While page 2 is still blocked in History, page 1 event must ALREADY have been received
	select {
	case <-page1EventReceived:
	default:
		t.Fatal("expected page 1 event to be notified immediately before page 2 unblocks")
	}

	// Verify page 1 transaction was committed to database while page 2 is blocked
	txns, err := store.ListTransactions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(txns) != 1 {
		t.Fatalf("expected exactly 1 transaction committed before page 2 unblocks, got %d", len(txns))
	}

	// Now unblock page 2
	close(unblockPage2)

	// Wait for task to complete successfully
	select {
	case stepErr := <-errCh:
		if stepErr != nil {
			t.Fatalf("unexpected step error: %v", stepErr)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for task completion")
	}

	// Both transactions committed
	txns, err = store.ListTransactions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(txns) != 2 {
		t.Fatalf("expected 2 transactions committed, got %d", len(txns))
	}

	// Poll run totals verified
	runs, err := store.ListPollRuns(ctx, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected 1 poll run, got %v (err: %v)", len(runs), err)
	}
	if runs[0].Status != "SUCCEEDED" {
		t.Fatalf("expected SUCCEEDED poll run, got %s", runs[0].Status)
	}
	if runs[0].Pages != 2 {
		t.Fatalf("expected 2 pages in poll run, got %d", runs[0].Pages)
	}
	if runs[0].RowsSeen != 2 {
		t.Fatalf("expected 2 rows seen, got %d", runs[0].RowsSeen)
	}
}

type fenceFailureMockClient struct {
	store  *storage.Store
	connID string
}

func (m *fenceFailureMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	// Mutate generation right before task parses history and calls ingest
	_, _ = m.store.DB().ExecContext(ctx, "UPDATE connections SET generation = generation + 1 WHERE id = ?", m.connID)
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *fenceFailureMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	body := `
	<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="last" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>TXN_FAIL</td><td>14/09/2026</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Transfer Fail</td></tr>
		<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>
	</table>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestRealtimeTask_NoPublishOnFailedIngest(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "rt_no_publish.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}

	var published atomic.Int32
	client := &fenceFailureMockClient{
		store:  store,
		connID: conn.ID,
	}
	mon := New(store, client, 5*time.Second, 15*time.Second)
	mon.WithEventNotifier(func(events []storage.EventNotification) {
		published.Add(int32(len(events)))
	})

	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err == nil && res.Error == nil {
		t.Fatal("expected step to fail due to generation fence mismatch")
	}

	if published.Load() != 0 {
		t.Fatalf("expected 0 events published on failed ingest, got %d", published.Load())
	}

	txns, err := store.ListTransactions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(txns) != 0 {
		t.Fatalf("expected 0 transactions inserted, got %d", len(txns))
	}

	runs, err := store.ListPollRuns(ctx, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected 1 poll run, got %v (err: %v)", len(runs), err)
	}
	if runs[0].Status != "FAILED" {
		t.Fatalf("expected FAILED poll status, got %s", runs[0].Status)
	}
}

type page2FenceFailureMockClient struct {
	historyCalls atomic.Int32
	store        *storage.Store
	connID       string
}

func (m *page2FenceFailureMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *page2FenceFailureMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	call := m.historyCalls.Add(1)
	if call == 1 {
		body := `
		<form action="/history" method="POST">
			<input type="hidden" name="dse_operationName" value="op1" />
			<input type="hidden" name="dse_processorState" value="ps2" />
			<input type="hidden" name="AccountNbr" value="123456" />
		</form>
		<table>
			<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
			<tr><td>TXN_PAGE1</td><td>14/09/2026</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Transfer 1</td></tr>
			<tr><td colspan="6"><a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
		</table>`
		return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
	}

	// Mutate generation right before page 2 ingest
	_, _ = m.store.DB().ExecContext(ctx, "UPDATE connections SET generation = generation + 1 WHERE id = ?", m.connID)

	body := `
	<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="last" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>TXN_PAGE2</td><td>14/09/2026</td><td>0</td><td>200,000</td><td>1,200,000</td><td>Transfer 2</td></tr>
		<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>
	</table>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestRealtimeTask_Page2FailedIngestDoesNotPublishPage2Event(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "rt_p2_fail_ingest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}

	var published atomic.Int32
	client := &page2FenceFailureMockClient{
		store:  store,
		connID: conn.ID,
	}
	mon := New(store, client, 5*time.Second, 15*time.Second)
	mon.WithEventNotifier(func(events []storage.EventNotification) {
		published.Add(int32(len(events)))
	})

	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err == nil && res.Error == nil {
		t.Fatal("expected step to fail due to generation fence mismatch on page 2")
	}

	// Exactly 1 event published from page 1, zero events published from failed page 2
	if published.Load() != 1 {
		t.Fatalf("expected 1 event published from page 1 and none from failed page 2, got %d", published.Load())
	}

	// Exactly 1 transaction committed
	txns, err := store.ListTransactions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(txns) != 1 {
		t.Fatalf("expected exactly 1 transaction committed from page 1, got %d", len(txns))
	}

	// Poll is partial because page 1 committed before page 2 failed.
	runs, err := store.ListPollRuns(ctx, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected 1 poll run, got %v (err: %v)", len(runs), err)
	}
		if runs[0].Status != "PARTIAL" {
			t.Fatalf("expected PARTIAL poll status, got %s", runs[0].Status)
		}
	}

func TestRealtimeTask_FinishPollUnconfirmedAuthRejected(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "rt_unconfirmed_auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}

	mon := New(store, nil, 5*time.Second, 15*time.Second)
	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	task.started = true
	poll, err := store.StartPoll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task.poll = poll

	// Calling finishPoll with AUTH_REQUIRED without AuthConfirmed proof MUST fail
	res, err := task.finishPoll(ctx, "AUTH_REQUIRED", "unconfirmed auth")
	if err == nil {
		t.Fatal("expected finishPoll to fail when AuthConfirmed is false")
	}
	if res.Done {
		t.Fatal("expected task result Done=false on transient storage error")
	}

	connAfter, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if connAfter.State != "MONITORING" {
		t.Fatalf("expected connection to remain MONITORING, got: %s", connAfter.State)
	}
	if connAfter.Generation != conn.Generation {
		t.Fatalf("expected generation %d, got %d", conn.Generation, connAfter.Generation)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestRealtimeTask_SessionResyncRecoveryDoesNotTransitionAuthRequired(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "rt_resync_recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}

	var requestCount atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count := requestCount.Add(1)
		switch count {
		case 1:
			// Poll 1: Bootstrap probe
			body := `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="token1"><input name="dse_sessionId" value="sess1"><input name="AccountNbr" value="***1234"></form>`
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		case 2:
			// Poll 1: History request -> returns valid transactions
			body := `
			<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="token1"><input name="dse_sessionId" value="sess1"><input name="AccountNbr" value="***1234"></form>
			<table>
				<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
				<tr><td>TXN_1</td><td>14/09/2026</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Transfer 1</td></tr>
				<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>
			</table>`
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		case 3:
			// Poll 2: Stale conversational state in history POST -> returns LoginPage
			loginBody := `<input name="username"><input type="password" name="password">`
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(loginBody)), Request: r}, nil
		case 4:
			// Poll 2: Probe GET /acbib/Request confirms session still alive! Returns fresh tokens
			body := `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="token2"><input name="dse_sessionId" value="sess2"><input name="AccountNbr" value="***1234"></form>`
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		case 5:
			// Poll 2: Replayed POST history with resynced token2 -> returns valid transactions
			body := `
			<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="token2"><input name="dse_sessionId" value="sess2"><input name="AccountNbr" value="***1234"></form>
			<table>
				<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
				<tr><td>TXN_2</td><td>14/09/2026</td><td>0</td><td>200,000</td><td>1,200,000</td><td>Transfer 2</td></tr>
				<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>
			</table>`
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		case 6:
			// Poll 3: History request with fresh token2 -> succeeds
			body := `
			<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="token2"><input name="dse_sessionId" value="sess2"><input name="AccountNbr" value="***1234"></form>
			<table>
				<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
				<tr><td>TXN_3</td><td>14/09/2026</td><td>0</td><td>300,000</td><td>1,500,000</td><td>Transfer 3</td></tr>
				<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>
			</table>`
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		default:
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
	})

	acbClient, err := acb.NewClient("https://online.acb.com.vn", transport)
	if err != nil {
		t.Fatal(err)
	}

	mon := New(store, acbClient, 5*time.Second, 15*time.Second)

	// Poll 1
	task1 := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res1, err := task1.Step(ctx)
	if err != nil || !res1.Done || res1.Outcome != scheduler.OutcomeSuccess {
		t.Fatalf("poll 1 failed: res=%+v err=%v", res1, err)
	}

	// Poll 2: Stale conversational state triggered resync and transparent replay
	task2 := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res2, err := task2.Step(ctx)
	if err != nil || !res2.Done || res2.Outcome != scheduler.OutcomeSuccess {
		t.Fatalf("poll 2 with resync failed: res=%+v err=%v", res2, err)
	}

	// Poll 3: Next poll succeeds directly
	task3 := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res3, err := task3.Step(ctx)
	if err != nil || !res3.Done || res3.Outcome != scheduler.OutcomeSuccess {
		t.Fatalf("poll 3 failed: res=%+v err=%v", res3, err)
	}

	// Verify invariant: State remains MONITORING, Generation is strictly UNCHANGED
	connFinal, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if connFinal.State != "MONITORING" {
		t.Fatalf("expected state to remain MONITORING, got: %s", connFinal.State)
	}
	if connFinal.Generation != conn.Generation {
		t.Fatalf("expected generation %d, got %d", conn.Generation, connFinal.Generation)
	}

		// Verify all 3 transactions were ingested
		txns, err := store.ListTransactions(ctx, 10)
		if err != nil || len(txns) != 3 {
			t.Fatalf("expected 3 transactions ingested, got %d (err: %v)", len(txns), err)
		}
	}

func TestRealtimeTask_BootstrapClassifiedAsHistoryPageSubmitsHistoryQuery(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "rt_bootstrap_history_kind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}

	todayVN := time.Now().In(time.FixedZone("Asia/Ho_Chi_Minh", 7*3600)).Format("02/01/2006")

	// Custom mock client where Bootstrap returns a page classified as HistoryPage
	// (matching real ACB bootstrap page with ibkacctDetailProc and table headers but no data).
	mockClient := &bootstrapHistoryPageMockClient{
		todayVN: todayVN,
	}

	mon := New(store, mockClient, 5*time.Second, 15*time.Second)
	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)

	res, err := task.Step(ctx)
	if err != nil {
		t.Fatalf("unexpected step error: %v", err)
	}
	runsList, _ := store.ListPollRuns(ctx, 1)
	if len(runsList) > 0 {
		t.Logf("poll run status: %s, error: %s, classifier: %s", runsList[0].Status, runsList[0].Error, runsList[0].Classifier)
	}
	if !res.Done || res.Outcome != scheduler.OutcomeSuccess {
		t.Fatalf("expected poll to succeed, got res=%+v", res)
	}

	if mockClient.historyCalls.Load() != 1 {
		t.Fatalf("expected 1 history call, got %d", mockClient.historyCalls.Load())
	}

	runs, err := store.ListPollRuns(ctx, 1)
	if err != nil || len(runs) != 1 || runs[0].Status != "SUCCEEDED" {
		t.Fatalf("expected SUCCEEDED poll run, got %+v (err: %v)", runs, err)
	}

	txns, err := store.ListTransactions(ctx, 10)
	if err != nil || len(txns) != 1 || txns[0].Description != "Realtime payment" {
		t.Fatalf("expected 1 transaction ingested, got %+v (err: %v)", txns, err)
	}
}

type bootstrapHistoryPageMockClient struct {
	bootstrapCalls atomic.Int32
	historyCalls   atomic.Int32
	todayVN        string
}

func (m *bootstrapHistoryPageMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	m.bootstrapCalls.Add(1)
	body := `
	<form action="/acbib/Request" method="POST">
		<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="dse_sessionId" value="sess1" />
		<input type="hidden" name="AccountNbr" value="" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
	</table>`
	return acb.Response{
		StatusCode: 200,
		URL:        "https://online.acb.com.vn/acbib/Request",
		Body:       body,
		Kind:       acb.HistoryPage,
	}, nil
}

func (m *bootstrapHistoryPageMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	m.historyCalls.Add(1)
	body := fmt.Sprintf(`
	<form action="/acbib/Request" method="POST">
		<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		<input type="hidden" name="dse_processorState" value="ps2" />
		<input type="hidden" name="dse_sessionId" value="sess1" />
		<input type="hidden" name="AccountNbr" value="***1234" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>TXN_RT_1</td><td>%s</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Realtime payment</td></tr>
		<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>
	</table>`, m.todayVN)
	return acb.Response{
		StatusCode: 200,
		URL:        "https://online.acb.com.vn/acbib/Request",
		Body:       body,
		Kind:       acb.HistoryPage,
	}, nil
}


