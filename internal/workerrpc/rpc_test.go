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

	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

type mockWorkerHandler struct {
	syncCalled            bool
	ensureHistoryFrom     string
	ensureHistoryTo       string
	settingsChangedCalled bool
	wakeDispatcherCalled  bool
	verifiedAccount       string

	settingsErr error
	wakeErr     error
	syncBlock   chan struct{}
}

func (m *mockWorkerHandler) RequestSync(ctx context.Context) error {
	m.syncCalled = true
	if m.syncBlock != nil {
		<-m.syncBlock
	}
	return nil
}

func (m *mockWorkerHandler) EnsureHistory(ctx context.Context, fromDay, toDay string) (int, error) {
	m.ensureHistoryFrom = fromDay
	m.ensureHistoryTo = toDay
	return 42, nil
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

	// 2. EnsureHistory
	count, err := client.EnsureHistory(ctx, "2026-09-01", "2026-09-10")
	if err != nil {
		t.Fatalf("EnsureHistory failed: %v", err)
	}
	if count != 42 || mock.ensureHistoryFrom != "2026-09-01" || mock.ensureHistoryTo != "2026-09-10" {
		t.Errorf("EnsureHistory unexpected result: count=%d, from=%s, to=%s", count, mock.ensureHistoryFrom, mock.ensureHistoryTo)
	}

	// 3. NotifySettingsChanged
	if err := client.NotifySettingsChanged(ctx); err != nil {
		t.Fatalf("NotifySettingsChanged failed: %v", err)
	}
	if !mock.settingsChangedCalled {
		t.Errorf("expected NotifySettingsChanged to be called")
	}

	// 4. WakeDispatcher
	if err := client.WakeDispatcher(ctx); err != nil {
		t.Fatalf("WakeDispatcher failed: %v", err)
	}
	if !mock.wakeDispatcherCalled {
		t.Errorf("expected WakeDispatcher to be called")
	}

	// 5. VerifySession success
	if err := client.VerifySession(ctx, "12345678", 1, []byte("correct")); err != nil {
		t.Fatalf("VerifySession failed: %v", err)
	}
	if mock.verifiedAccount != "12345678" {
		t.Errorf("expected account to match")
	}

	// 6. VerifySession failure
	if err := client.VerifySession(ctx, "12345678", 1, []byte("wrong")); err == nil {
		t.Fatalf("expected VerifySession with wrong password to fail")
	}

	// 7. Unauthorized client
	unauthClient := workerrpc.NewClient(ts.URL, "bad-token")
	if err := unauthClient.RequestSync(ctx); err == nil {
		t.Fatalf("expected unauthenticated request to fail")
	}
}

func TestWorkerRPC_MethodValidation(t *testing.T) {
	mock := &mockWorkerHandler{}
	token := "secret-test-token-123"
	server, err := workerrpc.NewServer(mock, token)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/rpc/request-sync", nil)
	req.Header.Set(workerrpc.HeaderInternalToken, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /rpc/request-sync: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", resp.StatusCode)
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
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/rpc/ensure-history", strings.NewReader(`{"fromDay":"`+hugePayload+`"}`))
	req.Header.Set(workerrpc.HeaderInternalToken, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /rpc/ensure-history with huge body: %v", err)
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
		t.Fatal("expected NotifySettingsChanged to return error from server, got nil")
	}
	if err := client.WakeDispatcher(ctx); err == nil {
		t.Fatal("expected WakeDispatcher to return error from server, got nil")
	}
}
