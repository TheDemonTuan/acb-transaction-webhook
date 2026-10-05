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

func newRealtimeRegressionStore(t *testing.T, ctx context.Context) (*storage.Store, storage.Connection) {
	t.Helper()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "realtime_regression.db"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	return store, conn
}

func fixedRealtimeTime() time.Time {
	return time.Date(2026, 9, 22, 12, 0, 0, 0, acb.DefaultLocation)
}

type rolloverRegressionClient struct {
	historyCalls atomic.Int32
	onFirstPage  func()
}

func (m *rolloverRegressionClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	return acb.Response{StatusCode: 200, Body: `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps1" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>`, Kind: acb.AccountDetailPage}, nil
}

func (m *rolloverRegressionClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	if m.historyCalls.Add(1) != 1 {
		return acb.Response{StatusCode: 500}, errors.New("unexpected request after realtime rollover")
	}
	if m.onFirstPage != nil {
		m.onFirstPage()
	}
	return acb.Response{StatusCode: 200, Body: `<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="ps2" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>ROLLOVER_TX1</td><td>22/09/2026</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Transfer 1</td></tr>
		<tr><td colspan="6"><a href="/history?page=2" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
	</table>`, Kind: acb.HistoryPage}, nil
}

func TestRealtimeTask_DateRolloverStopsOldCursor(t *testing.T) {
	ctx := context.Background()
	store, conn := newRealtimeRegressionStore(t, ctx)
	defer store.Close()

	current := time.Date(2026, 9, 22, 23, 59, 59, 0, acb.DefaultLocation)
	client := &rolloverRegressionClient{}
	client.onFirstPage = func() { current = current.AddDate(0, 0, 1) }
	mon := New(store, client, 5*time.Second, 15*time.Second)
	mon.now = func() time.Time { return current }

	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done {
		t.Fatal("expected rollover to finish the old realtime chain")
	}
	if client.historyCalls.Load() != 1 {
		t.Fatalf("expected no old-day request after rollover, got %d history calls", client.historyCalls.Load())
	}

	runs, err := store.ListPollRuns(ctx, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected one poll run, got %+v (%v)", runs, err)
	}
	if runs[0].Status != "PARTIAL" || runs[0].Error != acb.ErrRealtimeDateRollover.Error() {
		t.Fatalf("expected rollover partial poll, got status=%q error=%q", runs[0].Status, runs[0].Error)
	}
	if runs[0].Pages != 1 || runs[0].RowsSeen != 0 {
		t.Fatalf("rollover records the attempted page without parsing rows, got pages=%d rows=%d", runs[0].Pages, runs[0].RowsSeen)
	}
	txns, err := store.ListTransactions(ctx, 10)
	if err != nil || len(txns) != 0 {
		t.Fatalf("response received after rollover must not be ingested: count=%d err=%v", len(txns), err)
	}
	currentConn, err := store.Connection(ctx)
	if err != nil || currentConn.State != "MONITORING" || currentConn.Generation != conn.Generation || mon.IsBackoffActive() {
		t.Fatalf("rollover must preserve session state without backoff: conn=%+v err=%v", currentConn, err)
	}
}

type repeatedCursorRegressionClient struct {
	historyCalls atomic.Int32
}

func (m *repeatedCursorRegressionClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	return (&multiPageMockClient{}).Bootstrap(ctx)
}

func (m *repeatedCursorRegressionClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	page := m.historyCalls.Add(1)
	return acb.Response{StatusCode: 200, Body: fmt.Sprintf(`<form action="/history" method="POST">
		<input type="hidden" name="dse_operationName" value="op1" />
		<input type="hidden" name="dse_processorState" value="same" />
		<input type="hidden" name="AccountNbr" value="123456" />
	</form>
	<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th><th>Số dư</th><th>Nội dung giao dịch</th></tr>
		<tr><td>REPEAT_TX%d</td><td>22/09/2026</td><td>0</td><td>100,000</td><td>1,000,000</td><td>Transfer %d</td></tr>
		<tr><td colspan="6"><a href="/history?page=next" onclick="submitEvent('nextPage')">Trang sau</a></td></tr>
	</table>`, page, page), Kind: acb.HistoryPage}, nil
}

