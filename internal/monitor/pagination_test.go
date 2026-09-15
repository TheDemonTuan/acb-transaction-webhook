package monitor

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type pagedMockClient struct {
	historyCalls atomic.Int32
	failOnPage2  bool
}

func (m *pagedMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *pagedMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	call := m.historyCalls.Add(1)
	if call == 1 {
		// Page 1 with active "Trang sau" link to page 2
		body := `<form action="/history" method="POST">
			<input type="hidden" name="dse_operationName" value="op1" />
			<input type="hidden" name="dse_processorState" value="ps2" />
			<input type="hidden" name="AccountNbr" value="123456" />
		</form>
		<table>
			<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
			<tr><td>PAGE1_TX1</td><td>12/09/2026</td><td>0</td><td>50,000</td><td>100,000</td><td>Transfer 1</td></tr>
			<tr><td colspan="6"><a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
		</table>`
		return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
	}

	if m.failOnPage2 {
		return acb.Response{StatusCode: 500}, context.DeadlineExceeded
	}

	// Page 2 (last page, no next link)
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="last" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>PAGE2_TX2</td><td>12/09/2026</td><td>0</td><td>75,000</td><td>175,000</td><td>Transfer 2</td></tr>
		<tr><td colspan="6"><span class="disabled">Trang sau</span></td></tr>
	</table>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestEnsureHistoryMultiPageFullSync(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_paged.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connID := "conn_paged"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatal(err)
	}

	client := &pagedMockClient{failOnPage2: false}
	mon := New(store, client, 5*time.Second, 15*time.Second)
	runner := NewHistoryJobRunner(store, client, mon.Scheduler(), nil)

	_, _, err = store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-12")
	if err != nil {
		t.Fatalf("CreateOrGetHistorySyncJob: %v", err)
	}
	processed, err := runner.ProcessNextJob(ctx)
	if err != nil {
		t.Fatalf("ProcessNextJob multi-page failed: %v", err)
	}
	if !processed {
		t.Fatal("expected job to be processed")
	}
	if client.historyCalls.Load() != 2 {
		t.Fatalf("expected 2 history calls for 2 pages, got %d", client.historyCalls.Load())
	}

	// Verify coverage is marked COMPLETE after successful full multi-page sync
	covered, err := store.CheckRangeCoverage(ctx, connID, "2026-09-12", "2026-09-12")
	if err != nil || !covered {
		t.Fatalf("expected range to be covered after complete sync: covered=%v, err=%v", covered, err)
	}
}

func TestEnsureHistoryIncompleteWithholdsCoverage(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_paged_fail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connID := "conn_paged_fail"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatal(err)
	}

	client := &pagedMockClient{failOnPage2: true}
	mon := New(store, client, 5*time.Second, 15*time.Second)
	runner := NewHistoryJobRunner(store, client, mon.Scheduler(), nil)

	_, _, err = store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-12")
	if err != nil {
		t.Fatalf("CreateOrGetHistorySyncJob: %v", err)
	}
	_, _ = runner.ProcessNextJob(ctx)

	// Verify coverage was NOT marked COMPLETE
	covered, err := store.CheckRangeCoverage(ctx, connID, "2026-09-12", "2026-09-12")
	if err != nil {
		t.Fatal(err)
	}
	if covered {
		t.Fatal("coverage MUST NOT be recorded as COMPLETE when history sync is incomplete!")
	}
}

