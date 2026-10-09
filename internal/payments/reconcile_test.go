package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func reserveReconcileOrder(t *testing.T, store *storage.Store, status string, bound bool) storage.PaymentOrder {
	t.Helper()
	order, _, err := store.ReservePaymentOrder(context.Background(), storage.PaymentOrderIntent{ChannelID: "fixture-client", IdempotencyKey: createKey, RequestHash: paymentRequestHash(50000, "STATIC_URL"), AmountVnd: 50000, Origin: "STATIC_URL"})
	if err != nil {
		t.Fatal(err)
	}
	link, qr, account := "", "", ""
	if bound {
		link, qr, account = "fixture-link", "original-provider-qr", "VA-ORDER-1"
	}
	_, err = store.DB().Exec(`UPDATE payment_orders SET status=?,payment_link_id=NULLIF(?,''),qr_code=NULLIF(?,''),account_number=NULLIF(?,''),operation_token=NULL,operation_lease_until=NULL,next_reconcile_at=? WHERE id=?`, status, link, qr, account, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), order.ID)
	if err != nil {
		t.Fatal(err)
	}
	order, err = store.PaymentOrder(context.Background(), order.ID)
	if err != nil {
		t.Fatal(err)
	}
	return order
}

func reconcileLinkFixture(order storage.PaymentOrder, status string) map[string]any {
	data := linkFixture(status)
	data["orderCode"] = order.OrderCode
	if status != "PAID" {
		data["amountPaid"], data["amountRemaining"], data["transactions"] = 0, 50000, []any{}
	}
	return data
}

func loadReconcileOrder(t *testing.T, store *storage.Store, id string) storage.PaymentOrder {
	t.Helper()
	order, err := store.PaymentOrder(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return order
}

func assertNoPaymentReceipt(t *testing.T, store *storage.Store) {
	t.Helper()
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM payment_receipts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("receipts=%d err=%v", count, err)
	}
}

func TestReconcileHTTP404RecoversSameStoredRequest(t *testing.T) {
	store := createTestStore(t)
	order := reserveReconcileOrder(t, store, "CREATING", false)
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method == "GET" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "404", "desc": "absent"})
			return
		}
		req := decodeCreateRequest(t, r)
		expires, _ := time.Parse(time.RFC3339Nano, order.ExpiresAt)
		if req.OrderCode != order.OrderCode || req.Description != order.Description || req.Amount != 50000 || req.ExpiredAt == nil || int64(*req.ExpiredAt) != expires.Unix() {
			t.Errorf("recovery changed intent: %+v", req)
		}
		writeSignedFixture(t, w, signedCreateData(req))
	}))
	defer server.Close()
	svc := createTestService(t, store, server)
	if err := svc.reconcileDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := loadReconcileOrder(t, store, order.ID)
	if got.Status != "PENDING" || got.OrderCode != order.OrderCode || got.ID != order.ID || got.QRCode == "" {
		t.Fatalf("recovered snapshot: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || requests[0] != "GET /v2/payment-requests/"+strconv.FormatInt(order.OrderCode, 10) || requests[1] != "POST /v2/payment-requests" {
		t.Fatalf("requests=%v", requests)
	}
}

func TestReconcileProviderEnvelopeIsNotAbsenceAndBackoffSurvivesRestart(t *testing.T) {
	store := createTestStore(t)
	order := reserveReconcileOrder(t, store, "CREATING", false)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" {
			t.Errorf("unproven absence retried Create: %s", r.Method)
		}
		w.Header().Set("Retry-After", "120")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "UNDOCUMENTED", "desc": "not found duplicate or uncertain"})
	}))
	defer server.Close()
	svc := createTestService(t, store, server)
	before := time.Now()
	if err := svc.reconcileDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := loadReconcileOrder(t, store, order.ID)
	next, _ := time.Parse(time.RFC3339Nano, got.NextReconcileAt)
	if got.Status != "CREATING" || got.ReconcileAttempts != 1 || next.Before(before.Add(120*time.Second)) {
		t.Fatalf("schedule=%+v", got)
	}
	restarted := createTestService(t, store, server)
	if err := restarted.reconcileDue(context.Background()); err != nil || calls.Load() != 1 {
		t.Fatalf("restart discarded schedule: calls=%d err=%v", calls.Load(), err)
	}
	assertNoPaymentReceipt(t, store)
}

