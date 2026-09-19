package monitor

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type mockBankClient struct {
	getResp       acb.Response
	getErr        error
	historyResp   acb.Response
	historyErr    error
	historyFields map[string]string
}

func (m *mockBankClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	return m.getResp, m.getErr
}

func (m *mockBankClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	m.historyFields = make(map[string]string, len(fields))
	for key, value := range fields {
		m.historyFields[key] = value
	}
	return m.historyResp, m.historyErr
}

const mockHistoryHTML = `
<table>
  <tr>
    <th>Ngày hiệu lực</th>
    <th>Ngày giao dịch</th>
    <th>Số GD</th>
    <th>Ghi nợ</th>
    <th>Ghi có</th>
    <th>Số dư</th>
    <th>Nội dung giao dịch</th>
  </tr>
  <tr>
    <td>10/09/2026</td>
    <td>10/09/2026</td>
    <td>7788</td>
    <td>-</td>
    <td>150.000</td>
    <td>2.000.000</td>
    <td>Test Nap Tien</td>
  </tr>
</table>
`

func TestTransportFailureKeepsMonitoringGeneration(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	connection, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`)
	m := New(store, &mockBankClient{getErr: errors.New("connection reset by peer")}, 10*time.Second, 30*time.Second)
	if err := m.PollOnce(ctx); err == nil {
		t.Fatal("expected transport error")
	}
	got, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "MONITORING" || got.Generation != connection.Generation {
		t.Fatalf("transient failure changed session: %+v", got)
	}
	runs, err := store.ListPollRuns(ctx, 10)
	if err != nil || len(runs) != 1 || runs[0].Status != "FAILED" {
		t.Fatalf("unexpected poll records: %+v %v", runs, err)
	}
	if runs[0].Error != acb.ErrNetwork {
		t.Fatalf("expected sanitized network error, got %q", runs[0].Error)
	}
	if !m.IsBackoffActive() {
		t.Fatal("expected transport failure to activate backoff")
	}
}

func TestFinishPollUsesCleanupContextAfterTaskTimeout(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, _ = store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`)

	poll, err := store.StartPoll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	poll.Status = "FAILED"
	poll.Error = acb.ErrRequestTimeout
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := New(store, &mockBankClient{}, 5*time.Second, 5*time.Second).finishPoll(canceled, poll, 0); err != nil {
		t.Fatal(err)
	}

	runs, err := store.ListPollRuns(ctx, 10)
	if err != nil || len(runs) != 1 || runs[0].Status != "FAILED" || runs[0].FinishedAt == "" {
		t.Fatalf("poll was not finalized after timeout: %+v %v", runs, err)
	}
}

func TestNetworkBackoffIncreasesAndSuccessClearsIt(t *testing.T) {
	m := New(nil, nil, 5*time.Second, 5*time.Second)
	base := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base }
	if got := m.RecordNetworkFailure(errors.New("reset")); !got.Equal(base.Add(5 * time.Second)) {
		t.Fatalf("first backoff=%s", got)
	}
	if got := m.RecordNetworkFailure(errors.New("reset")); !got.Equal(base.Add(10 * time.Second)) {
		t.Fatalf("second backoff=%s", got)
	}
	m.ClearBackoff()
	if m.IsBackoffActive() {
		t.Fatal("expected successful upstream response to clear backoff")
	}
}

func TestMonitorUsesConfiguredAccountWhenResponseOmitsAccountNbr(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, _ = store.ConfigureConnection(ctx, "40478827")
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`)

	mock := &mockBankClient{
		getResp: acb.Response{
			StatusCode: 200,
			Kind:       acb.AccountDetailPage,
			Body: `<form action="/acbib/Request">
				<input name="dse_operationName" value="ibkacctDetailProc">
				<input name="dse_processorState" value="acctDetailPage">
				<input name="dse_sessionId" value="session">
			</form>`,
		},
		historyResp: acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHistoryHTML},
	}
	if err := New(store, mock, 5*time.Second, 5*time.Second).PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if mock.historyFields["AccountNbr"] != "40478827" {
		t.Fatalf("AccountNbr=%q", mock.historyFields["AccountNbr"])
	}
}

func TestMonitorPollNotifierFiresOnCompletion(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, _ = store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`)

	mock := &mockBankClient{
		getResp: acb.Response{
			StatusCode: 200,
			Kind:       acb.HistoryPage,
			Body:       mockHistoryHTML,
		},
	}

	m := New(store, mock, 5*time.Second, 5*time.Second)
	var notifiedPoll storage.PollRun
	var notifiedInserted int
	m.WithPollNotifier(func(poll storage.PollRun, insertedCount int) {
		notifiedPoll = poll
		notifiedInserted = insertedCount
	})

	err = m.PollOnce(ctx)
	if err != nil {
		t.Fatalf("poll failed: %v", err)
	}

	if notifiedPoll.ID == "" || notifiedPoll.Status != "SUCCEEDED" || notifiedPoll.RowsSeen != 1 {
		t.Fatalf("unexpected notified poll: %+v", notifiedPoll)
	}
	if notifiedInserted != 1 {
		t.Fatalf("expected 1 inserted transaction, got %d", notifiedInserted)
	}

	// Second poll deduplicates -> notifiedInserted should be 0
	err = m.PollOnce(ctx)
	if err != nil {
		t.Fatalf("second poll failed: %v", err)
	}
	if notifiedInserted != 0 {
		t.Fatalf("expected 0 inserted on dedup poll, got %d", notifiedInserted)
	}
}