func TestCatchUpIncompleteWithholdsCheckpoint(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_catchup_fail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connID := "conn_catchup_fail"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatal(err)
	}

	// Set initial checkpoint CoverageTo = 2026-09-10
	if err := store.SaveCheckpoint(ctx, storage.Checkpoint{
		ConnectionID: connID,
		ScanID:       "init",
		CoverageFrom: "2026-09-08",
		CoverageTo:   "2026-09-10",
	}); err != nil {
		t.Fatal(err)
	}

	// Client has next page, but fails on page 2
	client := &pagedMockClient{failOnPage2: true}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	_ = mon.catchUp(ctx)

	// Verify checkpoint was NOT advanced to today
	cp, err := store.GetCheckpoint(ctx, connID)
	if err != nil || cp == nil {
		t.Fatalf("expected checkpoint to exist, err=%v", err)
	}
	if cp.CoverageTo != "2026-09-10" {
		t.Fatalf("checkpoint MUST NOT be advanced when catch-up is incomplete, got: %s", cp.CoverageTo)
	}
	if !mon.catchUpPending {
		t.Fatal("catchUpPending must remain true after incomplete catch-up")
	}
}

type globalTotalMockClient struct {
	calls atomic.Int32
}

func (m *globalTotalMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *globalTotalMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	call := m.calls.Add(1)
	if call == 1 {
		body := `<form action="/history" method="POST">
			<input type="hidden" name="dse_operationName" value="op1" />
			<input type="hidden" name="dse_processorState" value="ps2" />
		</form>
		<table>
			<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th></tr>
			<tr><td>GLOBAL_TX1</td><td>12/09/2026</td><td>0</td><td>10.000</td></tr>
			<tr><td colspan="4"><a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
		</table>
		<div>Tổng số dòng: 2</div>`
		return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
	}

	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="last" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th></tr>
		<tr><td>GLOBAL_TX2</td><td>12/09/2026</td><td>0</td><td>20.000</td></tr>
		<tr><td colspan="4"><span class="disabled">Trang sau</span></td></tr>
	</table>
	<div>Tổng số dòng: 2</div>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestEnsureHistoryGlobalTotalRowsNotTruncated(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_global_total.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connID := "conn_global_total"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatal(err)
	}

	client := &globalTotalMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)
	runner := NewHistoryJobRunner(store, client, mon.Scheduler(), nil)

	_, _, err = store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-12")
	if err != nil {
		t.Fatalf("CreateOrGetHistorySyncJob: %v", err)
	}
	processed, err := runner.ProcessNextJob(ctx)
	if err != nil {
		t.Fatalf("expected runner to succeed when cumulative rows == global total, got err: %v", err)
	}
	if !processed {
		t.Fatal("expected job to be processed")
	}

	covered, err := store.CheckRangeCoverage(ctx, connID, "2026-09-12", "2026-09-12")
	if err != nil || !covered {
		t.Fatalf("expected range to be marked covered: covered=%v, err=%v", covered, err)
	}
}

func TestRealtimePollPartialOnPage2Failure(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_realtime_partial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connID := "conn_realtime_partial"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatal(err)
	}

	client := &pagedMockClient{failOnPage2: true}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	var finishedPoll storage.PollRun
	mon.WithPollNotifier(func(poll storage.PollRun, insertedCount int) {
		finishedPoll = poll
	})

	err = mon.pollOnce(ctx, nil)
	if err != nil {
		t.Fatalf("expected pollOnce to succeed ingesting page 1, got: %v", err)
	}

	if finishedPoll.Status != "PARTIAL" {
		t.Fatalf("expected poll status PARTIAL when page 2 fails, got %s", finishedPoll.Status)
	}
	if finishedPoll.RowsSeen != 1 {
		t.Fatalf("expected 1 row seen from page 1, got %d", finishedPoll.RowsSeen)
	}
}

type multiDayFailMockClient struct {
	failOnDate string
}

