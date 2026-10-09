package payments

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPaymentStatusUsesOnlyVerifiedProviderActivity(t *testing.T) {
	store := createTestStore(t)
	order := reserveReconcileOrder(t, store, "PENDING", true)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" {
			t.Errorf("unexpected request %s", r.Method)
		}
		writeSignedFixture(t, w, reconcileLinkFixture(order, "PENDING"))
	}))
	defer server.Close()
	svc := createTestService(t, store, server)
	before, err := svc.Status(context.Background())
	if err != nil || before.LastReconciledAt != nil || before.LastWebhookAt != nil || calls.Load() != 0 {
		t.Fatalf("status invented provider evidence: %+v %v", before, err)
	}
	if err := svc.reconcileDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := svc.Status(context.Background())
	if err != nil || after.LastReconciledAt == nil || after.LastWebhookAt != nil || calls.Load() != 1 {
		t.Fatalf("verified reconciliation unreported: %+v %v", after, err)
	}
	for range 3 {
		status, err := svc.Status(context.Background())
		if err != nil || status.LastReconciledAt == nil || *status.LastReconciledAt != *after.LastReconciledAt || calls.Load() != 1 {
			t.Fatal("readonly status changed provider activity")
		}
	}
}
