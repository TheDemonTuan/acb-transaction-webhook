package workerrpc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

type mockWorkerHandler struct {
	syncCalled            bool
	createHistoryFrom     string
	createHistoryTo       string
	canceledJobID         string
	settingsChangedCalled bool
	wakeDispatcherCalled  bool
	verifiedAccount       string

	createJobErr error
	cancelJobErr error
	settingsErr  error
	wakeErr      error
	syncBlock    chan struct{}
	jobs         map[string]storage.HistorySyncJob
	mu           sync.Mutex
}

func (m *mockWorkerHandler) RequestSync(ctx context.Context) error {
	m.syncCalled = true
	if m.syncBlock != nil {
		<-m.syncBlock
	}
	return nil
}

func (m *mockWorkerHandler) CreateHistoryJob(ctx context.Context, fromDay, toDay string) (storage.HistorySyncJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createHistoryFrom = fromDay
	m.createHistoryTo = toDay
	if m.createJobErr != nil {
		return storage.HistorySyncJob{}, m.createJobErr
	}
	key := fromDay + ":" + toDay
	if m.jobs == nil {
		m.jobs = make(map[string]storage.HistorySyncJob)
	}
	if existing, exists := m.jobs[key]; exists {
		return existing, nil
	}
	job := storage.HistorySyncJob{
		ID:           "job_" + fromDay + "_" + toDay,
		ConnectionID: "conn_1",
		Generation:   1,
		RangeFrom:    fromDay,
		RangeTo:      toDay,
		Status:       storage.HistoryJobStatusQueued,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	m.jobs[key] = job
	return job, nil
}

func (m *mockWorkerHandler) CancelHistoryJob(ctx context.Context, jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.canceledJobID = jobID
	if m.cancelJobErr != nil {
		return m.cancelJobErr
	}
	if jobID == "non-existent" {
		return storage.ErrJobNotFound
	}
	if jobID == "terminal-job" {
		return storage.ErrJobTerminal
	}
	return nil
}

func (m *mockWorkerHandler) NotifySettingsChanged(ctx context.Context) error {
	m.settingsChangedCalled = true
	return m.settingsErr
}

func (m *mockWorkerHandler) WakeDispatcher(ctx context.Context) error {
	m.wakeDispatcherCalled = true
	return m.wakeErr
}

func (m *mockWorkerHandler) VerifySession(ctx context.Context, account string, generation int64, password []byte) error {
	if string(password) == "wrong" {
		return errors.New("invalid password")
	}
	m.verifiedAccount = account
	return nil
}

func TestWorkerRPC_ConstructorValidation(t *testing.T) {
	mock := &mockWorkerHandler{}

	if _, err := workerrpc.NewServer(nil, "token"); err == nil {
		t.Fatal("expected NewServer with nil handler to fail")
	}
	if _, err := workerrpc.NewServer(mock, ""); err == nil {
		t.Fatal("expected NewServer with empty token to fail")
	}
	if _, err := workerrpc.NewServer(mock, "   \n"); err == nil {
		t.Fatal("expected NewServer with whitespace token to fail")
	}
}

func TestWorkerRPC_Roundtrip(t *testing.T) {
	mock := &mockWorkerHandler{}
	token := "secret-test-token-123"
	server, err := workerrpc.NewServer(mock, token)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, token)
	ctx := context.Background()

	// 1. RequestSync
	if err := client.RequestSync(ctx); err != nil {
		t.Fatalf("RequestSync failed: %v", err)
	}
	if !mock.syncCalled {
		t.Errorf("expected RequestSync to be called on mock")
	}

	// 2. CreateHistoryJob
	job, err := client.CreateHistoryJob(ctx, "2026-09-01", "2026-09-10")
	if err != nil {
		t.Fatalf("CreateHistoryJob failed: %v", err)
	}
	if job.Status != storage.HistoryJobStatusQueued || job.RangeFrom != "2026-09-01" || job.RangeTo != "2026-09-10" {
		t.Errorf("CreateHistoryJob unexpected result: %+v", job)
	}

	// 3. CancelHistoryJob
	if err := client.CancelHistoryJob(ctx, job.ID); err != nil {
		t.Fatalf("CancelHistoryJob failed: %v", err)
	}
	if mock.canceledJobID != job.ID {
		t.Errorf("expected canceled job ID %s, got %s", job.ID, mock.canceledJobID)
	}

	// 4. EnsureHistory compatibility wrapper
	count, err := client.EnsureHistory(ctx, "2026-09-01", "2026-09-10")
	if err != nil {
		t.Fatalf("EnsureHistory wrapper failed: %v", err)
	}
	if count != 0 {
		t.Errorf("expected count 0 from initial job, got %d", count)
	}

	// 5. NotifySettingsChanged
	if err := client.NotifySettingsChanged(ctx); err != nil {
		t.Fatalf("NotifySettingsChanged failed: %v", err)
	}
	if !mock.settingsChangedCalled {
		t.Errorf("expected NotifySettingsChanged to be called")
	}

	// 6. WakeDispatcher
	if err := client.WakeDispatcher(ctx); err != nil {
		t.Fatalf("WakeDispatcher failed: %v", err)
	}
	if !mock.wakeDispatcherCalled {
		t.Errorf("expected WakeDispatcher to be called")
	}

	// 7. VerifySession success
	if err := client.VerifySession(ctx, "12345678", 1, []byte("correct")); err != nil {
		t.Fatalf("VerifySession failed: %v", err)
	}
	if mock.verifiedAccount != "12345678" {
		t.Errorf("expected account to match")
	}

	// 8. VerifySession failure
	if err := client.VerifySession(ctx, "12345678", 1, []byte("wrong")); err == nil {
		t.Fatalf("expected VerifySession with wrong password to fail")
	}

	// 9. Unauthorized client
	unauthClient := workerrpc.NewClient(ts.URL, "bad-token")
	if err := unauthClient.RequestSync(ctx); err == nil {
		t.Fatalf("expected unauthenticated request to fail")
	}
}