func (m *multiDayFailMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *multiDayFailMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	date := fields["FromDate"]
	if m.failOnDate != "" && date == m.failOnDate {
		return acb.Response{StatusCode: 500}, context.DeadlineExceeded
	}
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th></tr>
		<tr><td>TX_` + date + `</td><td>` + date + `</td><td>0</td><td>10.000</td></tr>
	</table>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestProgressiveCatchUpAdvancesCheckpointPerDay(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_prog_catchup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connID := "conn_prog_catchup"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatal(err)
	}

	loc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	nowInTz := time.Now().In(loc)
	yesterday := nowInTz.AddDate(0, 0, -1).Format("2006-01-02")
	beforeYesterday := nowInTz.AddDate(0, 0, -2).Format("2006-01-02")

	// Initial checkpoint: CoverageTo = beforeYesterday
	if err := store.SaveCheckpoint(ctx, storage.Checkpoint{
		ConnectionID: connID,
		ScanID:       "init",
		CoverageFrom: nowInTz.AddDate(0, 0, -4).Format("2006-01-02"),
		CoverageTo:   beforeYesterday,
	}); err != nil {
		t.Fatal(err)
	}

	todayDate := nowInTz.Format("02/01/2006")
	client := &multiDayFailMockClient{failOnDate: todayDate}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	err = mon.catchUp(ctx)
	if err == nil {
		t.Fatalf("expected catchUp to fail on %s", todayDate)
	}

	// Verify that yesterday WAS completed and checkpoint was progressively advanced to yesterday!
	cp, err := store.GetCheckpoint(ctx, connID)
	if err != nil || cp == nil {
		t.Fatalf("expected checkpoint to exist, err=%v", err)
	}
	if cp.CoverageTo != yesterday {
		t.Fatalf("expected checkpoint to progressively advance to %s, got: %s", yesterday, cp.CoverageTo)
	}

	// Verify coverage was recorded for yesterday
	covered, err := store.CheckRangeCoverage(ctx, connID, yesterday, yesterday)
	if err != nil || !covered {
		t.Fatalf("expected %s to be covered, got covered=%v, err=%v", yesterday, covered, err)
	}

	// Now fix 12/09/2026 and run catchUp again after the transient backoff.
	client.failOnDate = ""
	mon.ClearBackoff()
	err = mon.catchUp(ctx)
	if err != nil {
		t.Fatalf("expected second catchUp to succeed, got: %v", err)
	}

	// Verify checkpoint advanced to today and catchUpPending is false
	todayISO := nowInTz.Format("2006-01-02")
	cpAfter, err := store.GetCheckpoint(ctx, connID)
	if err != nil || cpAfter == nil {
		t.Fatalf("expected checkpoint to exist, err=%v", err)
	}
	if cpAfter.CoverageTo != todayISO {
		t.Fatalf("expected checkpoint to reach %s, got: %s", todayISO, cpAfter.CoverageTo)
	}
	if mon.catchUpPending {
		t.Fatal("expected catchUpPending to be false after complete catchUp")
	}
}

type emptyNavMockClient struct{}