func TestRealtimeTask_RepeatedCursorStopsPartial(t *testing.T) {
	ctx := context.Background()
	store, conn := newRealtimeRegressionStore(t, ctx)
	defer store.Close()

	client := &repeatedCursorRegressionClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)
	mon.now = fixedRealtimeTime
	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done {
		t.Fatal("expected repeated cursor to terminate the poll")
	}
	if client.historyCalls.Load() != 2 {
		t.Fatalf("expected repeated cursor to stop after two pages, got %d calls", client.historyCalls.Load())
	}

	runs, err := store.ListPollRuns(ctx, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected one poll run, got %+v (%v)", runs, err)
	}
	if runs[0].Status != "PARTIAL" || runs[0].Error != "pagination unavailable: cursor did not progress" {
		t.Fatalf("expected repeated-cursor partial result, got status=%q error=%q", runs[0].Status, runs[0].Error)
	}
}

func TestRealtimeTask_HardCapsAtTwentyPages(t *testing.T) {
	ctx := context.Background()
	store, conn := newRealtimeRegressionStore(t, ctx)
	defer store.Close()

	client := &multiPageMockClient{maxPages: 25}
	mon := New(store, client, 5*time.Second, 15*time.Second)
	mon.now = fixedRealtimeTime
	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err != nil || res.Done {
		t.Fatalf("expected foreground quantum to yield, result=%+v err=%v", res, err)
	}
	for i := 0; i < 30 && !res.Done; i++ {
		res, err = task.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !res.Done {
		t.Fatal("expected 20-page cap to finish the poll")
	}
	if client.pagesReturned.Load() != realtimeMaxPages {
		t.Fatalf("expected exactly %d requests, got %d", realtimeMaxPages, client.pagesReturned.Load())
	}

	runs, err := store.ListPollRuns(ctx, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected one poll run, got %+v (%v)", runs, err)
	}
	if runs[0].Status != "PARTIAL" || runs[0].Error != "PARTIAL_PAGE_BUDGET_REACHED" || runs[0].Pages != realtimeMaxPages {
		t.Fatalf("expected capped partial poll, got status=%q error=%q pages=%d", runs[0].Status, runs[0].Error, runs[0].Pages)
	}
}

type realtimeTruncatedRegressionClient struct{}

func (m *realtimeTruncatedRegressionClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	return (&multiPageMockClient{}).Bootstrap(ctx)
}

func (m *realtimeTruncatedRegressionClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	return acb.Response{StatusCode: 200, Body: `<table>
		<tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th></tr>
		<tr><td>TRUNCATED_TX1</td><td>22/09/2026</td><td>0</td><td>100,000</td></tr>
		<tr><td colspan="4"><span class="disabled">Trang sau</span></td></tr>
	</table><div>Tổng số dòng: 50</div>`, Kind: acb.HistoryPage}, nil
}

func TestRealtimeTask_TruncationFinishesPartial(t *testing.T) {
	ctx := context.Background()
	store, conn := newRealtimeRegressionStore(t, ctx)
	defer store.Close()

	mon := New(store, &realtimeTruncatedRegressionClient{}, 5*time.Second, 15*time.Second)
	mon.now = fixedRealtimeTime
	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done {
		t.Fatal("expected truncation to finish the poll")
	}

	runs, err := store.ListPollRuns(ctx, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected one poll run, got %+v (%v)", runs, err)
	}
	if runs[0].Status != "PARTIAL" || runs[0].Error != "REALTIME_PAGINATION_TRUNCATED" {
		t.Fatalf("expected truncation partial result, got status=%q error=%q", runs[0].Status, runs[0].Error)
	}
}

type todayBoundaryRegressionClient struct {
	multiPageMockClient
	t              *testing.T
	bootstrapCalls int
	historyCalls   int
	bootstrapErr   error
	failOnHistory  int
}

func (m *todayBoundaryRegressionClient) BootstrapToday(ctx context.Context, date string) (acb.Response, error) {
	m.bootstrapCalls++
	if date != fixedRealtimeTime().Format("02/01/2006") {
		m.t.Fatal("bootstrap did not receive the monitor's pinned local day")
	}
	if m.bootstrapErr != nil {
		return acb.Response{}, m.bootstrapErr
	}
	return m.Bootstrap(ctx)
}

func (m *todayBoundaryRegressionClient) HistoryToday(ctx context.Context, endpoint string, fields map[string]string, date string) (acb.Response, error) {
	m.historyCalls++
	if date != fixedRealtimeTime().Format("02/01/2006") || fields["FromDate"] != date || fields["ToDate"] != date || fields["activeDatetimeYN"] != "Y" {
		m.t.Fatal("today history lost its pinned day or verified source mode")
	}
	if m.historyCalls > 1 && (fields["_raw"] != "true" || fields["dse_nextEventName"] != "nextPage") {
		m.t.Fatal("today continuation lost the bank's raw navigation event")
	}
	if m.historyCalls == m.failOnHistory {
		return acb.Response{}, fmt.Errorf("today history: %w", acb.ErrRealtimeDateRollover)
	}
	return m.History(ctx, endpoint, fields)
}

func TestRealtimeTask_TodayRolloverIsPartialWithoutBackoff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bootstrap bool
		page      int
	}{
		{name: "bootstrap", bootstrap: true},
		{name: "initial_history", page: 1},
		{name: "foreground_continuation", page: 2},
		{name: "yielded_continuation", page: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, conn := newRealtimeRegressionStore(t, ctx)
			defer store.Close()
			client := &todayBoundaryRegressionClient{multiPageMockClient: multiPageMockClient{maxPages: 8}, t: t, failOnHistory: tc.page}
			if tc.bootstrap {
				client.bootstrapErr = fmt.Errorf("today bootstrap: %w", acb.ErrRealtimeDateRollover)
			}
			mon := New(store, client, 5*time.Second, 15*time.Second)
			mon.now = fixedRealtimeTime
			task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
			res, err := task.Step(ctx)
			if tc.page == 6 && (err != nil || res.Done) {
				t.Fatalf("expected foreground quantum to yield before page six: res=%+v err=%v", res, err)
			}
			for i := 0; i < 3 && !res.Done; i++ {
				res, err = task.Step(ctx)
			}
			if err != nil || !res.Done || !res.RequeueAt.IsZero() || res.Outcome != scheduler.OutcomeTransient {
				t.Fatalf("rollover should finish without auth/network retry: res=%+v err=%v", res, err)
			}
			wantPages := 0
			if tc.page > 1 {
				wantPages = tc.page - 1
			}
			if client.bootstrapCalls != 1 || client.historyCalls != tc.page || int(client.pagesReturned.Load()) != wantPages {
				t.Fatalf("today interface request boundary: bootstrap=%d history=%d returned=%d", client.bootstrapCalls, client.historyCalls, client.pagesReturned.Load())
			}
			runs, err := store.ListPollRuns(ctx, 1)
			wantPollPages := wantPages
			if wantPollPages == 0 {
				wantPollPages = 1
			}
			if err != nil || len(runs) != 1 || runs[0].Status != "PARTIAL" || runs[0].Error != acb.ErrRealtimeDateRollover.Error() || runs[0].Pages != wantPollPages || runs[0].RowsSeen != wantPages {
				t.Fatalf("rollover must retain only earlier page counters: polls=%+v err=%v", runs, err)
			}
			if wantPages > 0 && (runs[0].RowsMatched == nil || *runs[0].RowsMatched != wantPages) {
				t.Fatalf("earlier valid matched count must survive rollover: %+v", runs[0])
			}
			txns, err := store.ListTransactions(ctx, 10)
			if err != nil || len(txns) != wantPages {
				t.Fatalf("only valid earlier rows may survive rollover: count=%d err=%v", len(txns), err)
			}
			current, err := store.Connection(ctx)
			if err != nil || current.State != "MONITORING" || current.Generation != conn.Generation || mon.IsBackoffActive() {
				t.Fatalf("rollover changed auth state or activated backoff: conn=%+v err=%v", current, err)
			}
		})
	}
}
