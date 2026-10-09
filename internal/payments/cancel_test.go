package payments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestCancelOrderIssuedDuringDisabledCreationAndPaidWins(t *testing.T) {
	for _, paid := range []bool{false, true} {
		name := "cancelled"
		if paid {
			name = "paid"
		}
		t.Run(name, func(t *testing.T) {
			store := createTestStore(t)
			order := reserveReconcileOrder(t, store, "PENDING", true)
			var gets, cancels atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/cancel") {
					cancels.Add(1)
					status := "CANCELLED"
					if paid {
						status = "PAID"
					}
					writeSignedFixture(t, w, reconcileLinkFixture(order, status))
					return
				}
				gets.Add(1)
				writeSignedFixture(t, w, reconcileLinkFixture(order, "PAID"))
			}))
			defer server.Close()
			svc := createTestService(t, store, server)
			svc.cfg.PaymentsEnabled = false
			svc.cfg.PayOSWebhookConfirmed = false
			got, err := svc.CancelOrder(context.Background(), order.ID)
			if paid {
				if !errors.Is(err, ErrPaymentAlreadyPaid) || got.Status != "PAID" || gets.Load() != 1 {
					t.Fatalf("Cancel paid without verified Get: %+v err=%v gets=%d", got, err, gets.Load())
				}
				if _, err = svc.CancelOrder(context.Background(), order.ID); !errors.Is(err, ErrPaymentAlreadyPaid) || cancels.Load() != 1 {
					t.Fatalf("paid cancellation called provider again: %v calls=%d", err, cancels.Load())
				}
			} else if err != nil || got.Status != "CANCELLED" || cancels.Load() != 1 {
				t.Fatalf("issued cancellation gated: %+v err=%v", got, err)
			}
		})
	}
}

func TestCancelOrderAmbiguityGetsProviderAndSchedulesRetry(t *testing.T) {
	store := createTestStore(t)
	order := reserveReconcileOrder(t, store, "PENDING", true)
	var cancels, gets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			cancels.Add(1)
			w.Header().Set("Retry-After", "180")
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "UNKNOWN", "desc": "uncertain"})
			return
		}
		gets.Add(1)
		writeSignedFixture(t, w, reconcileLinkFixture(order, "PENDING"))
	}))
	defer server.Close()
	svc := createTestService(t, store, server)
	before := time.Now()
	got, err := svc.CancelOrder(context.Background(), order.ID)
	if err != nil || got.Status != "PENDING" || got.LastErrorCode != "CANCEL_OUTCOME_UNKNOWN" || cancels.Load() != 1 || gets.Load() != 1 {
		t.Fatalf("ambiguous cancellation: %+v err=%v", got, err)
	}
	next, _ := time.Parse(time.RFC3339Nano, got.NextReconcileAt)
	if next.Before(before.Add(180 * time.Second)) {
		t.Fatalf("Retry-After ignored: %s", got.NextReconcileAt)
	}
	assertNoPaymentReceipt(t, store)
}

func TestCancelOrderLiveLeaseReturnsSnapshotWithoutProvider(t *testing.T) {
	store := createTestStore(t)
	order := reserveReconcileOrder(t, store, "PENDING", true)
	_, claimed, err := store.ClaimPaymentOrder(context.Background(), order.ID, time.Now())
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	got, err := createTestService(t, store, server).CancelOrder(context.Background(), order.ID)
	if err != nil || got.Status != "PENDING" || calls.Load() != 0 {
		t.Fatalf("competing cancellation: %+v %v calls=%d", got, err, calls.Load())
	}
}

func reconcileWebhookBody(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"code": "00", "success": true, "data": data, "signature": fixtureSignature(t, data)})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestWebhookGetRaceCommitsOneReceiptAndEvent(t *testing.T) {
	store := createTestStore(t)
	order := reserveReconcileOrder(t, store, "PENDING", true)
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		writeSignedFixture(t, w, reconcileLinkFixture(order, "PAID"))
	}))
	defer server.Close()
	svc := createTestService(t, store, server)
	var events atomic.Int64
	svc.onCommit = func(storage.EventNotification) { events.Add(1) }
	finished := make(chan error, 1)
	go func() { finished <- svc.reconcileDue(context.Background()) }()
	<-entered
	data := webhookFixture()
	data["orderCode"] = order.OrderCode
	if err := svc.HandleWebhook(context.Background(), reconcileWebhookBody(t, data)); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	got := loadReconcileOrder(t, store, order.ID)
	var receipts, transactions int
	_ = store.DB().QueryRow(`SELECT count(*) FROM payment_receipts`).Scan(&receipts)
	_ = store.DB().QueryRow(`SELECT count(*) FROM transactions WHERE connection_id='payos-klb'`).Scan(&transactions)
	if got.Status != "PAID" || receipts != 1 || transactions != 1 || events.Load() != 1 {
		t.Fatalf("race duplicated financial writes: %+v receipts=%d transactions=%d events=%d", got, receipts, transactions, events.Load())
	}
}

func TestTerminalWebhookRequiresMatchingVerifiedGet(t *testing.T) {
	for _, status := range []string{"CANCELLED", "EXPIRED"} {
		for _, matching := range []bool{false, true} {
			name := status + " mismatch"
			if matching {
				name = status + " match"
			}
			t.Run(name, func(t *testing.T) {
				store := createTestStore(t)
				order := reserveReconcileOrder(t, store, status, true)
				var gets atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gets.Add(1)
					data := reconcileLinkFixture(order, "PAID")
					if !matching {
						data["transactions"].([]any)[0].(map[string]any)["reference"] = "DIFFERENT"
					}
					writeSignedFixture(t, w, data)
				}))
				defer server.Close()
				svc := createTestService(t, store, server)
				data := webhookFixture()
				data["orderCode"] = order.OrderCode
				if err := svc.HandleWebhook(context.Background(), reconcileWebhookBody(t, data)); err != nil {
					t.Fatal(err)
				}
				got := loadReconcileOrder(t, store, order.ID)
				if gets.Load() != 1 {
					t.Fatalf("terminal callback omitted Get: %d", gets.Load())
				}
				if matching {
					if got.Status != "PAID" {
						t.Fatalf("late paid refused: %+v", got)
					}
				} else {
					if got.Status == "PAID" {
						t.Fatal("nonmatching Get became money")
					}
					assertNoPaymentReceipt(t, store)
					reviews, err := store.ListPaymentReviews(context.Background(), "", 10)
					if err != nil || len(reviews.Items) != 1 || reviews.Items[0].Reason != "PAYMENT_DETAILS_PENDING" {
						t.Fatalf("terminal conflict discarded: %+v err=%v", reviews, err)
					}
				}
			})
		}
	}
}