func TestWorkerRPC_CreateHistoryJob_ValidAndDuplicateReuse(t *testing.T) {
	mock := &mockWorkerHandler{}
	token := "secret-test-token-123"
	server, err := workerrpc.NewServer(mock, token)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, token)
	ctx := context.Background()

	// Initial creation
	job1, err := client.CreateHistoryJob(ctx, "2026-09-01", "2026-09-15")
	if err != nil {
		t.Fatalf("CreateHistoryJob 1: %v", err)
	}
	if job1.ID == "" || job1.Status != storage.HistoryJobStatusQueued {
		t.Fatalf("unexpected job1 descriptor: %+v", job1)
	}

	// Duplicate creation: returns the exact same job ID
	job2, err := client.CreateHistoryJob(ctx, "2026-09-01", "2026-09-15")
	if err != nil {
		t.Fatalf("CreateHistoryJob 2: %v", err)
	}
	if job2.ID != job1.ID {
		t.Fatalf("expected duplicate range to reuse job ID %s, got %s", job1.ID, job2.ID)
	}
}

func TestWorkerRPC_CreateHistoryJob_InvalidRangeValidation(t *testing.T) {
	mock := &mockWorkerHandler{}
	token := "secret-test-token-123"
	server, err := workerrpc.NewServer(mock, token)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, token)
	ctx := context.Background()

	// 1. Invalid date format
	if _, err := client.CreateHistoryJob(ctx, "invalid-date", "2026-09-10"); err == nil {
		t.Fatal("expected error for invalid fromDay format, got nil")
	}
	if _, err := client.CreateHistoryJob(ctx, "2026-09-01", "not-a-date"); err == nil {
		t.Fatal("expected error for invalid toDay format, got nil")
	}

	// 2. fromDay after toDay
	if _, err := client.CreateHistoryJob(ctx, "2026-09-10", "2026-09-01"); err == nil {
		t.Fatal("expected error when fromDay > toDay, got nil")
	}

	// 3. Range exceeds 31 days
	if _, err := client.CreateHistoryJob(ctx, "2026-08-01", "2026-09-10"); err == nil {
		t.Fatal("expected error when range exceeds 31 days (40 days), got nil")
	}
}

func TestWorkerRPC_CreateHistoryJob_CanceledContext(t *testing.T) {
	mock := &mockWorkerHandler{}
	token := "secret-test-token-123"
	server, err := workerrpc.NewServer(mock, token)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, token)

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel() // Already canceled before call

	_, err = client.CreateHistoryJob(canceledCtx, "2026-09-01", "2026-09-10")
	if err == nil {
		t.Fatal("expected error when context canceled before enqueue, got nil")
	}
	if mock.createHistoryFrom != "" {
		t.Errorf("handler must not be executed when context is canceled beforehand")
	}
}

