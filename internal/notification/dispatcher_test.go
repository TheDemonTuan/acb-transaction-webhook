package notification

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type blockingSender struct {
	started chan struct{}
	release chan struct{}
}

func (s *blockingSender) Send(context.Context, SendRequest) SendResult {
	close(s.started)
	<-s.release
	return SendResult{Outcome: OutcomeSuccess, StatusCode: 200}
}

type mockSender struct {
	outcome           Outcome
	statusCode        int
	providerErrorCode string
	sanitizedError    string
	calls             int
}

func (m *mockSender) Send(ctx context.Context, req SendRequest) SendResult {
	m.calls++
	return SendResult{
		Outcome:           m.outcome,
		StatusCode:        m.statusCode,
		LatencyMs:         10,
		ProviderErrorCode: m.providerErrorCode,
		SanitizedError:    m.sanitizedError,
	}
}

func setupTestStoreWithKeyring(t *testing.T) *storage.Store {
	t.Helper()
	ctx := context.Background()
	keyPath := filepath.Join(t.TempDir(), "master.key")
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

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	store.WithKeyring(kr)
	return store
}

// settleDispatcherPayment exercises the real payment/receipt/event/outbox commit.
func settleDispatcherPayment(t *testing.T, store *storage.Store, reference string, amount int64, transactionAt, description string) {
	t.Helper()
	ctx := context.Background()
	paidAt, err := time.Parse(time.RFC3339, transactionAt)
	if err != nil {
		t.Fatal(err)
	}
	order, created, err := store.ReservePaymentOrder(ctx, storage.PaymentOrderIntent{
		ChannelID: "dispatcher-test-channel", IdempotencyKey: reference, RequestHash: reference,
		AmountVnd: amount, Origin: "OPERATOR_DYNAMIC",
	})
	if err != nil || !created {
		t.Fatalf("reserve payment: created=%v err=%v", created, err)
	}
	linkID, account := "link-"+reference, "va-"+reference
	if _, err := store.CompletePaymentOrderOperation(ctx, order.ID, order.OperationToken, storage.PaymentOrderUpdate{
		Status: "PENDING", PaymentLinkID: linkID, AccountNumber: account,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := store.SettlePayment(ctx, storage.SettlementInput{
		ChannelID: order.ChannelID, OrderCode: order.OrderCode, PaymentLinkID: linkID,
		Reference: reference, AmountVnd: amount, TransactionAt: paidAt, Description: description,
		VirtualAccountNumber: account, Source: "REALTIME",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Order.Status != "PAID" || result.Order.TransactionID == "" || result.Event == nil || result.Duplicate || result.ReviewReason != "" {
		t.Fatalf("payment did not commit a new credit event: %+v", result)
	}
}

func TestDispatcherMixedProviders(t *testing.T) {
	ctx := context.Background()
	store := setupTestStoreWithKeyring(t)
	defer store.Close()

	// Each active provider receives an outbox delivery from the same settlement.

	wh, _ := store.CreateEndpointWithSecret(ctx, "Test Hook", "https://example.com/webhook")
	_ = store.SetEndpointStatus(ctx, wh.ID, "ACTIVE")

	barkCh, _ := store.CreateBarkChannel(ctx, "Test iPhone", "device_key_123", nil)
	_ = store.SetEndpointStatus(ctx, barkCh.ID, "ACTIVE")

	settleDispatcherPayment(t, store, "MIX_001", 100000, "2026-09-13T12:00:00Z", "Test mixed dispatch")

	registry := NewRegistry()
	whSender := &mockSender{outcome: OutcomeSuccess, statusCode: 200}
	barkSender := &mockSender{outcome: OutcomeSuccess, statusCode: 200}
	registry.Register("WEBHOOK", whSender)
	registry.Register("BARK", barkSender)

	dispatcher := NewDispatcher(store, registry)

	// Dispatch 1
	p1, err := dispatcher.DispatchOne(ctx)
	if err != nil || !p1 {
		t.Fatalf("first dispatch failed: %v", err)
	}

	// Dispatch 2
	p2, err := dispatcher.DispatchOne(ctx)
	if err != nil || !p2 {
		t.Fatalf("second dispatch failed: %v", err)
	}

	// Dispatch 3 -> nothing left
	p3, err := dispatcher.DispatchOne(ctx)
	if err != nil || p3 {
		t.Fatalf("expected queue empty, got: %v", p3)
	}

	if whSender.calls != 1 || barkSender.calls != 1 {
		t.Fatalf("expected 1 call each, got wh=%d bark=%d", whSender.calls, barkSender.calls)
	}

	summary, _ := store.DeliverySummary(ctx)
	if summary.Pending != 0 || summary.DeadLetter != 0 {
		t.Fatalf("expected 0 pending and 0 deadletter, got: %+v", summary)
	}
}

func TestDispatcherRetryExhaustionAndReplay(t *testing.T) {
	ctx := context.Background()
	store := setupTestStoreWithKeyring(t)
	defer store.Close()

	barkCh, _ := store.CreateBarkChannel(ctx, "Failing iPhone", "key_fail", nil)
	_ = store.SetEndpointStatus(ctx, barkCh.ID, "ACTIVE")

	settleDispatcherPayment(t, store, "FAIL_001", 200000, "2026-09-13T12:00:00Z", "Test fail dispatch")

	registry := NewRegistry()
	failingSender := &mockSender{outcome: OutcomeRetry, statusCode: 502, providerErrorCode: "HTTP_502"}
	registry.Register("BARK", failingSender)

	dispatcher := NewDispatcher(store, registry)
	dispatcher.SetMaxRetries(2)

	// Attempt 1 -> OutcomeRetry
	p1, err := dispatcher.DispatchOne(ctx)
	if err != nil || !p1 {
		t.Fatal(err)
	}

	// Make past due
	_, _ = store.DB().ExecContext(ctx, `UPDATE deliveries SET next_attempt_at = datetime('now', '-1 minute')`)

	// Attempt 2 -> OutcomeRetry, but attempts (2) >= maxRetries (2) -> DEAD_LETTER
	p2, err := dispatcher.DispatchOne(ctx)
	if err != nil || !p2 {
		t.Fatal(err)
	}

	summary, _ := store.DeliverySummary(ctx)
	if summary.DeadLetter != 1 {
		t.Fatalf("expected 1 deadletter, got: %+v", summary)
	}

	var deliveryID string
	_ = store.DB().QueryRowContext(ctx, `SELECT id FROM deliveries WHERE status = 'DEAD_LETTER'`).Scan(&deliveryID)

	// Replay delivery
	err = store.ReplayDelivery(ctx, deliveryID)
	if err != nil {
		t.Fatalf("ReplayDelivery failed: %v", err)
	}

	// Sender now succeeds
	failingSender.outcome = OutcomeSuccess
	failingSender.statusCode = 200

	p3, err := dispatcher.DispatchOne(ctx)
	if err != nil || !p3 {
		t.Fatalf("dispatch after replay failed: %v", err)
	}

	summary, _ = store.DeliverySummary(ctx)
	if summary.Pending != 0 || summary.DeadLetter != 0 {
		t.Fatalf("expected completed delivery, got: %+v", summary)
	}
}

func TestDispatcherDrainWaitsForInFlightSend(t *testing.T) {
	ctx := context.Background()
	store := setupTestStoreWithKeyring(t)
	defer store.Close()
	endpoint, err := store.CreateEndpointWithSecret(ctx, "Drain Hook", "https://example.com/webhook")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEndpointStatus(ctx, endpoint.ID, "ACTIVE"); err != nil {
		t.Fatal(err)
	}
	settleDispatcherPayment(t, store, "DRAIN_001", 100, "2026-09-15T00:00:00Z", "Test drain dispatch")

	sender := &blockingSender{started: make(chan struct{}), release: make(chan struct{})}
	registry := NewRegistry()
	registry.Register(ProviderWebhook, sender)
	dispatcher := NewDispatcher(store, registry).SetWorkers(1)
	result := make(chan error, 1)
	go func() {
		_, err := dispatcher.DispatchOne(ctx)
		result <- err
	}()
	<-sender.started

	drainCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := dispatcher.Drain(drainCtx); err != context.DeadlineExceeded {
		t.Fatalf("expected drain deadline while send is active, got %v", err)
	}
	if dispatcher.ActiveDeliveries() != 1 {
		t.Fatalf("expected one active delivery, got %d", dispatcher.ActiveDeliveries())
	}
	close(sender.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if !dispatcher.IsPaused() || dispatcher.ActiveDeliveries() != 0 {
		t.Fatalf("dispatcher was not safely drained: paused=%v active=%d", dispatcher.IsPaused(), dispatcher.ActiveDeliveries())
	}
}
