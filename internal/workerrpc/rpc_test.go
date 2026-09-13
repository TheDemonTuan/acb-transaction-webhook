package workerrpc_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

type mockWorkerHandler struct {
	syncCalled            bool
	ensureHistoryFrom     string
	ensureHistoryTo       string
	settingsChangedCalled bool
	wakeDispatcherCalled  bool
	verifiedAccount       string
}

func (m *mockWorkerHandler) RequestSync(ctx context.Context) error {
	m.syncCalled = true
	return nil
}

func (m *mockWorkerHandler) EnsureHistory(ctx context.Context, fromDay, toDay string) (int, error) {
	m.ensureHistoryFrom = fromDay
	m.ensureHistoryTo = toDay
	return 42, nil
}

func (m *mockWorkerHandler) NotifySettingsChanged() {
	m.settingsChangedCalled = true
}

func (m *mockWorkerHandler) WakeDispatcher() {
	m.wakeDispatcherCalled = true
}

func (m *mockWorkerHandler) VerifySession(ctx context.Context, account string, generation int64, password []byte) error {
	if string(password) == "wrong" {
		return errors.New("invalid password")
	}
	m.verifiedAccount = account
	return nil
}

func TestWorkerRPC_Roundtrip(t *testing.T) {
	mock := &mockWorkerHandler{}
	token := "secret-test-token-123"
	server := workerrpc.NewServer(mock, token)

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
	client.NotifySettingsChanged()
	if !mock.settingsChangedCalled {
		t.Errorf("expected NotifySettingsChanged to be called")
	}

	// 4. WakeDispatcher
	client.WakeDispatcher()
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
