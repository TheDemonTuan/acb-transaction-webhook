package integration_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
	"github.com/thedemontuan/acb-transaction-webhook/internal/payments"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// Gate acquisition overlaps a real provider request. New creates and signed
// settlements are rejected without financial writes, then replay after resume
// preserves the already issued order and the in-flight creation intent.
func TestConcurrentPaymentAndDeployAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f := newPaymentFixture(t, store, nil)
	issued := createPayment(t, f, 50000, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	f.beforeCreate = func(requestCtx context.Context, _ payos.CreatePaymentLinkRequest) {
		close(started)
		select {
		case <-release:
		case <-requestCtx.Done():
		}
	}
	type createResult struct {
		order   storage.PaymentOrder
		created bool
		err     error
	}
	const pendingKey = "00000000-0000-4000-8000-000000000002"
	result := make(chan createResult, 1)
	go func() {
		order, created, err := f.service.CreateOrder(ctx, 120000, "OPERATOR_DYNAMIC", pendingKey)
		result <- createResult{order, created, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("provider create did not start")
	}
	pending, err := store.PaymentOrderByKey(ctx, "integration-channel", pendingKey)
	if err != nil || pending.Status != "CREATING" || pending.OperationToken == "" {
		t.Fatalf("in-flight intent not durable: %+v err=%v", pending, err)
	}
	gate, err := store.AcquireMutationGate(ctx, "integration-deploy", time.Minute, "payment cutover")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.ReleaseMutationGate(context.Background(), "integration-deploy", gate.LeaseToken) }()
	// These operations run together while another creation is still in flight.
	var wg sync.WaitGroup
	wg.Add(2)
	var createErr error
	var webhookStatus int
	var retryAfter string
	go func() {
		defer wg.Done()
		_, _, createErr = f.service.CreateOrder(ctx, 3000, "STATIC_URL", "00000000-0000-4000-8000-000000000003")
	}()
	body := paymentWebhookBody(t, issued)
	go func() {
		defer wg.Done()
		w := postPaymentWebhook(t, f.handler, body)
		webhookStatus, retryAfter = w.Code, w.Header().Get("Retry-After")
	}()
	wg.Wait()
	if !errors.Is(createErr, payments.ErrPaymentUnavailable) || webhookStatus != http.StatusServiceUnavailable || retryAfter != "5" {
		t.Fatalf("gate did not reject mutations: create=%v webhook=%d retryAfter=%s", createErr, webhookStatus, retryAfter)
	}
	if calls := f.creates.Load(); calls != 2 {
		t.Fatalf("locked gate called provider: calls=%d", calls)
	}
	for _, table := range []string{"payment_receipts", "transactions", "events", "event_journal", "payos_webhook_inbox"} {
		var count int
		if err := store.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("gate wrote %s: count=%d err=%v", table, count, err)
		}
	}
	var orders int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM payment_orders`).Scan(&orders); err != nil || orders != 2 {
		t.Fatalf("rejected creation left intent: count=%d err=%v", orders, err)
	}
	if err := store.ReleaseMutationGate(ctx, "integration-deploy", gate.LeaseToken); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case completed := <-result:
		if completed.err != nil || !completed.created || completed.order.ID != pending.ID || completed.order.OrderCode != pending.OrderCode || completed.order.Status != "PENDING" {
			t.Fatalf("in-flight create changed intent: %+v", completed)
		}
	case <-ctx.Done():
		t.Fatal("in-flight creation did not drain")
	}
	settlePayment(t, f, issued)
	settlePayment(t, f, issued)
	replayed, created, err := f.service.CreateOrder(ctx, 120000, "OPERATOR_DYNAMIC", pendingKey)
	if err != nil || created || replayed.ID != pending.ID || replayed.OrderCode != pending.OrderCode || f.creates.Load() != 2 {
		t.Fatalf("resume replay created another intent: order=%+v created=%v calls=%d err=%v", replayed, created, f.creates.Load(), err)
	}
	for _, table := range []string{"payment_receipts", "transactions", "events", "event_journal"} {
		var count int
		if err := store.DB().QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("resume replay duplicated %s: count=%d err=%v", table, count, err)
		}
	}
}
