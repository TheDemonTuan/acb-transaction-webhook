package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func signedWebhookBody(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	return map[string]any{"code": "00", "success": true, "data": data, "signature": fixtureSignature(t, data)}
}

func TestHandleWebhookBeforeCreateBindWakesAfterInboxCommit(t *testing.T) {
	store := createTestStore(t)
	order, created, err := store.ReservePaymentOrder(context.Background(), storage.PaymentOrderIntent{ChannelID: "fixture-client", IdempotencyKey: createKey, RequestHash: "fixture-request", AmountVnd: 50000, Origin: "STATIC_URL"})
	if err != nil || !created {
		t.Fatalf("reserve %+v %v", order, err)
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unbound webhook made provider request")
		w.WriteHeader(500)
	}))
	defer fixture.Close()
	var wakes atomic.Int64
	service := createTestService(t, store, fixture).WithReconcileWake(func(ctx context.Context) error {
		var inbox int
		if err := store.DB().QueryRowContext(ctx, "SELECT count(*) FROM payos_webhook_inbox WHERE reason='AWAITING_ORDER_BIND'").Scan(&inbox); err != nil || inbox != 1 {
			t.Errorf("wake before durable inbox %d %v", inbox, err)
		}
		wakes.Add(1)
		return errors.New("fixture worker unavailable")
	})
	data := webhookFixture()
	data["orderCode"] = order.OrderCode
	body := signedWebhookBody(t, data)
	for range 3 {
		if err := service.HandleWebhook(context.Background(), body); err != nil {
			t.Fatalf("committed review returned failure: %v", err)
		}
	}
	var receipts, inbox int
	if err := store.DB().QueryRow("SELECT count(*) FROM payment_receipts").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow("SELECT count(*) FROM payos_webhook_inbox").Scan(&inbox); err != nil {
		t.Fatal(err)
	}
	current, err := store.PaymentOrder(context.Background(), order.ID)
	if err != nil || current.Status != "CREATING" || current.PaymentLinkID != "" || receipts != 0 || inbox != 1 || wakes.Load() != 3 {
		t.Fatalf("premature binding current=%+v receipts=%d inbox=%d wakes=%d err=%v", current, receipts, inbox, wakes.Load(), err)
	}
}

func TestHandleWebhookIssuedOrderIgnoresCreateReadinessFlags(t *testing.T) {
	store := createTestStore(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeCreateRequest(t, r)
		writeSignedFixture(t, w, signedCreateData(request))
	}))
	defer fixture.Close()
	creator := createTestService(t, store, fixture)
	order, _, err := creator.CreateOrder(context.Background(), 50000, "STATIC_URL", createKey)
	if err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int64
	service := NewService(config.Config{PayOSClientID: "fixture-client", PayOSAPIKey: "fixture-api-key", PayOSChecksumKey: fixtureChecksumKey, PaymentsEnabled: false, PayOSWebhookConfirmed: false}, store, fixtureAdapter(t, fixture), func(storage.EventNotification) { commits.Add(1) })
	data := webhookFixture()
	data["orderCode"] = order.OrderCode
	if err := service.HandleWebhook(context.Background(), signedWebhookBody(t, data)); err != nil {
		t.Fatal(err)
	}
	current, err := store.PaymentOrder(context.Background(), order.ID)
	if err != nil || current.Status != "PAID" || commits.Load() != 1 {
		t.Fatalf("disabled creation flags lost issued settlement %+v %v", current, err)
	}
}

func TestHandleWebhookSafeIntegerCanonicalInboxAndUnknownSignedFields(t *testing.T) {
	store := createTestStore(t)
	fixture := httptest.NewServer(http.NotFoundHandler())
	defer fixture.Close()
	service := createTestService(t, store, fixture)
	data := webhookFixture()
	data["amount"] = json.Number("5e4")
	data["orderCode"] = json.Number("100000000001.0")
	body := signedWebhookBody(t, data)
	if err := service.HandleWebhook(context.Background(), body); err != nil {
		t.Fatalf("safe integer lexical form failed: %v", err)
	}
	data = webhookFixture()
	if err := service.HandleWebhook(context.Background(), signedWebhookBody(t, data)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRow("SELECT count(*) FROM payos_webhook_inbox").Scan(&count); err != nil || count != 1 {
		t.Fatalf("noncanonical duplicate count=%d err=%v", count, err)
	}
	body = signedWebhookBody(t, webhookFixture())
	body["data"].(map[string]any)["unknownSignedField"] = map[string]any{"changed": true}
	if err := service.HandleWebhook(context.Background(), body); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("unknown signed field discarded: %v", err)
	}
	if err := store.DB().QueryRow("SELECT count(*) FROM payos_webhook_inbox").Scan(&count); err != nil || count != 1 {
		t.Fatalf("invalid signature persisted: %d %v", count, err)
	}
}

func TestHandleWebhookQuiesceRetainsProviderRetryContract(t *testing.T) {
	store := createTestStore(t)
	fixture := httptest.NewServer(http.NotFoundHandler())
	defer fixture.Close()
	service := createTestService(t, store, fixture)
	if err := service.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleWebhook(context.Background(), signedWebhookBody(t, webhookFixture())); !errors.Is(err, ErrWebhookUnavailable) {
		t.Fatalf("quiesced callback ACK: %v", err)
	}
	var inbox int
	if err := store.DB().QueryRow("SELECT count(*) FROM payos_webhook_inbox").Scan(&inbox); err != nil || inbox != 0 {
		t.Fatalf("quiesced inbox=%d err=%v", inbox, err)
	}
	service.Resume()
	if err := service.HandleWebhook(context.Background(), signedWebhookBody(t, webhookFixture())); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow("SELECT count(*) FROM payos_webhook_inbox").Scan(&inbox); err != nil || inbox != 1 || service.ActiveRequests() != 0 {
		t.Fatalf("resumed inbox=%d active=%d err=%v", inbox, service.ActiveRequests(), err)
	}
}
