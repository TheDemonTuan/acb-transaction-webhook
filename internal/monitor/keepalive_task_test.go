package monitor

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type keepaliveDetailsMockClient struct {
	bootstrapCalls atomic.Int32
	historyCalls   atomic.Int32
	respKind       acb.PageKind
	respStatus     int
	authErr        error
}

func (m *keepaliveDetailsMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	m.bootstrapCalls.Add(1)
	kind := m.respKind
	if kind == "" {
		kind = acb.AccountDetailPage
	}
	status := m.respStatus
	if status == 0 {
		status = 200
	}
	if m.authErr != nil {
		return acb.Response{StatusCode: status, Kind: kind, Body: "<html><body>OK</body></html>"}, m.authErr
	}
	if kind == acb.LoginPage || kind == acb.OTPChallenge || kind == acb.CaptchaPage {
		return acb.Response{StatusCode: status, Kind: kind, Body: "<html><body>OK</body></html>"}, &acb.AuthFailure{Kind: kind, Reason: "SESSION_EXPIRED"}
	}
	return acb.Response{StatusCode: status, Kind: kind, Body: "<html><body>OK</body></html>"}, nil
}

func (m *keepaliveDetailsMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	m.historyCalls.Add(1)
	return acb.Response{StatusCode: 200, Kind: acb.HistoryPage}, nil
}

func TestKeepaliveTask_BootstrapOnlyNeverHistory(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "ka_never_hist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	client := &keepaliveDetailsMockClient{}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	task := NewKeepaliveTask(mon, conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err != nil {
		t.Fatalf("unexpected step error: %v", err)
	}
	if !res.Done {
		t.Fatal("expected keepalive task to be done")
	}

	if client.bootstrapCalls.Load() != 1 {
		t.Fatalf("expected 1 bootstrap call, got %d", client.bootstrapCalls.Load())
	}
	if client.historyCalls.Load() != 0 {
		t.Fatalf("expected 0 history calls during keepalive, got %d", client.historyCalls.Load())
	}
}