func TestWorkerRPC_CancelHistoryJob_Semantics(t *testing.T) {
	mock := &mockWorkerHandler{}
	token := "secret-test-token-123"
	server, err := workerrpc.NewServer(mock, token)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, token)
	ctx := context.Background()

	// 1. Success
	if err := client.CancelHistoryJob(ctx, "job_valid_123"); err != nil {
		t.Fatalf("CancelHistoryJob failed: %v", err)
	}

	// 2. Not found
	if err := client.CancelHistoryJob(ctx, "non-existent"); err == nil {
		t.Fatal("expected error for non-existent job, got nil")
	}

	// 3. Terminal state conflict
	if err := client.CancelHistoryJob(ctx, "terminal-job"); err == nil {
		t.Fatal("expected error for terminal job cancellation, got nil")
	}
}

func TestWorkerRPC_MaxBodyLimit(t *testing.T) {
	mock := &mockWorkerHandler{}
	token := "secret-test-token-123"
	server, err := workerrpc.NewServer(mock, token, workerrpc.WithMaxBodyBytes(128))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	hugePayload := strings.Repeat("x", 512)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/rpc/history-jobs", strings.NewReader(`{"fromDay":"`+hugePayload+`"}`))
	req.Header.Set(workerrpc.HeaderInternalToken, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /rpc/history-jobs with huge body: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("expected error status code for exceeded body size, got %d", resp.StatusCode)
	}
}

func TestWorkerRPC_RequestIDPropagation(t *testing.T) {
	mock := &mockWorkerHandler{}
	token := "secret-test-token-123"
	server, err := workerrpc.NewServer(mock, token)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, token)
	customID := "test-request-id-999"
	ctx := workerrpc.WithRequestID(context.Background(), customID)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/rpc/request-sync", nil)
	req.Header.Set(workerrpc.HeaderInternalToken, token)
	req.Header.Set(workerrpc.HeaderRequestID, customID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	returnedID := resp.Header.Get(workerrpc.HeaderRequestID)
	if returnedID != customID {
		t.Fatalf("expected X-Request-Id %q, got %q", customID, returnedID)
	}

	// Verify with client
	if err := client.RequestSync(ctx); err != nil {
		t.Fatalf("client RequestSync: %v", err)
	}
}

func TestWorkerRPC_BoundedConcurrency(t *testing.T) {
	mock := &mockWorkerHandler{
		syncBlock: make(chan struct{}),
	}
	token := "secret-test-token-123"

	// Server with concurrency limit of 1
	server, err := workerrpc.NewServer(mock, token, workerrpc.WithMaxConcurrent(1))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, token)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = client.RequestSync(context.Background())
	}()

	// Wait briefly for first request to enter handler
	time.Sleep(50 * time.Millisecond)

	// Second concurrent request should be rejected with 429
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/rpc/wake-dispatcher", nil)
	req.Header.Set(workerrpc.HeaderInternalToken, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("wake request: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected 429 Too Many Requests, got %d", resp.StatusCode)
	}

	close(mock.syncBlock)
	wg.Wait()
}

func TestWorkerRPC_ReadyChecker(t *testing.T) {
	mock := &mockWorkerHandler{}
	token := "secret-test-token-123"
	server, err := workerrpc.NewServer(mock, token)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	readyErr := errors.New("database not ready")
	server.SetReadyChecker(func(ctx context.Context) error {
		return readyErr
	})

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, token)
	if err := client.Ready(context.Background()); err == nil {
		t.Fatal("expected Ready to fail when checker returns error, but got nil")
	}

	// Now set to healthy
	server.SetReadyChecker(func(ctx context.Context) error {
		return nil
	})
	if err := client.Ready(context.Background()); err != nil {
		t.Fatalf("expected Ready to succeed when checker returns nil, got: %v", err)
	}
}

func TestWorkerRPC_NotifyAndWakeErrors(t *testing.T) {
	mock := &mockWorkerHandler{
		settingsErr: errors.New("failed to notify settings"),
		wakeErr:     errors.New("failed to wake dispatcher"),
	}
	token := "secret-test-token-123"
	server, err := workerrpc.NewServer(mock, token)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, token)
	ctx := context.Background()

	if err := client.NotifySettingsChanged(ctx); err == nil {
		t.Fatal("expected NotifySettingsChanged to fail, got nil")
	}
	if err := client.WakeDispatcher(ctx); err == nil {
		t.Fatal("expected WakeDispatcher to fail, got nil")
	}
}