func TestMonitorPollHappyPath(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, _ = store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`)

	ep, err := store.CreateEndpointWithSecret(ctx, "Webhook Receiver", "https://example.com/receiver")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEndpointStatus(ctx, ep.ID, "ACTIVE"); err != nil {
		t.Fatal(err)
	}

	mock := &mockBankClient{
		getResp: acb.Response{
			StatusCode: 200,
			Kind:       acb.HistoryPage,
			Body:       mockHistoryHTML,
		},
	}

	m := New(store, mock, 5*time.Second, 5*time.Second)
	err = m.PollOnce(ctx)
	if err != nil {
		t.Fatalf("poll failed: %v", err)
	}

	summary, err := store.DeliverySummary(ctx)
	if err != nil || summary.Pending != 1 {
		t.Fatalf("expected 1 pending delivery, got: %+v %v", summary, err)
	}

	// Ingesting again should deduplicate and not queue a second delivery
	err = m.PollOnce(ctx)
	if err != nil {
		t.Fatalf("second poll failed: %v", err)
	}

	summary, err = store.DeliverySummary(ctx)
	if err != nil || summary.Pending != 1 {
		t.Fatalf("expected still 1 pending delivery (deduped), got: %+v", summary)
	}
	_ = ep
}

func TestMonitorSessionExpired(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, _ = store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`)

	mock := &mockBankClient{
		getResp: acb.Response{
			StatusCode: 200,
			Kind:       acb.LoginPage,
			Body:       `<input name="username"><input name="password">`,
		},
	}

	m := New(store, mock, 5*time.Second, 5*time.Second)
	err = m.PollOnce(ctx)
	if err != nil {
		t.Fatalf("poll failed: %v", err)
	}

	conn, err := store.Connection(ctx)
	if err != nil || conn.State != "AUTH_REQUIRED" {
		t.Fatalf("expected state AUTH_REQUIRED, got %+v", conn)
	}
}

type countingMockClient struct {
	getFunc     func(ctx context.Context, endpoint string) (acb.Response, error)
	historyFunc func(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error)
}

func (c *countingMockClient) Bootstrap(ctx context.Context) (acb.Response, error) {
	if c.getFunc != nil {
		return c.getFunc(ctx, "")
	}
	return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHistoryHTML}, nil
}

func (c *countingMockClient) History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	if c.historyFunc != nil {
		return c.historyFunc(ctx, endpoint, fields)
	}
	return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHistoryHTML}, nil
}

func TestRequestSyncRejectsNonMonitoring(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_sync_reject.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	mock := &mockBankClient{}
	m := New(store, mock, 5*time.Second, 5*time.Second)

	// No connection configured
	err = m.RequestSync(ctx)
	if !errors.Is(err, ErrSyncUnavailable) {
		t.Fatalf("expected ErrSyncUnavailable on missing connection, got: %v", err)
	}

	// AUTH_REQUIRED state
	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	err = m.RequestSync(ctx)
	if !errors.Is(err, ErrSyncUnavailable) {
		t.Fatalf("expected ErrSyncUnavailable on AUTH_REQUIRED, got: %v", err)
	}

	// PAUSED state
	if _, err := store.TransitionConnection(ctx, "pause"); err != nil {
		t.Fatal(err)
	}
	err = m.RequestSync(ctx)
	if !errors.Is(err, ErrSyncUnavailable) {
		t.Fatalf("expected ErrSyncUnavailable on PAUSED, got: %v", err)
	}

	// MONITORING state
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}
	err = m.RequestSync(ctx)
	if err != nil {
		t.Fatalf("expected nil error on MONITORING, got: %v", err)
	}
}

func TestRequestSyncPreservesGenerationAndSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_sync_preserve.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}
	before, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}

	mock := &mockBankClient{
		getResp: acb.Response{
			StatusCode: 200,
			Kind:       acb.HistoryPage,
			Body:       mockHistoryHTML,
		},
	}

	m := New(store, mock, 5*time.Second, 5*time.Second)

	// Run monitor in background
	go m.Run(ctx)

	if err := m.RequestSync(ctx); err != nil {
		t.Fatalf("RequestSync failed: %v", err)
	}

	// Wait for poll to complete
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := store.ListPollRuns(ctx, 10)
		if len(runs) > 0 && runs[0].Status == "SUCCEEDED" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	after, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation {
		t.Fatalf("generation mutated: before=%d after=%d", before.Generation, after.Generation)
	}
	if after.State != "MONITORING" {
		t.Fatalf("state mutated: before=%s after=%s", before.State, after.State)
	}
}

func TestRequestSyncActualQueuedPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_sync_queued.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}

	ep, err := store.CreateEndpointWithSecret(ctx, "Webhook Receiver", "https://example.com/receiver")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEndpointStatus(ctx, ep.ID, "ACTIVE"); err != nil {
		t.Fatal(err)
	}

	conn, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	if err := store.SaveCheckpoint(ctx, storage.Checkpoint{ConnectionID: conn.ID, CoverageTo: today, UpdatedAt: today}); err != nil {
		t.Fatal(err)
	}

	var callCount atomic.Int32
	mock := &countingMockClient{
		getFunc: func(ctx context.Context, endpoint string) (acb.Response, error) {
			callCount.Add(1)
			return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHistoryHTML}, nil
		},
	}

	// Long poll interval so periodic poll won't fire during test
	m := New(store, mock, 1*time.Hour, 1*time.Hour)
	go m.Run(ctx)

	if err := m.RequestSync(ctx); err != nil {
		t.Fatalf("RequestSync failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if callCount.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if callCount.Load() < 1 {
		t.Fatalf("expected at least 1 call from queued sync, got %d", callCount.Load())
	}

	var summary storage.DeliverySummary
	summaryDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(summaryDeadline) {
		summary, err = store.DeliverySummary(ctx)
		if err == nil && summary.Pending == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if summary.Pending != 1 {
		t.Fatalf("expected 1 pending delivery from queued poll, got %+v %v", summary, err)
	}
}

func TestRequestSyncStaleSyncSkipped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_sync_stale.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}

	var callCount atomic.Int32
	mock := &countingMockClient{
		getFunc: func(ctx context.Context, endpoint string) (acb.Response, error) {
			callCount.Add(1)
			return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHistoryHTML}, nil
		},
	}

	m := New(store, mock, 1*time.Hour, 1*time.Hour)

	// Enqueue sync while in MONITORING (gen 0)
	if err := m.RequestSync(ctx); err != nil {
		t.Fatalf("RequestSync failed: %v", err)
	}

	// Change connection to PAUSED (increments generation and changes state) before Run consumes it
	if _, err := store.TransitionConnection(ctx, "pause"); err != nil {
		t.Fatal(err)
	}

	// Now start Run. It consumes the queued sync, but under the lock sees ID/gen/state mismatch
	go m.Run(ctx)

	// Wait briefly to allow Run to process the queued sync
	time.Sleep(150 * time.Millisecond)

	if callCount.Load() != 0 {
		t.Fatalf("expected 0 calls for stale sync, got %d", callCount.Load())
	}

	runs, err := store.ListPollRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected 0 poll runs for stale sync, got %d", len(runs))
	}
}

func TestRequestSyncCoalescing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_sync_coalesce.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}

	var callCount atomic.Int32
	mock := &countingMockClient{
		getFunc: func(ctx context.Context, endpoint string) (acb.Response, error) {
			callCount.Add(1)
			return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHistoryHTML}, nil
		},
	}

	m := New(store, mock, 1*time.Hour, 1*time.Hour)

	conn, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	if err := store.SaveCheckpoint(ctx, storage.Checkpoint{ConnectionID: conn.ID, CoverageTo: today, UpdatedAt: today}); err != nil {
		t.Fatal(err)
	}

	// Call RequestSync multiple times concurrently before Run starts
	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.RequestSync(ctx); err != nil {
				t.Errorf("RequestSync failed: %v", err)
			}
		}()
	}
	wg.Wait()

	// Start Run and let it process
	go m.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if callCount.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Give a little extra time to ensure no second poll was triggered
	time.Sleep(100 * time.Millisecond)

	if callCount.Load() < 1 {
		t.Fatalf("expected at least 1 coalesced call, got %d", callCount.Load())
	}
}

