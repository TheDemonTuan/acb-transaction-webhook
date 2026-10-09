package webhook

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestDispatcherHappyPath(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Retained ACB history must remain dispatchable after the payOS runtime cutover.
	const connectionID = "historical-acb"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO connections(id,bank_code,state,account_masked,generation,created_at,updated_at) VALUES(?,'ACB','PAUSED','***1234',1,'2026-09-10T00:00:00Z','2026-09-10T00:00:00Z')`, connectionID); err != nil {
		t.Fatalf("seed historical connection: %v", err)
	}

	var receivedBody []byte
	var receivedSig, receivedTS, receivedNonce string
	var serverSecret string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		receivedSig = r.Header.Get("X-Bank-Signature")
		receivedTS = r.Header.Get("X-Bank-Timestamp")
		receivedNonce = r.Header.Get("X-Bank-Nonce")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	ep, err := store.CreateEndpointWithSecret(ctx, "Test Hook", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetEndpointStatus(ctx, ep.ID, "ACTIVE")
	serverSecret = ep.Secret

	txn, err := store.IngestTransaction(ctx, storage.TransactionInput{
		ConnectionID:  connectionID,
		SemanticKey:   "ACB:100",
		CanonicalHash: "hash100",
		TransactionAt: "2026-09-10",
		EffectiveAt:   "2026-09-10",
		Credit:        50000,
		ParserVersion: "v1",
	})
	if err != nil {
		t.Fatalf("seed historical transaction: %v", err)
	}

	_, err = store.EmitTransactionEvent(ctx, txn.TransactionID, "bank.transaction.credit", "acb", "ACB:100", map[string]any{
		"credit": 50000,
	})
	if err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcher(store, ts.Client()).SetSkipURLValidation(true)
	processed, err := dispatcher.DispatchOne(ctx)
	if err != nil || !processed {
		t.Fatalf("dispatch failed: %v, processed=%v", err, processed)
	}

	// Verify HMAC on received body
	if err := Verify([]byte(serverSecret), receivedBody, receivedTS, receivedNonce, receivedSig, time.Now().UTC(), time.Minute); err != nil {
		t.Fatalf("HMAC verification failed: %v", err)
	}

	// Delivery summary should have 0 pending
	summary, err := store.DeliverySummary(ctx)
	if err != nil || summary.Pending != 0 {
		t.Fatalf("summary unexpected: %+v", summary)
	}

	var attemptCount int
	var outcome string
	var statusCode int
	err = store.DB().QueryRowContext(ctx, `SELECT count(*), max(outcome), max(status_code) FROM delivery_attempts`).Scan(&attemptCount, &outcome, &statusCode)
	if err != nil || attemptCount != 1 || outcome != "SUCCESS" || statusCode != 200 {
		t.Fatalf("unexpected attempt record: count=%d outcome=%s status=%d err=%v", attemptCount, outcome, statusCode, err)
	}
}

func TestDispatcherRetryAndDeadLetter(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const connectionID = "historical-acb"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO connections(id,bank_code,state,account_masked,generation,created_at,updated_at) VALUES(?,'ACB','PAUSED','***1234',1,'2026-09-10T00:00:00Z','2026-09-10T00:00:00Z')`, connectionID); err != nil {
		t.Fatalf("seed historical connection: %v", err)
	}

	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	ep, err := store.CreateEndpointWithSecret(ctx, "Failing Hook", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetEndpointStatus(ctx, ep.ID, "ACTIVE")

	txn, err := store.IngestTransaction(ctx, storage.TransactionInput{
		ConnectionID:  connectionID,
		SemanticKey:   "ACB:101",
		CanonicalHash: "hash101",
		TransactionAt: "2026-09-10",
		EffectiveAt:   "2026-09-10",
		Credit:        50000,
		ParserVersion: "v1",
	})
	if err != nil {
		t.Fatalf("seed historical transaction: %v", err)
	}

	if _, err := store.EmitTransactionEvent(ctx, txn.TransactionID, "bank.transaction.credit", "acb", "ACB:101", map[string]any{"credit": 50000}); err != nil {
		t.Fatal(err)
	}

	dispatcher := NewDispatcher(store, ts.Client()).SetSkipURLValidation(true)
	dispatcher.maxRetries = 2

	// First attempt -> fail, reschedule
	processed, err := dispatcher.DispatchOne(ctx)
	if err != nil || !processed {
		t.Fatalf("first dispatch failed: %v", err)
	}

	// Manually update next_attempt_at to past so it can be claimed again immediately
	_, _ = store.DB().ExecContext(ctx, `UPDATE deliveries SET next_attempt_at = datetime('now', '-1 minute')`)

	// Second attempt -> fail, exceeds maxRetries=2 -> DEAD_LETTER
	processed, err = dispatcher.DispatchOne(ctx)
	if err != nil || !processed {
		t.Fatalf("second dispatch failed: %v", err)
	}

	summary, err := store.DeliverySummary(ctx)
	if err != nil || summary.DeadLetter != 1 || summary.Pending != 0 {
		t.Fatalf("expected 1 dead letter, got: %+v", summary)
	}

	rows, err := store.DB().QueryContext(ctx, `SELECT attempt_number, status_code, outcome FROM delivery_attempts ORDER BY attempt_number ASC`)
	if err != nil {
		t.Fatalf("query attempts failed: %v", err)
	}
	defer rows.Close()
	type attRec struct {
		num     int
		code    int
		outcome string
	}
	var recorded []attRec
	for rows.Next() {
		var a attRec
		if err := rows.Scan(&a.num, &a.code, &a.outcome); err != nil {
			t.Fatal(err)
		}
		recorded = append(recorded, a)
	}
	if len(recorded) != 2 || recorded[0].outcome != "RETRY" || recorded[1].outcome != "TERMINAL_FAILURE" {
		t.Fatalf("unexpected recorded attempts: %+v", recorded)
	}
}

func TestDispatcherEventDrivenWakeImmediate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway_wake.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const connectionID = "historical-acb"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO connections(id,bank_code,state,account_masked,generation,created_at,updated_at) VALUES(?,'ACB','PAUSED','***1234',1,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`, connectionID); err != nil {
		t.Fatalf("seed historical connection: %v", err)
	}

	deliveredCh := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		select {
		case deliveredCh <- struct{}{}:
		default:
		}
	}))
	defer ts.Close()

	ep, err := store.CreateEndpointWithSecret(ctx, "Wake Hook", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetEndpointStatus(ctx, ep.ID, "ACTIVE")

	dispatcher := NewDispatcher(store, ts.Client()).SetSkipURLValidation(true)
	go dispatcher.Run(ctx)

	// Create transaction and event/delivery
	txn, err := store.IngestTransaction(ctx, storage.TransactionInput{
		ConnectionID:  connectionID,
		SemanticKey:   "ACB:9999",
		CanonicalHash: "hash9999",
		TransactionAt: "2026-09-12",
		EffectiveAt:   "2026-09-12",
		Credit:        100000,
		ParserVersion: "v1",
	})
	if err != nil {
		t.Fatalf("seed historical transaction: %v", err)
	}
	_, err = store.EmitTransactionEvent(ctx, txn.TransactionID, "bank.transaction.credit", "acb", "ACB:9999", map[string]any{"credit": 100000})
	if err != nil {
		t.Fatal(err)
	}

	// Trigger immediate wake
	start := time.Now()
	dispatcher.Wake()

	select {
	case <-deliveredCh:
		elapsed := time.Since(start)
		if elapsed > 1*time.Second {
			t.Fatalf("wake took too long: %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for immediate delivery after Wake()")
	}
}
