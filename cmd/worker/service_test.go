package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/bark"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/maintenance"
	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/payments"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerstate"
)

const testChecksumKey = "worker-test-checksum"

func testHMAC(value string) string {
	mac := hmac.New(sha256.New, []byte(testChecksumKey))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func testSignedPayload(t *testing.T, data map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(normalized))
	for k := range normalized {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		var val string
		switch item := normalized[k].(type) {
		case nil:
		case string:
			val = item
		case float64:
			val = strconv.FormatFloat(item, 'f', -1, 64)
		default:
			b, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			val = string(b)
		}
		parts = append(parts, k+"="+val)
	}
	return testHMAC(strings.Join(parts, "&"))
}

func writeTestSigned(t *testing.T, w http.ResponseWriter, data map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":      "00",
		"desc":      "success",
		"data":      data,
		"signature": testSignedPayload(t, data),
	})
}

func setupWorkerTestStore(t *testing.T) (*storage.Store, *security.Keyring) {
	t.Helper()
	ctx := context.Background()
	keyDir := t.TempDir()
	keyPath := filepath.Join(keyDir, "master.key")
	rawKey := make([]byte, 32)
	for i := range rawKey {
		rawKey[i] = byte(i + 1)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(rawKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	kr, err := security.LoadKeyring(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, filepath.Join(keyDir, "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	store.WithKeyring(kr)
	return store, kr
}

type recordingSender struct {
	outcome          notification.Outcome
	calls            atomic.Int64
	deliveredPayload []byte
}

func (r *recordingSender) Send(ctx context.Context, req notification.SendRequest) notification.SendResult {
	r.calls.Add(1)
	r.deliveredPayload = req.EventPayload
	return notification.SendResult{Outcome: r.outcome, StatusCode: 200, LatencyMs: 1}
}
func TestWorkerService_WakePaymentReconciler(t *testing.T) {
	ws := &workerService{}
	if err := ws.WakePaymentReconciler(context.Background()); err == nil {
		t.Fatal("expected error when payments not initialized")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ws.WakePaymentReconciler(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestWorkerService_QuiesceDrainsPaymentsBeforeDispatcher(t *testing.T) {
	ctx := context.Background()
	store, _ := setupWorkerTestStore(t)
	defer store.Close()

	var drainOrder []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestSigned(t, w, map[string]any{
			"id": "link-1", "orderCode": int64(100000000001), "amount": 50000, "status": "PENDING",
			"transactions": []any{},
		})
	}))
	defer server.Close()

	provider, err := payments.NewPayOS("client-1", "api-1", testChecksumKey, payments.WithBaseURL(server.URL), payments.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{PayOSClientID: "client-1", PayOSAPIKey: "api-1", PayOSChecksumKey: testChecksumKey, PaymentsEnabled: true, PayOSWebhookConfirmed: true}
	paymentService := payments.NewService(cfg, store, provider, nil)

	registry := notification.NewRegistry()
	sender := &recordingSender{outcome: notification.OutcomeSuccess}
	registry.Register("WEBHOOK", sender)
	dispatcher := notification.NewDispatcher(store, registry)

	coordinator := workerstate.NewCoordinator()
	if err := coordinator.SetReady(); err != nil {
		t.Fatal(err)
	}

	maintRunner := maintenance.NewRunner(store)
	ws := &workerService{
		payments:    paymentService,
		dispatcher:  dispatcher,
		store:       store,
		coordinator: coordinator,
		maintRunner: maintRunner,
	}

	qResp, err := ws.Quiesce(ctx)
	if err != nil {
		t.Fatalf("quiesce failed: %v", err)
	}
	if !qResp.Quiesced || qResp.Status != "quiesced" || qResp.Dispatcher != "IDLE" {
		t.Fatalf("unexpected quiesce response: %+v", qResp)
	}
	_ = drainOrder

	if err := ws.Resume(ctx); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
}

func TestWorkerService_QuiesceReportsDurableWatermarkAndReconcilerState(t *testing.T) {
	ctx := context.Background()
	store, _ := setupWorkerTestStore(t)
	defer store.Close()

	seq, err := store.AppendJournalEvent(ctx, "ep1", "test.event", "agg_1", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{PayOSClientID: "client-1", PayOSAPIKey: "api-1", PayOSChecksumKey: testChecksumKey}
	paymentService := payments.NewService(cfg, store, nil, nil)
	dispatcher := notification.NewDispatcher(store, notification.NewRegistry())
	coordinator := workerstate.NewCoordinator()
	_ = coordinator.SetReady()

	ws := &workerService{
		payments:    paymentService,
		dispatcher:  dispatcher,
		store:       store,
		coordinator: coordinator,
	}

	resp, err := ws.Quiesce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resp.JournalSeq != seq {
		t.Fatalf("expected journal sequence %d, got %d", seq, resp.JournalSeq)
	}
	if resp.ActivePaymentRequests != 0 {
		t.Fatalf("expected 0 active payment requests, got %d", resp.ActivePaymentRequests)
	}
}

func TestWorkerService_NotificationProviderMetadata(t *testing.T) {
	ctx := context.Background()

	wsUnconf := &workerService{barkSender: nil, barkPublicURL: ""}
	respUnconf, err := wsUnconf.NotificationProviderMetadata(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(respUnconf.Providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(respUnconf.Providers))
	}
	for _, p := range respUnconf.Providers {
		if p.ID == "BARK" && (p.Configured || p.Status != "unconfigured") {
			t.Fatalf("expected bark unconfigured: %+v", p)
		}
		if p.ID == "WEBHOOK" && !p.Configured {
			t.Fatalf("expected webhook configured: %+v", p)
		}
	}

	barkSender := bark.NewSender(bark.Config{
		ServerURL: "http://127.0.0.1:8080",
		PublicURL: "https://bark.example.com",
	}, nil, "")
	wsConf := &workerService{barkSender: barkSender, barkPublicURL: "https://bark.example.com"}
	respConf, err := wsConf.NotificationProviderMetadata(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var barkFound bool
	for _, p := range respConf.Providers {
		if p.ID == "BARK" {
			barkFound = true
			if !p.Configured || p.Status != "configured" || p.PublicURL != "https://bark.example.com" {
				t.Fatalf("unexpected bark status: %+v", p)
			}
		}
	}
	if !barkFound {
		t.Fatal("BARK provider not found")
	}
}

func TestWorkerService_TestNotificationChannelDeliversKienlongPayload(t *testing.T) {
	ctx := context.Background()
	store, _ := setupWorkerTestStore(t)
	defer store.Close()

	ch, err := store.CreateEndpointWithSecret(ctx, "Test Hook", "https://example.com/webhook")
	if err != nil {
		t.Fatal(err)
	}

	sender := &recordingSender{outcome: notification.OutcomeSuccess}
	registry := notification.NewRegistry()
	registry.Register("WEBHOOK", sender)
	ws := &workerService{
		store:                store,
		notificationRegistry: registry,
	}

	resp, err := ws.TestNotificationChannel(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Success || resp.Status != "DELIVERED" {
		t.Fatalf("test channel delivery failed: %+v", resp)
	}
	if !strings.Contains(string(sender.deliveredPayload), `"bank":"KienlongBank"`) || !strings.Contains(string(sender.deliveredPayload), `"provider":"PAYOS"`) {
		t.Fatalf("delivered payload missing provider/bank metadata: %s", string(sender.deliveredPayload))
	}
}

func TestWorkerPaymentNotifier_RealtimeHubAndDispatcherWaked(t *testing.T) {
	hub := eventhub.New()
	_, ch, cancel := hub.Subscribe()
	defer cancel()

	waker := &mockWaker{}
	notifier := newWorkerPaymentNotifier(waker, hub)

	event := storage.EventNotification{
		JournalSeq:    101,
		Epoch:         "ep1",
		EventType:     "bank.transaction.credit",
		TransactionID: "txn_test",
		Payload:       []byte(`{"amount":50000}`),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	notifier(event)

	select {
	case received := <-ch:
		if received.Seq != 101 || received.AggregateID != "txn_test" || string(received.Payload) != `{"amount":50000}` {
			t.Fatalf("unexpected event received: %+v", received)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for published realtime event")
	}

	if waker.wakeCount != 1 {
		t.Fatalf("expected dispatcher wake count 1, got %d", waker.wakeCount)
	}
}