func TestReconcileMissingQROnceThenCancellationPersistsAcrossRestart(t *testing.T) {
	store := createTestStore(t)
	order := reserveReconcileOrder(t, store, "CREATING", false)
	var creates, cancels atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET":
			writeSignedFixture(t, w, reconcileLinkFixture(order, "PENDING"))
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			if cancels.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "UNKNOWN", "desc": "uncertain"})
				return
			}
			writeSignedFixture(t, w, reconcileLinkFixture(order, "CANCELLED"))
		default:
			creates.Add(1)
			persisted := loadReconcileOrder(t, store, order.ID)
			if !persisted.QRRecoveryAttempted {
				t.Error("recovery flag was not committed before provider Create")
			}
			req := decodeCreateRequest(t, r)
			data := signedCreateData(req)
			data["qrCode"] = ""
			writeSignedFixture(t, w, data)
		}
	}))
	defer server.Close()
	if err := createTestService(t, store, server).reconcileDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := loadReconcileOrder(t, store, order.ID)
	if !got.QRRecoveryAttempted || got.Status == "CANCELLED" || got.LastErrorCode != "QR_RECOVERY_CANCEL_PENDING" {
		t.Fatalf("uncertain Cancel falsely finalized: %+v", got)
	}
	_, err := store.DB().Exec(`UPDATE payment_orders SET next_reconcile_at=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = createTestService(t, store, server).reconcileDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = loadReconcileOrder(t, store, order.ID)
	if creates.Load() != 1 || cancels.Load() != 2 || got.Status != "CANCELLED" || got.LastErrorCode != "QR_RECOVERY_CANCELLED" || got.QRCode != "" {
		t.Fatalf("restart repeated recovery: %+v creates=%d cancels=%d", got, creates.Load(), cancels.Load())
	}
}

func TestReconcilePaidRequiresSingleCompleteTransaction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing transaction", func(d map[string]any) { d["transactions"] = []any{} }},
		{"multiple transfers", func(d map[string]any) { tx := d["transactions"].([]any)[0]; d["transactions"] = []any{tx, tx} }},
		{"missing reference", func(d map[string]any) { d["transactions"].([]any)[0].(map[string]any)["reference"] = "" }},
		{"invalid date", func(d map[string]any) {
			d["transactions"].([]any)[0].(map[string]any)["transactionDateTime"] = "invalid"
		}},
		{"missing receiving evidence", func(d map[string]any) {
			tx := d["transactions"].([]any)[0].(map[string]any)
			tx["accountNumber"], tx["virtualAccountNumber"] = "", ""
		}},
		{"amount paid differs", func(d map[string]any) { d["amountPaid"] = 49999 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := createTestStore(t)
			order := reserveReconcileOrder(t, store, "PENDING", true)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data := reconcileLinkFixture(order, "PAID")
				tc.mutate(data)
				writeSignedFixture(t, w, data)
			}))
			defer server.Close()
			if err := createTestService(t, store, server).reconcileDue(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := loadReconcileOrder(t, store, order.ID)
			if got.Status == "PAID" || got.LastErrorCode != "PAYMENT_DETAILS_PENDING" {
				t.Fatalf("invalid money settled: %+v", got)
			}
			assertNoPaymentReceipt(t, store)
		})
	}
}

func TestReconcilePendingStatesAndLatePaidWithOperationalFlagsOff(t *testing.T) {
	for _, status := range []string{"PENDING", "PROCESSING", "UNDERPAID", "CANCELLED", "EXPIRED"} {
		t.Run(status, func(t *testing.T) {
			store := createTestStore(t)
			order := reserveReconcileOrder(t, store, status, true)
			var paid atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerStatus := status
				if paid.Load() {
					providerStatus = "PAID"
				}
				writeSignedFixture(t, w, reconcileLinkFixture(order, providerStatus))
			}))
			defer server.Close()
			svc := createTestService(t, store, server)
			svc.cfg.PaymentsEnabled = false
			svc.cfg.PayOSWebhookConfirmed = false
			before := time.Now()
			if err := svc.reconcileDue(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := loadReconcileOrder(t, store, order.ID)
			if got.Status != status {
				t.Fatalf("state changed: %+v", got)
			}
			if status == "PENDING" || status == "PROCESSING" || status == "UNDERPAID" {
				next, _ := time.Parse(time.RFC3339Nano, got.NextReconcileAt)
				if next.Before(before.Add(60 * time.Second)) {
					t.Fatalf("pending schedule=%s", got.NextReconcileAt)
				}
			}
			paid.Store(true)
			if err := svc.reconcileOrder(context.Background(), order.ID); err != nil {
				t.Fatal(err)
			}
			got = loadReconcileOrder(t, store, order.ID)
			if got.Status != "PAID" || got.TransactionID == "" || got.NextReconcileAt != "" {
				t.Fatalf("late PAID not authoritative: %+v", got)
			}
			if err := svc.reconcileOrder(context.Background(), order.ID); err != nil {
				t.Fatal(err)
			}
			var receipts int
			_ = store.DB().QueryRow(`SELECT count(*) FROM payment_receipts`).Scan(&receipts)
			if receipts != 1 {
				t.Fatalf("receipts=%d", receipts)
			}
		})
	}
}

func TestReconcileInboxBindRequiresExactGetReceivingEvidence(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(strconv.FormatBool(mismatch), func(t *testing.T) {
			store := createTestStore(t)
			order := reserveReconcileOrder(t, store, "CREATING", false)
			data := webhookFixture()
			data["orderCode"] = order.OrderCode
			if mismatch {
				data["virtualAccountNumber"] = "DIFFERENT-VA"
			}
			hash, err := store.SaveVerifiedPaymentCallback(context.Background(), storage.VerifiedPaymentCallback{Data: data, OrderCode: order.OrderCode, PaymentLinkID: "fixture-link", Reference: "REF-1", Reason: "AWAITING_ORDER_BIND"})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeSignedFixture(t, w, reconcileLinkFixture(order, "PAID"))
			}))
			defer server.Close()
			svc := createTestService(t, store, server)
			var publications atomic.Int64
			svc.onCommit = func(storage.EventNotification) { publications.Add(1) }
			if err = svc.reconcileDue(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := loadReconcileOrder(t, store, order.ID)
			var processed string
			err = store.DB().QueryRow(`SELECT COALESCE(processed_at,'') FROM payos_webhook_inbox WHERE payload_hash=?`, hash).Scan(&processed)
			if err != nil {
				t.Fatal(err)
			}
			if mismatch {
				if got.Status == "PAID" || processed != "" {
					t.Fatalf("conflicting evidence settled: %+v", got)
				}
				assertNoPaymentReceipt(t, store)
			} else if got.Status != "PAID" || got.AccountNumber != "VA-ORDER-1" || got.PaymentLinkID != "fixture-link" || processed == "" || publications.Load() != 1 {
				t.Fatalf("bind not atomically consumed: %+v processed=%s publications=%d", got, processed, publications.Load())
			}
		})
	}
}

func TestReconcileUnknownPast24HoursPreservesReview(t *testing.T) {
	store := createTestStore(t)
	order := reserveReconcileOrder(t, store, "CREATING", false)
	_, err := store.DB().Exec(`UPDATE payment_orders SET expires_at=? WHERE id=?`, time.Now().Add(-25*time.Hour).UTC().Format(time.RFC3339Nano), order.ID)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "UNKNOWN", "desc": "unknown"})
	}))
	defer server.Close()
	if err = createTestService(t, store, server).reconcileDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := loadReconcileOrder(t, store, order.ID)
	if got.Status != "CREATING" || got.LastErrorCode != "CREATE_OUTCOME_UNKNOWN" {
		t.Fatalf("uncertain old intent discarded: %+v", got)
	}
	assertNoPaymentReceipt(t, store)
}

func TestReconcileQuiesceDrainsAdmittedGetAndResume(t *testing.T) {
	store := createTestStore(t)
	order := reserveReconcileOrder(t, store, "PENDING", true)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		writeSignedFixture(t, w, reconcileLinkFixture(order, "PENDING"))
	}))
	defer server.Close()
	defer unblock()
	svc := createTestService(t, store, server)
	finished := make(chan error, 1)
	go func() { finished <- svc.reconcileDue(context.Background()) }()
	<-entered
	if svc.ActiveRequests() != 1 {
		t.Fatal("request not tracked")
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := svc.Quiesce(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("quiesce did not wait: %v", err)
	}
	if err := svc.beginPaymentRequest(context.Background()); !errors.Is(err, ErrPaymentUnavailable) {
		t.Fatalf("quiesced request admitted: %v", err)
	}
	unblock()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := svc.Quiesce(context.Background()); err != nil || svc.ActiveRequests() != 0 {
		t.Fatalf("drain failed: %v", err)
	}
	svc.Resume()
	if err := svc.beginPaymentRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc.endPaymentRequest()
}

func TestReconcileBackoffBounds(t *testing.T) {
	svc := &Service{}
	for _, attempt := range []int{0, 1, 2, 10} {
		before := time.Now()
		order := storage.PaymentOrder{Status: "PENDING", ReconcileAttempts: attempt}
		update := svc.failureUpdate(order, nil, "PROVIDER_UNAVAILABLE")
		base := 30 * time.Second
		for i := 0; i < attempt && base < 15*time.Minute; i++ {
			base *= 2
		}
		if base > 15*time.Minute {
			base = 15 * time.Minute
		}
		if update.NextReconcileAt.Before(before.Add(base)) || update.NextReconcileAt.After(time.Now().Add(base+base/5)) || update.ReconcileAttempts != attempt+1 {
			t.Fatalf("attempt%d schedule=%+v", attempt, update)
		}
	}
}

func TestUnknownCallbackDoesNotCreateProviderOrder(t *testing.T) {
	store := createTestStore(t)
	data := webhookFixture()
	_, err := store.SaveVerifiedPaymentCallback(context.Background(), storage.VerifiedPaymentCallback{Data: data, OrderCode: 100000000001, PaymentLinkID: "fixture-link", Reference: "REF-1", Reason: "UNKNOWN_ORDER"})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	if err = createTestService(t, store, server).reconcileDue(context.Background()); err != nil || calls.Load() != 0 {
		t.Fatalf("unknown callback called provider: %v calls=%d", err, calls.Load())
	}
	var orders int
	_ = store.DB().QueryRow(`SELECT count(*) FROM payment_orders`).Scan(&orders)
	if orders != 0 {
		t.Fatalf("unknown callback created %d orders", orders)
	}
	reviews, err := store.ListPaymentReviews(context.Background(), "", 10)
	if err != nil || len(reviews.Items) != 1 || reviews.Items[0].Reason != "UNKNOWN_ORDER" {
		t.Fatalf("unknown callback disappeared: %+v err=%v", reviews, err)
	}
}

func TestDefinitiveCreateRejectionNeedsGet404BeforeFailed(t *testing.T) {
	store := createTestStore(t)
	order := reserveReconcileOrder(t, store, "CREATING", false)
	_, err := store.DB().Exec(`UPDATE payment_orders SET last_error_code='CREATE_REJECTED_HTTP_400' WHERE id=?`, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	var absent atomic.Bool
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" {
			t.Error("rejected order retried Create")
		}
		if absent.Load() {
			w.WriteHeader(http.StatusNotFound)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "UNKNOWN", "desc": "uncertain"})
	}))
	defer server.Close()
	svc := createTestService(t, store, server)
	if err = svc.reconcileDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := loadReconcileOrder(t, store, order.ID)
	if got.Status != "CREATING" || got.LastErrorCode != "CREATE_REJECTED_HTTP_400" {
		t.Fatalf("rejection proof lost or guessed absence: %+v", got)
	}
	absent.Store(true)
	if err = svc.reconcileOrder(context.Background(), order.ID); err != nil {
		t.Fatal(err)
	}
	got = loadReconcileOrder(t, store, order.ID)
	if got.Status != "FAILED" || calls.Load() != 2 {
		t.Fatalf("absence proof not applied: %+v calls=%d", got, calls.Load())
	}
	assertNoPaymentReceipt(t, store)
}

func TestReconcileProviderRequestsAreSpacedIncludingRecovery(t *testing.T) {
	store := createTestStore(t)
	order := reserveReconcileOrder(t, store, "CREATING", false)
	var mu sync.Mutex
	var times []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		if r.Method == "GET" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "404", "desc": "absent"})
			return
		}
		req := decodeCreateRequest(t, r)
		writeSignedFixture(t, w, signedCreateData(req))
	}))
	defer server.Close()
	if err := createTestService(t, store, server).reconcileDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) != 2 || times[1].Sub(times[0]) < 950*time.Millisecond {
		t.Fatalf("recovery exceeded request rate: %v", times)
	}
	got := loadReconcileOrder(t, store, order.ID)
	if got.Status != "PENDING" {
		t.Fatalf("recovery=%+v", got)
	}
}

func TestReconcileBatchCapsAtTwentyProviderOrders(t *testing.T) {
	store := createTestStore(t)
	orders := make(map[int64]storage.PaymentOrder, 21)
	for i := range 21 {
		digits := strconv.Itoa(i)
		key := "3e628a46-7db9-4f7b-a95f-" + strings.Repeat("0", 12-len(digits)) + digits
		order, _, err := store.ReservePaymentOrder(context.Background(), storage.PaymentOrderIntent{ChannelID: "fixture-client", IdempotencyKey: key, RequestHash: paymentRequestHash(50000, "STATIC_URL"), AmountVnd: 50000, Origin: "STATIC_URL"})
		if err != nil {
			t.Fatal(err)
		}
		order, err = store.CompletePaymentOrderOperation(context.Background(), order.ID, order.OperationToken, storage.PaymentOrderUpdate{Status: "PENDING", PaymentLinkID: "fixture-link-" + digits, QRCode: "original-provider-qr", AccountNumber: "VA-ORDER-1", NextReconcileAt: time.Now().Add(-time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		orders[order.OrderCode] = order
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		code, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/v2/payment-requests/"), 10, 64)
		order, ok := orders[code]
		if err != nil || !ok || r.Method != "GET" {
			t.Errorf("unexpected batch request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data := reconcileLinkFixture(order, "PENDING")
		data["id"] = order.PaymentLinkID
		writeSignedFixture(t, w, data)
	}))
	defer server.Close()
	if err := createTestService(t, store, server).reconcileDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 20 {
		t.Fatalf("batch requested %d orders", calls.Load())
	}
	due, err := store.DuePaymentOrders(context.Background(), "fixture-client", time.Now(), 20)
	if err != nil || len(due) != 1 {
		t.Fatalf("unprocessed batch remainder=%d err=%v", len(due), err)
	}
}
