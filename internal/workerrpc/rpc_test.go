package workerrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerstate"
)

type mockWorkerHandler struct {
	wakeDispatcherCalls        int
	wakePaymentReconcilerCalls int
	testNotificationCalls      int
	quiesceCalls               int
	resumeCalls                int
	quiesceResp                workerrpc.QuiesceResponse
	testNotificationResp       workerrpc.TestNotificationResponse
	wakeErr                    error
	quiesceErr                 error
	onWakeDispatcher           func(context.Context)
}

func (m *mockWorkerHandler) WakeDispatcher(ctx context.Context) error {
	m.wakeDispatcherCalls++
	if m.onWakeDispatcher != nil {
		m.onWakeDispatcher(ctx)
	}
	return m.wakeErr
}

func (m *mockWorkerHandler) WakePaymentReconciler(ctx context.Context) error {
	m.wakePaymentReconcilerCalls++
	return m.wakeErr
}

func (m *mockWorkerHandler) TestNotificationChannel(ctx context.Context, channelID string) (workerrpc.TestNotificationResponse, error) {
	m.testNotificationCalls++
	return m.testNotificationResp, nil
}

func (m *mockWorkerHandler) Quiesce(ctx context.Context) (workerrpc.QuiesceResponse, error) {
	m.quiesceCalls++
	if m.quiesceErr != nil {
		return workerrpc.QuiesceResponse{}, m.quiesceErr
	}
	return m.quiesceResp, nil
}

func (m *mockWorkerHandler) Resume(ctx context.Context) error {
	m.resumeCalls++
	return nil
}

func TestWorkerRPC_ConstructorValidation(t *testing.T) {
	mock := &mockWorkerHandler{}

	if _, err := workerrpc.NewServer(nil, "token"); err == nil {
		t.Fatal("expected error with nil handler")
	}
	if _, err := workerrpc.NewServer(mock, ""); err == nil {
		t.Fatal("expected error with empty token")
	}
	if _, err := workerrpc.NewServer(mock, "   "); err == nil {
		t.Fatal("expected error with whitespace token")
	}
	if srv, err := workerrpc.NewServer(mock, "valid-token"); err != nil || srv == nil {
		t.Fatalf("unexpected error with valid args: %v", err)
	}
}

func TestWorkerRPC_Roundtrip(t *testing.T) {
	mock := &mockWorkerHandler{
		quiesceResp: workerrpc.QuiesceResponse{
			Status:                "quiesced",
			Quiesced:              true,
			Dispatcher:            "IDLE",
			ActiveDeliveries:      0,
			ActivePaymentRequests: 0,
			JournalSeq:            123,
		},
		testNotificationResp: workerrpc.TestNotificationResponse{
			Success: true,
			Status:  "DELIVERED",
		},
	}
	srv, err := workerrpc.NewServer(mock, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, "secret-token")
	ctx := context.Background()

	if err := client.WakeDispatcher(ctx); err != nil {
		t.Fatalf("WakeDispatcher: %v", err)
	}
	if mock.wakeDispatcherCalls != 1 {
		t.Fatalf("expected 1 wake dispatcher call, got %d", mock.wakeDispatcherCalls)
	}

	if err := client.WakePaymentReconciler(ctx); err != nil {
		t.Fatalf("WakePaymentReconciler: %v", err)
	}
	if mock.wakePaymentReconcilerCalls != 1 {
		t.Fatalf("expected 1 wake payment reconciler call, got %d", mock.wakePaymentReconcilerCalls)
	}

	qResp, err := client.Quiesce(ctx)
	if err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if !qResp.Quiesced || qResp.JournalSeq != 123 || qResp.Dispatcher != "IDLE" {
		t.Fatalf("unexpected quiesce resp: %+v", qResp)
	}

	if err := client.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	nResp, err := client.TestNotificationChannel(ctx, "ch_1")
	if err != nil {
		t.Fatalf("TestNotificationChannel: %v", err)
	}
	if !nResp.Success || nResp.Status != "DELIVERED" {
		t.Fatalf("unexpected notification resp: %+v", nResp)
	}
}

func TestWorkerRPC_AuthFailClosed(t *testing.T) {
	mock := &mockWorkerHandler{}
	srv, err := workerrpc.NewServer(mock, "correct-token")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	clientWrong := workerrpc.NewClient(ts.URL, "wrong-token")
	err = clientWrong.WakeDispatcher(context.Background())
	if err == nil {
		t.Fatal("expected auth error with wrong token")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 status in error, got: %v", err)
	}

	clientEmpty := workerrpc.NewClient(ts.URL, "")
	err = clientEmpty.WakePaymentReconciler(context.Background())
	if err == nil {
		t.Fatal("expected auth error with empty token")
	}
}

