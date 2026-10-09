package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifiedPaymentInboxReplayAndPrivateEvidence(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	in := VerifiedPaymentCallback{OrderCode: 100000000001, PaymentLinkID: "link", Reference: "ref", Reason: "AWAITING_ORDER_BIND", Data: map[string]any{"orderCode": int64(100000000001), "counterAccountNumber": "private-sender-account", "virtualAccountNumber": "private-va", "amount": 50000}}
	first, err := store.SaveVerifiedPaymentCallback(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		got, err := store.SaveVerifiedPaymentCallback(ctx, in)
		if err != nil || got != first {
			t.Fatalf("duplicate callback: %q %v", got, err)
		}
	}
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM payos_webhook_inbox`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("replay grew inbox: %d", count)
	}
	reviews, err := store.ListPaymentReviews(ctx, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(reviews)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-sender-account", "private-va", "payload_json", "counterAccountNumber"} {
		if strings.Contains(string(body), private) {
			t.Fatalf("review leaks %s", private)
		}
	}
	pending, err := store.PendingPaymentCallbacks(ctx, in.OrderCode)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !strings.Contains(pending[0].PayloadJSON, "private-va") {
		t.Fatalf("binding consumer lost provider evidence: %+v", pending)
	}
	in.Reason = "PAYMENT_MISMATCH"
	in.Data["amount"] = 30000
	if _, err := store.SaveVerifiedPaymentCallback(ctx, in); err != nil {
		t.Fatal(err)
	}
	pending, err = store.PendingPaymentCallbacks(ctx, in.OrderCode)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("review conflict automatically retried: %+v", pending)
	}
}
