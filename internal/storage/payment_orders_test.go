package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func paymentOrderTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "payments.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func paymentOrderTestIntent(key string) PaymentOrderIntent {
	return PaymentOrderIntent{ChannelID: "test-channel", IdempotencyKey: key, RequestHash: "hash-50000-static", AmountVnd: 50000, Origin: "STATIC_URL"}
}

func reservePaymentTestOrder(t *testing.T, s *Store, key string) PaymentOrder {
	t.Helper()
	o, created, err := s.ReservePaymentOrder(context.Background(), paymentOrderTestIntent(key))
	if err != nil || !created {
		t.Fatalf("reserve created=%v err=%v", created, err)
	}
	return o
}

func TestPaymentOrderRenewalPersistsRecoveryAndExplicitAttemptReset(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
	o := reservePaymentTestOrder(t, s, "renew")
	before := time.Now().UTC()
	renewed, err := s.RenewPaymentOrderOperation(ctx, o.ID, o.OperationToken, true)
	if err != nil || renewed.OperationToken != o.OperationToken || !renewed.QRRecoveryAttempted {
		t.Fatalf("renewed=%+v err=%v", renewed, err)
	}
	lease, err := time.Parse(time.RFC3339Nano, renewed.OperationLeaseUntil)
	if err != nil || lease.Before(before.Add(PaymentOperationLease)) {
		t.Fatalf("renewed lease=%v err=%v", lease, err)
	}
	next, err := time.Parse(time.RFC3339Nano, renewed.NextReconcileAt)
	if err != nil || next.Before(lease) {
		t.Fatalf("reconcile schedule=%v lease=%v err=%v", next, lease, err)
	}
	renewed, err = s.RenewPaymentOrderOperation(ctx, o.ID, o.OperationToken, false)
	if err != nil || !renewed.QRRecoveryAttempted {
		t.Fatalf("recovery intent lost: %+v err=%v", renewed, err)
	}
	if _, err := s.RenewPaymentOrderOperation(ctx, o.ID, "obsolete-token", true); !errors.Is(err, ErrPaymentOperationLost) {
		t.Fatalf("stale renewal accepted: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE payment_orders SET reconcile_attempts=6 WHERE id=?`, o.ID); err != nil {
		t.Fatal(err)
	}
	completed, err := s.CompletePaymentOrderOperation(ctx, o.ID, o.OperationToken, PaymentOrderUpdate{
		Status: "PENDING", ResetReconcileAttempts: true, NextReconcileAt: time.Now().Add(time.Minute),
	})
	if err != nil || completed.ReconcileAttempts != 0 || !completed.QRRecoveryAttempted || completed.OperationToken != "" {
		t.Fatalf("explicit reset=%+v err=%v", completed, err)
	}
	if _, err := s.RenewPaymentOrderOperation(ctx, o.ID, o.OperationToken, true); !errors.Is(err, ErrPaymentOperationLost) {
		t.Fatalf("completed token renewed: %v", err)
	}
	claimed, ok, err := s.ClaimPaymentOrder(ctx, o.ID, time.Now())
	if err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	gate, err := s.AcquireMutationGate(ctx, "renew-test", time.Minute, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewPaymentOrderOperation(ctx, o.ID, claimed.OperationToken, true); !errors.Is(err, ErrMutationGateLocked) {
		t.Fatalf("renewal bypassed gate: %v", err)
	}
	unchanged, err := s.PaymentOrder(ctx, o.ID)
	if err != nil || !reflect.DeepEqual(unchanged, claimed) {
		t.Fatalf("blocked renewal changed snapshot: %+v err=%v", unchanged, err)
	}
	if err := s.ReleaseMutationGate(ctx, "renew-test", gate.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE payment_orders SET status='PAID' WHERE id=?`, o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewPaymentOrderOperation(ctx, o.ID, claimed.OperationToken, true); !errors.Is(err, ErrPaymentOperationLost) {
		t.Fatalf("PAID operation renewed: %v", err)
	}
}

func TestPaymentOrderConcurrentIdempotencyAcrossStores(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	type result struct {
		order   PaymentOrder
		created bool
		err     error
	}
	results := make(chan result, 16)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range cap(results) {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			s := first
			if i%2 == 1 {
				s = second
			}
			o, created, err := s.ReservePaymentOrder(ctx, paymentOrderTestIntent("same-intent"))
			results <- result{o, created, err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	var canonical PaymentOrder
	newCount := 0
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if canonical.ID == "" {
			canonical = r.order
		}
		if r.order.ID != canonical.ID || r.order.OrderCode != canonical.OrderCode || r.order.OperationToken != canonical.OperationToken {
			t.Fatalf("replay changed durable identity: %+v vs %+v", r.order, canonical)
		}
		if r.created {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("new intents=%d", newCount)
	}
	var count int
	if err := first.DB().QueryRowContext(ctx, `SELECT count(*) FROM payment_orders`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("orders=%d err=%v", count, err)
	}
	lease, err := time.Parse(time.RFC3339Nano, canonical.OperationLeaseUntil)
	if err != nil {
		t.Fatal(err)
	}
	claims := make(chan result, 16)
	for i := range cap(claims) {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := first
			if i%2 == 1 {
				s = second
			}
			o, claimed, err := s.ClaimPaymentOrder(ctx, canonical.ID, lease.Add(time.Second))
			claims <- result{o, claimed, err}
		}(i)
	}
	wg.Wait()
	close(claims)
	claimCount := 0
	for r := range claims {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.created {
			claimCount++
		}
	}
	if claimCount != 1 {
		t.Fatalf("concurrent lease holders=%d", claimCount)
	}
	for _, change := range []func(*PaymentOrderIntent){
		func(in *PaymentOrderIntent) { in.AmountVnd++ },
		func(in *PaymentOrderIntent) { in.Origin = "OPERATOR_DYNAMIC" },
		func(in *PaymentOrderIntent) { in.RequestHash = "other-hash" },
	} {
		in := paymentOrderTestIntent("same-intent")
		change(&in)
		if _, _, err := first.ReservePaymentOrder(ctx, in); !errors.Is(err, ErrPaymentIdempotencyConflict) {
			t.Fatalf("conflicting replay error=%v", err)
		}
	}
	in := paymentOrderTestIntent("same-intent")
	in.ChannelID = "separate-channel"
	other, created, err := first.ReservePaymentOrder(ctx, in)
	if err != nil || !created || other.ID == canonical.ID || other.OrderCode == canonical.OrderCode {
		t.Fatalf("independent channel order=%+v created=%v err=%v", other, created, err)
	}
}

func TestPaymentOrderSnapshotCapabilityAndPersistence(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
	o := reservePaymentTestOrder(t, s, "persist")
	decoded, err := base64.RawURLEncoding.DecodeString(o.ID)
	if err != nil || len(decoded) != 32 || strings.Contains(o.ID, "=") {
		t.Fatalf("capability must be unpadded random256: len=%d err=%v", len(decoded), err)
	}
	if o.OrderCode < 100000000000 || o.OrderCode > 999999999999 || o.Description != fmt.Sprintf("DH%d", o.OrderCode) {
		t.Fatalf("order code/description: %d %q", o.OrderCode, o.Description)
	}
	created, err := time.Parse(time.RFC3339Nano, o.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	expiry, err := time.Parse(time.RFC3339Nano, o.ExpiresAt)
	if err != nil || expiry.Sub(created) != 30*time.Minute {
		t.Fatalf("expiry=%v err=%v", expiry, err)
	}
	lease, err := time.Parse(time.RFC3339Nano, o.OperationLeaseUntil)
	if err != nil || lease.Sub(created) != PaymentOperationLease || o.NextReconcileAt != o.OperationLeaseUntil {
		t.Fatalf("lease=%v order=%+v err=%v", lease, o, err)
	}
	for _, get := range []func() (PaymentOrder, error){
		func() (PaymentOrder, error) { return s.PaymentOrder(ctx, o.ID) },
		func() (PaymentOrder, error) { return s.PaymentOrderByCode(ctx, o.OrderCode) },
		func() (PaymentOrder, error) { return s.PaymentOrderByKey(ctx, o.ChannelID, o.IdempotencyKey) },
	} {
		got, err := get()
		if err != nil || !reflect.DeepEqual(got, o) {
			t.Fatalf("snapshot=%+v err=%v", got, err)
		}
	}
	if _, err := s.PaymentOrder(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing snapshot=%v", err)
	}
	body, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["orderCode"] != fmt.Sprintf("%d", o.OrderCode) {
		t.Fatalf("orderCode must serialize decimal string: %s", body)
	}
	for _, hidden := range []string{"channelId", "idempotencyKey", "requestHash", "paymentLinkId", "operationToken", "operationLeaseUntil", "description"} {
		if _, ok := fields[hidden]; ok {
			t.Fatalf("internal field exposed: %s", hidden)
		}
	}
}

func TestPaymentOrderLeaseTimestampFractionBoundaries(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
	o := reservePaymentTestOrder(t, s, "precision")
	whole := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	fraction := whole.Add(time.Nanosecond)
	if _, err := s.DB().ExecContext(ctx, `UPDATE payment_orders SET operation_lease_until=?,next_reconcile_at=? WHERE id=?`, fraction.Format(time.RFC3339Nano), fraction.Format(time.RFC3339Nano), o.ID); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimPaymentOrder(ctx, o.ID, whole); err != nil || claimed {
		t.Fatalf("nanosecond-future lease prematurely claimed=%v err=%v", claimed, err)
	}
	if due, err := s.DuePaymentOrders(ctx, o.ChannelID, whole, 20); err != nil || len(due) != 0 {
		t.Fatalf("nanosecond-future schedule due=%+v err=%v", due, err)
	}
	if _, claimed, err := s.ClaimPaymentOrder(ctx, o.ID, fraction); err != nil || !claimed {
		t.Fatalf("exact deadline claim=%v err=%v", claimed, err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE payment_orders SET operation_lease_until=?,next_reconcile_at=? WHERE id=?`, whole.Format(time.RFC3339Nano), whole.Format(time.RFC3339Nano), o.ID); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.ClaimPaymentOrder(ctx, o.ID, fraction); err != nil || !claimed {
		t.Fatalf("whole-second expired lease blocked=%v err=%v", claimed, err)
	}
}

func TestPaymentOrderLeaseExclusionCASAndPaidPriority(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
	o := reservePaymentTestOrder(t, s, "lease")
	lease, err := time.Parse(time.RFC3339Nano, o.OperationLeaseUntil)
	if err != nil {
		t.Fatal(err)
	}
	blocked, claimed, err := s.ClaimPaymentOrder(ctx, o.ID, lease.Add(-time.Second))
	if err != nil || claimed || blocked.OperationToken != o.OperationToken {
		t.Fatalf("live lease claimed=%v order=%+v err=%v", claimed, blocked, err)
	}
	// An inbox wake cannot bypass the in-flight Create lease.
	if _, err := s.DB().ExecContext(ctx, `UPDATE payment_orders SET next_reconcile_at=? WHERE id=?`, lease.Add(-time.Minute).Format(time.RFC3339Nano), o.ID); err != nil {
		t.Fatal(err)
	}
	due, err := s.DuePaymentOrders(ctx, o.ChannelID, lease.Add(-time.Second), 20)
	if err != nil || len(due) != 0 {
		t.Fatalf("live create appeared due=%+v err=%v", due, err)
	}
	claimedOrder, claimed, err := s.ClaimPaymentOrder(ctx, o.ID, lease.Add(time.Second))
	if err != nil || !claimed || claimedOrder.OperationToken == o.OperationToken || claimedOrder.NextReconcileAt != claimedOrder.OperationLeaseUntil {
		t.Fatalf("expired lease claimed=%v order=%+v err=%v", claimed, claimedOrder, err)
	}
	if _, err := s.CompletePaymentOrderOperation(ctx, o.ID, o.OperationToken, PaymentOrderUpdate{Status: "PENDING", QRCode: "stale-qr"}); !errors.Is(err, ErrPaymentOperationLost) {
		t.Fatalf("stale response accepted: %v", err)
	}
	updated, err := s.CompletePaymentOrderOperation(ctx, o.ID, claimedOrder.OperationToken, PaymentOrderUpdate{
		Status: "CREATING", LastErrorCode: "CREATE_OUTCOME_UNKNOWN", NextReconcileAt: lease.Add(-time.Minute), ReconcileAttempts: 2, QRRecoveryAttempted: true,
	})
	if err != nil || updated.OperationToken != "" || updated.OperationLeaseUntil != "" || updated.NextReconcileAt != claimedOrder.OperationLeaseUntil || updated.ReconcileAttempts != 2 || !updated.QRRecoveryAttempted {
		t.Fatalf("deferred snapshot=%+v err=%v", updated, err)
	}
	nextLease, err := time.Parse(time.RFC3339Nano, updated.NextReconcileAt)
	if err != nil {
		t.Fatal(err)
	}
	due, err = s.DuePaymentOrders(ctx, o.ChannelID, nextLease.Add(time.Second), 20)
	if err != nil || len(due) != 1 || due[0].ID != o.ID {
		t.Fatalf("durable retry missing due=%+v err=%v", due, err)
	}
	claimedOrder, claimed, err = s.ClaimPaymentOrder(ctx, o.ID, nextLease.Add(time.Second))
	if err != nil || !claimed {
		t.Fatalf("reclaim=%v err=%v", claimed, err)
	}
	// Simulate the future settlement transaction: it does not need to wait
	// for the provider operation and does not destroy its metadata CAS token.
	paidAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.DB().ExecContext(ctx, `UPDATE payment_orders SET status='PAID',paid_at=?,last_error_code=NULL,next_reconcile_at=NULL WHERE id=?`, paidAt, o.ID); err != nil {
		t.Fatal(err)
	}
	paid, err := s.CompletePaymentOrderOperation(ctx, o.ID, claimedOrder.OperationToken, PaymentOrderUpdate{
		Status: "CANCELLED", PaymentLinkID: "verified-link", QRCode: "verified-qr", AccountNumber: "order-va", LastErrorCode: "late-error", ReconcileAttempts: 1,
	})
	if err != nil || paid.Status != "PAID" || paid.PaidAt != paidAt || paid.QRCode != "verified-qr" || paid.PaymentLinkID != "verified-link" || paid.NextReconcileAt != "" || paid.LastErrorCode != "" || paid.OperationToken != "" || !paid.QRRecoveryAttempted || paid.ReconcileAttempts != 2 {
		t.Fatalf("late cancel downgraded settlement: %+v err=%v", paid, err)
	}
	if _, claimed, err := s.ClaimPaymentOrder(ctx, o.ID, nextLease.Add(time.Hour)); err != nil || claimed {
		t.Fatalf("PAID claimed=%v err=%v", claimed, err)
	}
	if _, err := s.CompletePaymentOrderOperation(ctx, o.ID, "", PaymentOrderUpdate{}); !errors.Is(err, ErrPaymentOperationLost) {
		t.Fatalf("empty token accepted: %v", err)
	}
}

func TestPaymentOrderDeploymentGateAndCompletionRollback(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
	o := reservePaymentTestOrder(t, s, "existing")
	gate, err := s.AcquireMutationGate(ctx, "payment-test", time.Minute, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReservePaymentOrder(ctx, paymentOrderTestIntent("new")); !errors.Is(err, ErrMutationGateLocked) {
		t.Fatalf("new order bypassed gate: %v", err)
	}
	replay, created, err := s.ReservePaymentOrder(ctx, paymentOrderTestIntent("existing"))
	if err != nil || created || replay.ID != o.ID {
		t.Fatalf("read replay blocked: %+v created=%v err=%v", replay, created, err)
	}
	if _, err := s.CompletePaymentOrderOperation(ctx, o.ID, o.OperationToken, PaymentOrderUpdate{Status: "PENDING", QRCode: "new-qr"}); !errors.Is(err, ErrMutationGateLocked) {
		t.Fatalf("complete bypassed gate: %v", err)
	}
	still, err := s.PaymentOrder(ctx, o.ID)
	if err != nil || !reflect.DeepEqual(still, o) {
		t.Fatalf("failed completion did not rollback: %+v err=%v", still, err)
	}
	lease, err := time.Parse(time.RFC3339Nano, o.OperationLeaseUntil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimPaymentOrder(ctx, o.ID, lease.Add(time.Second)); !errors.Is(err, ErrMutationGateLocked) {
		t.Fatalf("claim bypassed gate: %v", err)
	}
	if err := s.ReleaseMutationGate(ctx, "payment-test", gate.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompletePaymentOrderOperation(ctx, o.ID, o.OperationToken, PaymentOrderUpdate{Status: "PENDING", QRCode: "new-qr", NextReconcileAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
}

func TestPaymentOrderPaginationAndFilters(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
	orders := make([]PaymentOrder, 5)
	for i := range orders {
		orders[i] = reservePaymentTestOrder(t, s, fmt.Sprintf("page-%d", i))
	}
	if _, err := s.CompletePaymentOrderOperation(ctx, orders[0].ID, orders[0].OperationToken, PaymentOrderUpdate{Status: "PENDING"}); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	cursor := ""
	for {
		page, err := s.ListPaymentOrders(ctx, PaymentOrderFilter{ChannelID: "test-channel", Cursor: cursor, Limit: 2})
		if err != nil || len(page.Items) > 2 {
			t.Fatalf("page=%+v err=%v", page, err)
		}
		for _, o := range page.Items {
			if seen[o.ID] {
				t.Fatalf("duplicate paginated order %s", o.ID)
			}
			seen[o.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != len(orders) {
		t.Fatalf("paginated count=%d", len(seen))
	}
	page, err := s.ListPaymentOrders(ctx, PaymentOrderFilter{Status: "PENDING"})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != orders[0].ID {
		t.Fatalf("filtered page=%+v err=%v", page, err)
	}
	page, err = s.ListPaymentOrders(ctx, PaymentOrderFilter{ChannelID: "different-channel"})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("channel filter page=%+v err=%v", page, err)
	}
	if _, err := s.ListPaymentOrders(ctx, PaymentOrderFilter{Cursor: "bad-cursor"}); err == nil {
		t.Fatal("invalid cursor accepted")
	}
	if _, err := s.ListPaymentOrders(ctx, PaymentOrderFilter{Status: "FAKE"}); !errors.Is(err, ErrInvalidPaymentOrder) {
		t.Fatalf("invalid status=%v", err)
	}
}

func TestPayOSMigrationPreservesV13HistoryAndOutbox(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v13.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,checksum TEXT NOT NULL,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version >= 14 {
			break
		}
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("migration %d: %v", m.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES(?,?,?)`, m.version, m.checksum, "2026-10-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	fixture := []string{
		`INSERT INTO connections(id,bank_code,state,generation,created_at,updated_at) VALUES('legacy','ACB','MONITORING',7,'2026-09-10T00:00:00Z','2026-09-10T00:00:00Z')`,
		`INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,debit,credit,balance,parser_version,baseline_state,first_seen_at,transaction_at_iso,transaction_day,date_precision,ingest_source) VALUES('credit','legacy','ACB:credit','hash-credit','2026-09-10','2026-09-10',0,123456,999999,'v1','NONE','2026-09-10T00:00:00Z','2026-09-10T00:00:00Z','2026-09-10','datetime','REALTIME')`,
		`INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,debit,credit,balance,parser_version,baseline_state,first_seen_at,transaction_at_iso,transaction_day,date_precision,ingest_source) VALUES('debit','legacy','ACB:debit','hash-debit','2026-09-11','2026-09-11',5000,0,994999,'v1','NONE','2026-09-11T00:00:00Z','2026-09-11T00:00:00Z','2026-09-11','datetime','REALTIME')`,
		`INSERT INTO events(id,transaction_id,event_type,payload,payload_hash,created_at) VALUES('event-credit','credit','bank.transaction.credit',X'7B7D','event-hash','2026-09-10T00:00:00Z')`,
		`INSERT INTO deliveries(id,event_id,endpoint_id,endpoint_revision,key_id,status,attempts,next_attempt_at,created_at,updated_at) VALUES('pending-delivery','event-credit','old-endpoint',1,'old-secret','PENDING',2,'2026-10-10T00:00:00Z','2026-09-10T00:00:00Z','2026-09-10T00:00:00Z')`,
		`INSERT INTO event_journal(seq,epoch,event_type,aggregate_id,payload_json,created_at) VALUES(42,'ep1','bank.transaction.credit','credit','{"bank":"ACB"}','2026-09-10T00:00:00Z')`,
	}
	for _, statement := range fixture {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	// Snapshot complete rows, not just totals, so IDs, hashes, delivery state
	// and historical payloads must remain byte-for-byte equivalent.
	queries := []string{
		`SELECT * FROM transactions ORDER BY id`,
		`SELECT * FROM events ORDER BY id`,
		`SELECT * FROM deliveries ORDER BY id`,
		`SELECT * FROM event_journal ORDER BY seq`,
		`SELECT * FROM schema_migrations WHERE version<=13 ORDER BY version`,
		`SELECT * FROM connections WHERE id='legacy'`,
	}
	before := make([][][]any, len(queries))
	for i, query := range queries {
		before[i] = paymentTestSQLSnapshot(t, db, query)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, query := range queries {
		after := paymentTestSQLSnapshot(t, s.DB(), query)
		if !reflect.DeepEqual(before[i], after) {
			t.Fatalf("migration changed historical query %s: before=%v after=%v", query, before[i], after)
		}
	}
	var bank, state string
	var generation int
	var credentials, sessions int
	if err := s.DB().QueryRowContext(ctx, `SELECT bank_code,state,generation FROM connections WHERE id='payos-klb'`).Scan(&bank, &state, &generation); err != nil || bank != "KienlongBank" || state != "WEBHOOK" || generation != 0 {
		t.Fatalf("provider connection bank=%s state=%s generation=%d err=%v", bank, state, generation, err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM connections WHERE id='payos-klb' AND (account_envelope IS NOT NULL OR account_identity_hmac IS NOT NULL)`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE connection_id='payos-klb'`).Scan(&sessions); err != nil || sessions != 0 || credentials != 0 {
		t.Fatalf("provider credentials=%d sessions=%d err=%v", credentials, sessions, err)
	}
	var checksum string
	if err := s.DB().QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version=15`).Scan(&checksum); err != nil || checksum != "2026-10-08-v15-payos-payment-orders" {
		t.Fatalf("migration15 checksum=%s err=%v", checksum, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migration replay: %v", err)
	}
}

func paymentTestSQLSnapshot(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := make([][]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		for i, value := range values {
			if b, ok := value.([]byte); ok {
				values[i] = append([]byte(nil), b...)
			}
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