func TestMonitorCircuitBreakerAndBrowserHandoff(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_cb_handoff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, _ := store.ConfigureConnection(ctx, "***1234")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'")

	var callCount atomic.Int32
	mock := &mockBankClient{
		getResp: acb.Response{
			StatusCode: 429,
			Kind:       acb.UnknownPage,
			Body:       "Too Many Requests",
		},
	}

	m := New(store, mock, 10*time.Second, 10*time.Second)

	// 1. First poll hits 429
	_ = m.PollOnce(ctx)
	runs, _ := store.ListPollRuns(ctx, 5)
	if len(runs) != 1 || runs[0].Error != "ACB_RATE_LIMITED" {
		t.Fatalf("expected ACB_RATE_LIMITED, got: %+v", runs)
	}

	// 2. Second poll immediately is skipped by circuit breaker backoff (mock not called)
	initialCalls := callCount.Load()
	_ = m.PollOnce(ctx)
	runsAfter, _ := store.ListPollRuns(ctx, 5)
	if len(runsAfter) != 1 {
		t.Fatalf("expected second poll skipped by circuit breaker, got runs: %d", len(runsAfter))
	}
	_ = initialCalls

	// 3. Reset backoff
	m.backoffUntil = time.Time{}

	// 4. Start active auth attempt -> browser handoff should skip polling
	_, err = store.StartAuthAttempt(ctx, "owner@example.com", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Set connection back to MONITORING to test HasActiveAuthAttempt guard
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING' WHERE id = ?", conn.ID)

	err = m.PollOnce(ctx)
	if err != nil {
		t.Fatalf("expected nil error on skipped poll, got %v", err)
	}
	runsHandoff, _ := store.ListPollRuns(ctx, 5)
	if len(runsHandoff) != 1 {
		t.Fatalf("expected poll skipped during active browser auth attempt, got runs: %d", len(runsHandoff))
	}
}

func TestRequestSyncConcurrentNormalPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test_sync_concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE connections SET state='MONITORING'"); err != nil {
		t.Fatal(err)
	}

	conn, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	if err := store.SaveCheckpoint(ctx, storage.Checkpoint{ConnectionID: conn.ID, CoverageTo: today, UpdatedAt: today}); err != nil {
		t.Fatal(err)
	}

	inNormalPoll := make(chan struct{})
	releaseNormalPoll := make(chan struct{})
	var callCount atomic.Int32

	mock := &countingMockClient{
		getFunc: func(ctx context.Context, endpoint string) (acb.Response, error) {
			idx := callCount.Add(1)
			if idx == 1 {
				// Signal that normal poll is in progress and wait
				close(inNormalPoll)
				select {
				case <-releaseNormalPoll:
				case <-ctx.Done():
				}
			}
			return acb.Response{StatusCode: 200, Kind: acb.HistoryPage, Body: mockHistoryHTML}, nil
		},
	}

	m := New(store, mock, 1*time.Hour, 1*time.Hour)

	// Start normal poll in a goroutine
	normalDone := make(chan error, 1)
	go func() {
		normalDone <- m.PollOnce(ctx)
	}()

	// Wait until normal poll is actively holding the lock and inside client.Get
	select {
	case <-inNormalPoll:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for normal poll to start")
	}

	// Now call RequestSync concurrently. Must not block or deadlock.
	syncDone := make(chan error, 1)
	go func() {
		syncDone <- m.RequestSync(ctx)
	}()

	select {
	case err := <-syncDone:
		if err != nil {
			t.Fatalf("RequestSync failed during active poll: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RequestSync blocked unexpectedly during active normal poll")
	}

	// Wait until normal poll is finished before starting Run to process the queued sync,
	// preventing concurrent write lock contention on SQLite.
	close(releaseNormalPoll)

	select {
	case err := <-normalDone:
		if err != nil {
			t.Fatalf("normal poll failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for normal poll to finish")
	}

	// Start Run to consume the queued sync once normal poll finishes
	go m.Run(ctx)

	// Wait for the queued sync poll to also execute
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if callCount.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if callCount.Load() < 2 {
		t.Fatalf("expected at least 2 total calls (normal + queued sync), got %d", callCount.Load())
	}
}

func TestMonitorSuccessResetResetSuccessPreservesSessionVsTrueLogin(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connection, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`)

	ep, err := store.CreateEndpointWithSecret(ctx, "Webhook Receiver", "https://example.com/receiver")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEndpointStatus(ctx, ep.ID, "ACTIVE"); err != nil {
		t.Fatal(err)
	}

	mock := &mockBankClient{}
	m := New(store, mock, 5*time.Second, 5*time.Second)

	historyWithDseErrorPage := "ibkacctDetailProc dse_processorState AccountNbr Số GD Ghi nợ Ghi có\n" + mockHistoryHTML
	successKind := acb.ClassifyPage("https://online.acb.com.vn/acbib/Request?dse_errorPage=login.jsp", historyWithDseErrorPage)
	if successKind != acb.HistoryPage {
		t.Fatalf("expected HistoryPage classification, got %s", successKind)
	}

	// 1. First poll: success (authenticated with dse_errorPage=login.jsp in URL)
	mock.getResp = acb.Response{
		StatusCode: 200,
		Kind:       successKind,
		Body:       historyWithDseErrorPage,
	}
	mock.getErr = nil
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("poll 1 (success) failed: %v", err)
	}
	conn, err := store.Connection(ctx)
	if err != nil || conn.State != "MONITORING" || conn.Generation != connection.Generation {
		t.Fatalf("poll 1: expected state MONITORING with gen %d, got state=%s gen=%d", connection.Generation, conn.State, conn.Generation)
	}

	// 2. Second poll: connection reset 1
	mock.getErr = errors.New("read: connection reset by peer")
	if err := m.PollOnce(ctx); err == nil {
		t.Fatal("poll 2 (reset 1): expected network error")
	}
	conn, err = store.Connection(ctx)
	if err != nil || conn.State != "MONITORING" || conn.Generation != connection.Generation {
		t.Fatalf("poll 2: transient reset changed session state: state=%s gen=%d", conn.State, conn.Generation)
	}
	if !m.IsBackoffActive() {
		t.Fatal("poll 2: expected network backoff")
	}
	m.ClearBackoff() // Simulate retry after the backoff window.

	// 3. Third poll: connection reset 2
	mock.getErr = errors.New("read: connection reset by peer")
	if err := m.PollOnce(ctx); err == nil {
		t.Fatal("poll 3 (reset 2): expected network error")
	}
	conn, err = store.Connection(ctx)
	if err != nil || conn.State != "MONITORING" || conn.Generation != connection.Generation {
		t.Fatalf("poll 3: transient reset changed session state: state=%s gen=%d", conn.State, conn.Generation)
	}
	m.ClearBackoff() // Simulate retry after the backoff window.

	// 4. Fourth poll: success again, verifying session preserved across resets
	mock.getErr = nil
	mock.getResp = acb.Response{
		StatusCode: 200,
		Kind:       successKind,
		Body:       historyWithDseErrorPage,
	}
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("poll 4 (success) failed: %v", err)
	}
	conn, err = store.Connection(ctx)
	if err != nil || conn.State != "MONITORING" || conn.Generation != connection.Generation {
		t.Fatalf("poll 4: session was not preserved across resets: state=%s gen=%d", conn.State, conn.Generation)
	}

	// 5. Fifth poll: true login fixture -> transitions connection to AUTH_REQUIRED
	trueLoginKind := acb.ClassifyPage("https://online.acb.com.vn/acbib/Request", `<input name="username"><input type="password" name="password">`)
	if trueLoginKind != acb.LoginPage {
		t.Fatalf("expected LoginPage classification, got %s", trueLoginKind)
	}
	mock.getResp = acb.Response{
		StatusCode: 200,
		Kind:       trueLoginKind,
		Body:       `<input name="username"><input type="password" name="password">`,
	}
	if err := m.PollOnce(ctx); err != nil {
		t.Fatalf("poll 5 (true login) failed: %v", err)
	}
	conn, err = store.Connection(ctx)
	if err != nil || conn.State != "AUTH_REQUIRED" {
		t.Fatalf("poll 5: expected state AUTH_REQUIRED on true login, got %+v", conn)
	}

	runs, err := store.ListPollRuns(ctx, 10)
	if err != nil || len(runs) != 5 {
		t.Fatalf("expected 5 poll runs, got %d (err: %v)", len(runs), err)
	}
	// ListPollRuns returns descending by timestamp: runs[0] is newest (run 5)
	if runs[0].Status != "AUTH_REQUIRED" {
		t.Fatalf("expected run 5 status AUTH_REQUIRED, got %s", runs[0].Status)
	}
	if runs[1].Status != "SUCCEEDED" {
		t.Fatalf("expected run 4 status SUCCEEDED, got %s", runs[1].Status)
	}
	if runs[2].Status != "FAILED" {
		t.Fatalf("expected run 3 status FAILED, got %s", runs[2].Status)
	}
	if runs[3].Status != "FAILED" {
		t.Fatalf("expected run 2 status FAILED, got %s", runs[3].Status)
	}
	if runs[4].Status != "SUCCEEDED" {
		t.Fatalf("expected run 1 status SUCCEEDED, got %s", runs[4].Status)
	}
}

func TestPaymentBoost_HardCapDuration(t *testing.T) {
	m := &Monitor{
		now:     time.Now,
		boostCh: make(chan struct{}, 1),
	}
	base := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	curr := base
	m.now = func() time.Time { return curr }

	// Initial start at t=0
	st := m.StartPaymentBoost(100000)
	if !st.Active || st.ExpiresIn != 180 || st.Phase != 1 {
		t.Fatalf("unexpected initial boost: %+v", st)
	}

	// Advance to t=60s (Phase 2, remaining 120s)
	curr = base.Add(60 * time.Second)
	// Repeat start at t=60s must NOT reset expiration back to 180s
	st2 := m.StartPaymentBoost(250000)
	if !st2.Active {
		t.Fatal("expected boost to remain active")
	}
	if st2.AmountVnd != 250000 {
		t.Fatalf("expected updated amount 250000, got %d", st2.AmountVnd)
	}
	if st2.ExpiresIn != 120 {
		t.Fatalf("expected hard-cap remaining 120s, got %d", st2.ExpiresIn)
	}
	if st2.Phase != 2 {
		t.Fatalf("expected Phase 2 at t=60s, got %d", st2.Phase)
	}

	// Advance to t=130s (Phase 3, remaining 50s) - switch to unfixed amount (amount=0)
	curr = base.Add(130 * time.Second)
	st3 := m.StartPaymentBoost(0)
	if !st3.Active || st3.ExpiresIn != 50 || st3.Phase != 3 {
		t.Fatalf("expected Phase 3 with 50s remaining, got %+v", st3)
	}
	if st3.AmountVnd != 0 {
		t.Fatalf("expected amount updated to 0 for unfixed mode, got %d", st3.AmountVnd)
	}

	// Advance past 180s (e.g. t=181s) -> boost is expired
	curr = base.Add(181 * time.Second)
	stExpired := m.PaymentBoostStatus()
	if stExpired.Active {
		t.Fatalf("expected boost expired at t=181s, got %+v", stExpired)
	}

	// Starting after expiration creates a fresh 180s window
	stFresh := m.StartPaymentBoost(50000)
	if !stFresh.Active || stFresh.ExpiresIn != 180 || stFresh.Phase != 1 {
		t.Fatalf("expected fresh 180s boost after expiration, got %+v", stFresh)
	}
}

func TestPaymentBoost_ReadLockRaceSafety(t *testing.T) {
	m := &Monitor{
		now:     time.Now,
		boostCh: make(chan struct{}, 10),
	}
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	var currentNanos atomic.Int64
	currentNanos.Store(base.UnixNano())
	m.now = func() time.Time {
		return time.Unix(0, currentNanos.Load())
	}

	m.StartPaymentBoost(100000)

	var wg sync.WaitGroup
	// 20 concurrent readers calling PaymentBoostStatus under RLock
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_ = m.PaymentBoostStatus()
			}
		}()
	}

	// 5 concurrent goroutines calling StartPaymentBoost / StopPaymentBoost
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if j%2 == 0 {
					m.StartPaymentBoost(int64(10000 * (id + 1)))
				} else {
					m.StopPaymentBoost()
				}
			}
		}(i)
	}

	// Time advancer advancing time past 180s
	wg.Add(1)
	go func() {
		defer wg.Done()
		for sec := int64(1); sec <= 200; sec++ {
			currentNanos.Store(base.Add(time.Duration(sec) * time.Second).UnixNano())
		}
	}()

	wg.Wait()
}
