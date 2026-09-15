package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestClaimDeliveryUsesLeaseAndClaimToken(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	nowAt := time.Now().UTC().Truncate(time.Second)
	_, err = store.DB().ExecContext(ctx, `INSERT INTO webhook_endpoints(id,name,status,current_revision,created_at,updated_at) VALUES('endpoint','test','ACTIVE',1,?,?)`, now(), now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().ExecContext(ctx, `INSERT INTO events(id,event_type,payload,payload_hash,created_at) VALUES('event','bank.transaction.credit',X'7B7D','hash',?)`, now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().ExecContext(ctx, `INSERT INTO deliveries(id,event_id,endpoint_id,endpoint_revision,key_id,status,next_attempt_at,created_at,updated_at) VALUES('delivery','event','endpoint',1,'k1','PENDING',?,?,?)`, nowAt.Format(time.RFC3339Nano), now(), now())
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := store.ClaimDelivery(ctx, nowAt, time.Minute)
	if err != nil || delivery.ClaimToken == "" || delivery.Status != "IN_FLIGHT" || delivery.Attempts != 1 {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}
	if _, err := store.ClaimDelivery(ctx, nowAt, time.Minute); err == nil {
		t.Fatal("delivery was claimed twice")
	}
	if err := store.CompleteDelivery(ctx, delivery, false, nowAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteDelivery(ctx, delivery, true, nowAt); err == nil {
		t.Fatal("old claim completed twice")
	}
}

func TestRecordAttemptStoresAndRetrieves(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "attempts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	nowAt := time.Now().UTC().Truncate(time.Second)
	_, err = store.DB().ExecContext(ctx, `INSERT INTO webhook_endpoints(id,name,status,current_revision,created_at,updated_at) VALUES('endpoint_att','test','ACTIVE',1,?,?)`, now(), now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().ExecContext(ctx, `INSERT INTO events(id,event_type,payload,payload_hash,created_at) VALUES('event_att','bank.transaction.credit',X'7B7D','hash',?)`, now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB().ExecContext(ctx, `INSERT INTO deliveries(id,event_id,endpoint_id,endpoint_revision,key_id,status,next_attempt_at,created_at,updated_at) VALUES('deliv_att','event_att','endpoint_att',1,'k1','PENDING',?,?,?)`, nowAt.Format(time.RFC3339Nano), now(), now())
	if err != nil {
		t.Fatal(err)
	}

	err = store.RecordAttempt(ctx, "deliv_att", 1, 502, 125, "RETRY", "bad gateway", "WEBHOOK", "HTTP_502")
	if err != nil {
		t.Fatalf("RecordAttempt failed: %v", err)
	}

	var id, deliveryID, outcome, sanitizedError, provider, providerErrorCode, createdAt string
	var attemptNum, statusCode, latencyMs int
	err = store.DB().QueryRowContext(ctx, `
		SELECT id, delivery_id, attempt_number, status_code, latency_ms, outcome, sanitized_error, provider, COALESCE(provider_error_code, ''), created_at
		FROM delivery_attempts
		WHERE delivery_id = ? AND attempt_number = ?
	`, "deliv_att", 1).Scan(&id, &deliveryID, &attemptNum, &statusCode, &latencyMs, &outcome, &sanitizedError, &provider, &providerErrorCode, &createdAt)
	if err != nil {
		t.Fatalf("query delivery_attempts failed: %v", err)
	}

	if deliveryID != "deliv_att" || attemptNum != 1 || statusCode != 502 || latencyMs != 125 || outcome != "RETRY" || sanitizedError != "bad gateway" || provider != "WEBHOOK" || providerErrorCode != "HTTP_502" {
		t.Fatalf("unexpected record: deliveryID=%s attemptNum=%d statusCode=%d latencyMs=%d outcome=%s sanitizedError=%s provider=%s providerErrorCode=%s",
			deliveryID, attemptNum, statusCode, latencyMs, outcome, sanitizedError, provider, providerErrorCode)
	}
}

func TestNextDeliveryDueUsesActivePendingAndExpiredClaims(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "next-due.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	nowAt := time.Now().UTC().Truncate(time.Second)
	for _, statement := range []string{
		`INSERT INTO webhook_endpoints(id,name,status,current_revision,created_at,updated_at) VALUES('active','active','ACTIVE',1,'now','now'),('disabled','disabled','DISABLED',1,'now','now')`,
		`INSERT INTO events(id,event_type,payload,payload_hash,created_at) VALUES('event-a','test',X'7B7D','a','now'),('event-b','test',X'7B7D','b','now')`,
	} {
		if _, err := store.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	future := nowAt.Add(time.Hour).Format(time.RFC3339Nano)
	expired := nowAt.Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO deliveries(id,event_id,endpoint_id,endpoint_revision,key_id,status,next_attempt_at,lease_until,created_at,updated_at) VALUES
		('disabled-pending','event-a','disabled',1,'k1','PENDING',? ,NULL,'now','now'),
		('expired-claim','event-b','active',1,'k1','IN_FLIGHT',?,?,'now','now')`, future, future, expired); err != nil {
		t.Fatal(err)
	}
	due, err := store.NextDeliveryDue(ctx, nowAt)
	if err != nil {
		t.Fatal(err)
	}
	if !due.Equal(nowAt.Add(-time.Minute)) {
		t.Fatalf("expected expired claim deadline, got %s", due)
	}
}