func TestWorkerRPC_MaxBodyLimit(t *testing.T) {
	mock := &mockWorkerHandler{}
	srv, err := workerrpc.NewServer(mock, "token", workerrpc.WithMaxBodyBytes(64))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	largeBody := strings.Repeat("x", 256)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/rpc/notification-channels/test", strings.NewReader(`{"channelId":"`+largeBody+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(workerrpc.HeaderInternalToken, "token")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected rejection for oversized body, got %d", resp.StatusCode)
	}
}

func TestWorkerRPC_RequestIDPropagation(t *testing.T) {
	mock := &mockWorkerHandler{}
	srv, err := workerrpc.NewServer(mock, "token")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	customReqID := "req-trace-test-12345"
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/rpc/wake-dispatcher", strings.NewReader("{}"))
	req.Header.Set(workerrpc.HeaderInternalToken, "token")
	req.Header.Set(workerrpc.HeaderRequestID, customReqID)

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	gotReqID := resp.Header.Get(workerrpc.HeaderRequestID)
	if gotReqID != customReqID {
		t.Fatalf("expected request ID %q, got %q", customReqID, gotReqID)
	}
}

func TestWorkerRPC_BoundedConcurrency(t *testing.T) {
	started := make(chan struct{})
	block := make(chan struct{})
	mock := &mockWorkerHandler{
		onWakeDispatcher: func(ctx context.Context) {
			close(started)
			<-block
		},
	}
	srv, err := workerrpc.NewServer(mock, "token", workerrpc.WithMaxConcurrent(1))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	go func() {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/rpc/wake-dispatcher", nil)
		req.Header.Set(workerrpc.HeaderInternalToken, "token")
		resp, _ := ts.Client().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}()

	<-started
	defer close(block)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/rpc/payments/wake", nil)
	req.Header.Set(workerrpc.HeaderInternalToken, "token")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when max concurrency exceeded, got %d", resp.StatusCode)
	}
}

func TestWorkerRPC_ReadyChecker(t *testing.T) {
	mock := &mockWorkerHandler{}
	srv, err := workerrpc.NewServer(mock, "token")
	if err != nil {
		t.Fatal(err)
	}

	readyErr := errors.New("storage unready")
	srv.SetReadyChecker(func(ctx context.Context) error {
		return readyErr
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, "token")
	if err := client.Ready(context.Background()); err == nil {
		t.Fatal("expected Ready to return error when readyChecker fails")
	}

	readyErr = nil
	if err := client.Ready(context.Background()); err != nil {
		t.Fatalf("expected Ready to pass when readyChecker succeeds: %v", err)
	}
}

func TestWorkerRPC_DrainCommand(t *testing.T) {
	mock := &mockWorkerHandler{
		quiesceResp: workerrpc.QuiesceResponse{Status: "quiesced", Quiesced: true, Dispatcher: "IDLE"},
	}
	srv, err := workerrpc.NewServer(mock, "token")
	if err != nil {
		t.Fatal(err)
	}

	var drained bool
	srv.SetDrainHandler(func(ctx context.Context) error {
		drained = true
		return nil
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, "token")
	if err := client.Drain(context.Background()); err != nil {
		t.Fatalf("Drain call failed: %v", err)
	}
	if !drained {
		t.Fatal("drainHandler was not called")
	}
	if mock.quiesceCalls != 1 {
		t.Fatalf("expected Drain to trigger Quiesce, calls=%d", mock.quiesceCalls)
	}
}

func TestWorkerRPC_DrainingRejectsUpstreamCommands(t *testing.T) {
	mock := &mockWorkerHandler{}
	srv, err := workerrpc.NewServer(mock, "token")
	if err != nil {
		t.Fatal(err)
	}

	state := workerstate.StateDraining
	srv.SetStateProvider(func() workerstate.State {
		return state
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, "token")
	ctx := context.Background()

	if err := client.WakePaymentReconciler(ctx); err == nil {
		t.Fatal("expected WakePaymentReconciler to fail during drain")
	}

	if _, err := client.TestNotificationChannel(ctx, "ch1"); err == nil {
		t.Fatal("expected TestNotificationChannel to fail during drain")
	}
}

func TestWorkerRPC_WorkerRpcVersion_v3(t *testing.T) {
	mock := &mockWorkerHandler{}
	srv, err := workerrpc.NewServer(mock, "token")
	if err != nil {
		t.Fatal(err)
	}
	srv.SetRuntimeInfo("worker", "commit-456", "green", "14")

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/readyz", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["workerRpcVersion"] != "v3" {
		t.Fatalf("expected workerRpcVersion 'v3', got %v", body["workerRpcVersion"])
	}
}

type mockProviderReader struct {
	resp workerrpc.NotificationProvidersResponse
}

func (m *mockProviderReader) NotificationProviderMetadata(ctx context.Context) (workerrpc.NotificationProvidersResponse, error) {
	return m.resp, nil
}

func TestWorkerRPC_NotificationProviders_SuccessAndAuth(t *testing.T) {
	mock := &mockWorkerHandler{}
	srv, err := workerrpc.NewServer(mock, "token")
	if err != nil {
		t.Fatal(err)
	}
	srv.SetProviderReader(&mockProviderReader{
		resp: workerrpc.NotificationProvidersResponse{
			Providers: []workerrpc.NotificationProviderMetadata{
				{ID: "WEBHOOK", Name: "Webhook", Configured: true},
			},
		},
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := workerrpc.NewClient(ts.URL, "token")
	resp, err := client.NotificationProviderMetadata(context.Background())
	if err != nil {
		t.Fatalf("NotificationProviderMetadata: %v", err)
	}
	if len(resp.Providers) != 1 || resp.Providers[0].ID != "WEBHOOK" {
		t.Fatalf("unexpected providers: %+v", resp)
	}
}