func (m *emptyNavMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *emptyNavMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	body := `<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th></tr>
		<tr><td>TX_EMPTY_NAV</td><td>12/09/2026</td><td>0</td><td>10.000</td></tr>
		<tr><td colspan="4"><a href="javascript:void(0)" onclick="someNav()">Trang sau</a></td></tr>
	</table>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestRealtimePollPartialWhenHasNextWithEmptyNavigation(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_realtime_empty_nav.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connID := "conn_empty_nav"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatal(err)
	}

	client := &emptyNavMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	var finishedPoll storage.PollRun
	mon.WithPollNotifier(func(poll storage.PollRun, insertedCount int) {
		finishedPoll = poll
	})

	err = mon.pollOnce(ctx, nil)
	if err != nil {
		t.Fatalf("expected pollOnce to succeed ingesting current transactions, got: %v", err)
	}

	if finishedPoll.Status != "PARTIAL" {
		t.Fatalf("expected poll status PARTIAL when navigation is empty, got %s", finishedPoll.Status)
	}
	if finishedPoll.RowsSeen != 1 {
		t.Fatalf("expected 1 row seen, got %d", finishedPoll.RowsSeen)
	}
}

type emptyNextActionMockClient struct {
	calls atomic.Int32
}

func (m *emptyNextActionMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/acbib/Request" method="POST">
		<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		<input type="hidden" name="dse_processorState" value="first" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *emptyNextActionMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	call := m.calls.Add(1)
	if call == 1 {
		// Page 1: NextAction is empty, but NextFields contains next event
		body := `<form action="/acbib/Request" method="POST">
			<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
			<input type="hidden" name="AccountNbr" value="123456" />
			<input type="hidden" name="dse_processorState" value="page2" />
		</form>
		<table>
			<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th></tr>
			<tr><td>P1_TX</td><td>12/09/2026</td><td>0</td><td>10.000</td></tr>
			<tr><td colspan="4"><a href="#" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
		</table>`
		return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
	}
	// Page 2: Last page
	body := `<form action="/acbib/Request" method="POST">
		<input type="hidden" name="dse_operationName" value="ibkacctDetailProc" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th></tr>
		<tr><td>P2_TX</td><td>12/09/2026</td><td>0</td><td>20.000</td></tr>
		<tr><td colspan="4"><span class="disabled">Trang sau</span></td></tr>
	</table>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestEnsureHistoryEmptyNextActionContinuesWithNextFields(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_empty_action.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connID := "conn_empty_action"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatal(err)
	}

	client := &emptyNextActionMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)
	runner := NewHistoryJobRunner(store, client, mon.Scheduler(), nil)

	_, _, err = store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-12")
	if err != nil {
		t.Fatalf("CreateOrGetHistorySyncJob: %v", err)
	}
	processed, err := runner.ProcessNextJob(ctx)
	if err != nil {
		t.Fatalf("ProcessNextJob failed: %v", err)
	}
	if !processed {
		t.Fatal("expected job to be processed")
	}
	if client.calls.Load() != 2 {
		t.Fatalf("expected 2 calls (page 1 and page 2), got %d", client.calls.Load())
	}
	covered, err := store.CheckRangeCoverage(ctx, connID, "2026-09-12", "2026-09-12")
	if err != nil || !covered {
		t.Fatalf("expected range to be covered after 2 pages, got: %v", covered)
	}
}

type truncatedMockClient struct{}

func (m *truncatedMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	body := `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.AccountDetailPage}, nil
}

func (m *truncatedMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	// Reports 50 total rows but only returns 1 row and next is disabled
	body := `<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th></tr>
		<tr><td>TX_TRUNC</td><td>12/09/2026</td><td>0</td><td>10.000</td></tr>
		<tr><td colspan="4"><span class="disabled">Trang sau</span></td></tr>
	</table>
	<div>Tổng số dòng: 50</div>`
	return acb.Response{StatusCode: 200, Body: body, Kind: acb.HistoryPage}, nil
}

func TestEnsureHistoryTruncationFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_trunc_fail_closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connID := "conn_trunc_fc"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatal(err)
	}

	client := &truncatedMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)
	runner := NewHistoryJobRunner(store, client, mon.Scheduler(), nil)

	job, _, err := store.CreateOrGetHistorySyncJob(ctx, connID, 1, "2026-09-12", "2026-09-12")
	if err != nil {
		t.Fatalf("CreateOrGetHistorySyncJob: %v", err)
	}
	_, err = runner.ProcessNextJob(ctx)
	if err == nil {
		t.Fatal("expected runner to return error on truncated history, got nil")
	}

	// Verify job was marked FAILED with TRUNCATED_HISTORY
	updatedJob, err := store.GetHistorySyncJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedJob.Status != "FAILED" || updatedJob.ErrorCode != "TRUNCATED_HISTORY" {
		t.Fatalf("expected job status FAILED / TRUNCATED_HISTORY, got: %s / %s", updatedJob.Status, updatedJob.ErrorCode)
	}

	// Verify coverage was NOT recorded
	covered, err := store.CheckRangeCoverage(ctx, connID, "2026-09-12", "2026-09-12")
	if err != nil {
		t.Fatal(err)
	}
	if covered {
		t.Fatal("coverage MUST NOT be recorded when pagination is truncated")
	}
}