func TestKeepaliveTask_RealtimePollOutranksKeepalive(t *testing.T) {
	q := scheduler.NewTaskQueue(100)
	now := time.Now()

	mon := New(nil, nil, 5*time.Second, 15*time.Second)
	ka := NewKeepaliveTask(mon, "conn1", 1)
	rt := NewRealtimeTask(mon, PriorityRealtimePoll, "conn1", 1)

	// Push keepalive first
	if err := q.Push(ka, now); err != nil {
		t.Fatal(err)
	}
	// Push realtime second
	if err := q.Push(rt, now.Add(1*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	// Realtime must pop first despite arriving after keepalive
	first := q.Pop()
	if first == nil || first.Kind() != "REALTIME_POLL" {
		t.Fatalf("expected REALTIME_POLL to outrank KEEPALIVE, got %+v", first)
	}

	second := q.Pop()
	if second == nil || second.Kind() != "KEEPALIVE" {
		t.Fatalf("expected KEEPALIVE to pop second, got %+v", second)
	}
}

func TestKeepaliveTask_LoginExpiryTransitionsToAuthRequired(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "ka_expiry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	client := &keepaliveDetailsMockClient{respKind: acb.LoginPage}
	mon := New(store, client, 5*time.Second, 15*time.Second)

	task := NewKeepaliveTask(mon, conn.ID, conn.Generation)
	res, err := task.Step(ctx)
	if err != nil {
		t.Fatalf("unexpected step error: %v", err)
	}
	if !res.Done {
		t.Fatal("expected keepalive task to be done")
	}
	if res.Outcome != scheduler.OutcomeAuth {
		t.Fatalf("expected OutcomeAuth, got %s", res.Outcome)
	}

	runs, _ := store.ListPollRuns(ctx, 1)
	if len(runs) != 1 || runs[0].Status != "AUTH_REQUIRED" {
		t.Fatalf("expected AUTH_REQUIRED poll run, got %+v", runs)
	}
}

func TestKeepaliveTask_RateLimitAndMaintenanceBackoff(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "ka_backoff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	// 1. Rate limited 429
	client429 := &keepaliveDetailsMockClient{respStatus: 429}
	mon := New(store, client429, 5*time.Second, 15*time.Second)

	task := NewKeepaliveTask(mon, conn.ID, conn.Generation)
	res, _ := task.Step(ctx)
	if res.Outcome != scheduler.OutcomeTransient {
		t.Fatalf("expected OutcomeTransient on 429, got %s", res.Outcome)
	}
	if !mon.IsBackoffActive() {
		t.Fatal("expected backoff to be active after 429")
	}

	// 2. Maintenance page
	mon.ClearBackoff()
	clientMaint := &keepaliveDetailsMockClient{respKind: acb.MaintenancePage}
	monMaint := New(store, clientMaint, 5*time.Second, 15*time.Second)

	taskMaint := NewKeepaliveTask(monMaint, conn.ID, conn.Generation)
	resMaint, _ := taskMaint.Step(ctx)
	if resMaint.Outcome != scheduler.OutcomeTransient {
		t.Fatalf("expected OutcomeTransient on maintenance, got %s", resMaint.Outcome)
	}
	if !monMaint.IsBackoffActive() {
		t.Fatal("expected backoff to be active after maintenance page")
	}
}

type keepaliveTodayMockClient struct {
	keepaliveDetailsMockClient
	todayCalls int
	todayDate  string
	todayErr   error
}

func (m *keepaliveTodayMockClient) BootstrapToday(_ context.Context, date string) (acb.Response, error) {
	m.todayCalls++
	m.todayDate = date
	return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: `<table><tr><th>Số GD</th><th>Ngày giao dịch</th><th>Ghi nợ</th><th>Ghi có</th></tr><tr><td>KEEPALIVE_ONLY</td><td>22/09/2026</td><td>0</td><td>100</td></tr></table>`}, m.todayErr
}

func (m *keepaliveTodayMockClient) HistoryToday(ctx context.Context, endpoint string, fields map[string]string, _ string) (acb.Response, error) {
	return m.History(ctx, endpoint, fields)
}

func TestKeepaliveTask_UsesTodayBootstrapWithoutIngestion(t *testing.T) {
	for _, rollover := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollover=%t", rollover), func(t *testing.T) {
			ctx := context.Background()
			store, conn := newRealtimeRegressionStore(t, ctx)
			defer store.Close()
			client := &keepaliveTodayMockClient{}
			if rollover {
				client.todayErr = fmt.Errorf("today bootstrap: %w", acb.ErrRealtimeDateRollover)
			}
			mon := New(store, client, 5*time.Second, 15*time.Second)
			mon.now = fixedRealtimeTime
			res, err := NewKeepaliveTask(mon, conn.ID, conn.Generation).Step(ctx)
			if err != nil || !res.Done || !res.RequeueAt.IsZero() || res.Outcome != scheduler.OutcomeSuccess {
				t.Fatalf("keepalive today result=%+v err=%v", res, err)
			}
			if client.todayCalls != 1 || client.todayDate != fixedRealtimeTime().Format("02/01/2006") || client.bootstrapCalls.Load() != 0 || client.historyCalls.Load() != 0 {
				t.Fatalf("keepalive must call only pinned Today bootstrap: today=%d day=%q generic=%d history=%d", client.todayCalls, client.todayDate, client.bootstrapCalls.Load(), client.historyCalls.Load())
			}
			runs, err := store.ListPollRuns(ctx, 1)
			wantStatus, wantError := "SUCCEEDED", ""
			if rollover {
				wantStatus, wantError = "PARTIAL", acb.ErrRealtimeDateRollover.Error()
			}
			if err != nil || len(runs) != 1 || runs[0].Status != wantStatus || runs[0].Error != wantError || runs[0].Pages != 0 || runs[0].RowsSeen != 0 || runs[0].RowsMatched != nil {
				t.Fatalf("keepalive must not parse history rows: polls=%+v err=%v", runs, err)
			}
			var transactions, events int
			if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM transactions").Scan(&transactions); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&events); err != nil {
				t.Fatal(err)
			}
			current, err := store.Connection(ctx)
			if err != nil || current.State != "MONITORING" || current.Generation != conn.Generation || mon.IsBackoffActive() || transactions != 0 || events != 0 {
				t.Fatalf("keepalive must preserve state without ingestion/backoff: conn=%+v transactions=%d events=%d err=%v", current, transactions, events, err)
			}
		})
	}
}
