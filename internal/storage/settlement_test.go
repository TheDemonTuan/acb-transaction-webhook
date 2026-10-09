package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func settlementTestOrder(t *testing.T, s *Store, key string, amount int64, bound bool) (PaymentOrder, SettlementInput) {
	t.Helper()
	intent := paymentOrderTestIntent(key)
	intent.AmountVnd = amount
	o, created, err := s.ReservePaymentOrder(context.Background(), intent)
	if err != nil || !created {
		t.Fatalf("reserve created=%v err=%v", created, err)
	}
	in := SettlementInput{ChannelID: o.ChannelID, OrderCode: o.OrderCode, PaymentLinkID: "link-" + key, Reference: "reference-" + key, AmountVnd: amount, TransactionAt: time.Date(2026, 10, 8, 18, 25, 0, 0, time.FixedZone("HCM", 7*3600)), AccountNumber: "main-account", VirtualAccountNumber: "va-" + key, Description: "provider-description", Source: "REALTIME"}
	if bound {
		o, err = s.CompletePaymentOrderOperation(context.Background(), o.ID, o.OperationToken, PaymentOrderUpdate{Status: "PENDING", PaymentLinkID: in.PaymentLinkID, AccountNumber: in.VirtualAccountNumber, QRCode: "verified-qr", NextReconcileAt: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
	}
	return o, in
}

func settlementTestEvidence(in SettlementInput) *SettlementEvidence {
	return &SettlementEvidence{OrderCode: in.OrderCode, PaymentLinkID: in.PaymentLinkID, Reference: in.Reference, AmountVnd: in.AmountVnd, TransactionAt: in.TransactionAt, AccountNumber: in.AccountNumber, VirtualAccountNumber: in.VirtualAccountNumber}
}

func settlementTestCallback(in SettlementInput, reason string) *VerifiedPaymentCallback {
	data := map[string]any{"orderCode": in.OrderCode, "paymentLinkId": in.PaymentLinkID, "reference": in.Reference, "amount": in.AmountVnd, "currency": "VND", "code": "00", "accountNumber": in.AccountNumber, "virtualAccountNumber": in.VirtualAccountNumber, "transactionDateTime": in.TransactionAt.In(time.FixedZone("HCM", 7*3600)).Format("2006-01-02 15:04:05")}
	return &VerifiedPaymentCallback{Data: data, OrderCode: in.OrderCode, PaymentLinkID: in.PaymentLinkID, Reference: in.Reference, Reason: reason}
}

func settlementTestEndpoint(t *testing.T, s *Store) {
	t.Helper()
	ep, err := s.CreateEndpointWithSecret(context.Background(), "settlement", "https://example.com/webhook")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEndpointStatus(context.Background(), ep.ID, "ACTIVE"); err != nil {
		t.Fatal(err)
	}
}

func settlementTestCounts(t *testing.T, s *Store, expected int) {
	t.Helper()
	for _, table := range []string{"transactions", "payment_receipts", "events", "deliveries", "event_journal"} {
		var n int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != expected {
			t.Fatalf("%s count=%d want=%d", table, n, expected)
		}
	}
}

func TestSettlePaymentSameAmountOutOfOrderAndNormalizedEvent(t *testing.T) {
	s := paymentOrderTestStore(t)
	settlementTestEndpoint(t, s)
	amounts := []int64{50000, 50000, 120000}
	orders := make([]PaymentOrder, 3)
	inputs := make([]SettlementInput, 3)
	for i, amount := range amounts {
		orders[i], inputs[i] = settlementTestOrder(t, s, strconv.Itoa(i), amount, true)
	}
	for _, i := range []int{1, 0, 2} {
		in := inputs[i]
		in.Description = "must not expose capability " + orders[i].ID
		if i == 2 {
			in.Source = "CATCH_UP"
		}
		result, err := s.SettlePayment(context.Background(), in)
		if err != nil || result.Event == nil || result.Duplicate || result.Order.ID != orders[i].ID || result.Order.Status != "PAID" || result.Order.TransactionID != result.Event.TransactionID || result.Event.JournalSeq <= 0 || result.Event.CommittedAt == "" {
			t.Fatalf("settle=%+v err=%v", result, err)
		}
		var event map[string]any
		if err := json.Unmarshal(result.Event.Payload, &event); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"bank": "KienlongBank", "provider": "PAYOS", "orderCode": strconv.FormatInt(in.OrderCode, 10), "paymentOrigin": "STATIC_URL", "paymentLinkId": in.PaymentLinkID, "credit": strconv.FormatInt(in.AmountVnd, 10), "debit": "0", "currency": "VND", "source": in.Source, "transactionNumber": in.Reference, "transactionDay": "2026-10-08", "description": orders[i].Description}
		for k, v := range want {
			if event[k] != v {
				t.Fatalf("event[%s]=%v want=%v", k, event[k], v)
			}
		}
		for _, secret := range []string{orders[i].ID, in.ChannelID, in.AccountNumber, in.VirtualAccountNumber} {
			if strings.Contains(string(result.Event.Payload), secret) {
				t.Fatalf("event leaked %q", secret)
			}
		}
		var connection, semantic, parser, baseline, source string
		var debit, credit int64
		var balance any
		if err := s.DB().QueryRow(`SELECT connection_id,semantic_key,parser_version,baseline_state,ingest_source,debit,credit,balance FROM transactions WHERE id=?`, result.Order.TransactionID).Scan(&connection, &semantic, &parser, &baseline, &source, &debit, &credit, &balance); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256([]byte(in.ChannelID))
		if connection != "payos-klb" || semantic != "PAYOS:"+hex.EncodeToString(h[:16])+":"+in.Reference || parser != "payos-v1" || baseline != "NONE" || source != in.Source || debit != 0 || credit != in.AmountVnd || balance != nil {
			t.Fatalf("invalid normalized transaction: %s %s %s %s %s %d %d %v", connection, semantic, parser, baseline, source, debit, credit, balance)
		}
	}
	settlementTestCounts(t, s, 3)
	var total int64
	if err := s.DB().QueryRow(`SELECT SUM(credit) FROM transactions`).Scan(&total); err != nil || total != 220000 {
		t.Fatalf("total=%d err=%v", total, err)
	}
}

func TestSettlePaymentReplayAndCrossStoreRace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "race.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	settlementTestEndpoint(t, first)
	_, in := settlementTestOrder(t, first, "race", 50000, true)
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan SettlementResult, 20)
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			store := first
			candidate := in
			if i%2 == 1 {
				store = second
				candidate.Source = "CATCH_UP"
				candidate.VerifiedGet = settlementTestEvidence(in)
			}
			r, err := store.SettlePayment(ctx, candidate)
			if err != nil {
				errs <- err
			} else {
				results <- r
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatal(err)
	}
	newEvents, duplicates := 0, 0
	for r := range results {
		if r.Event != nil {
			newEvents++
		}
		if r.Duplicate {
			duplicates++
		}
	}
	if newEvents != 1 || duplicates != 19 {
		t.Fatalf("events=%d duplicates=%d", newEvents, duplicates)
	}
	settlementTestCounts(t, first, 1)
}

func TestSettlePaymentReferenceConflictsAndExtraPaymentRetained(t *testing.T) {
	s := paymentOrderTestStore(t)
	settlementTestEndpoint(t, s)
	_, in := settlementTestOrder(t, s, "first", 50000, true)
	if _, err := s.SettlePayment(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*SettlementInput){"amount": func(p *SettlementInput) { p.AmountVnd++ }, "link": func(p *SettlementInput) { p.PaymentLinkID = "changed" }, "time": func(p *SettlementInput) { p.TransactionAt = p.TransactionAt.Add(time.Second) }} {
		t.Run(name, func(t *testing.T) {
			candidate := in
			change(&candidate)
			candidate.VerifiedCallback = settlementTestCallback(candidate, "placeholder")
			for range 2 {
				r, err := s.SettlePayment(context.Background(), candidate)
				if err != nil || r.ReviewReason != "REFERENCE_CONFLICT" || r.Event != nil || r.InboxHash == "" {
					t.Fatalf("conflict=%+v err=%v", r, err)
				}
			}
		})
	}
	_, second := settlementTestOrder(t, s, "second", 50000, true)
	second.Reference = in.Reference
	second.VerifiedCallback = settlementTestCallback(second, "placeholder")
	if r, err := s.SettlePayment(context.Background(), second); err != nil || r.ReviewReason != "REFERENCE_CONFLICT" {
		t.Fatalf("cross order=%+v err=%v", r, err)
	}
	extra := in
	extra.Reference = "second-real-reference"
	extra.VerifiedCallback = settlementTestCallback(extra, "placeholder")
	if r, err := s.SettlePayment(context.Background(), extra); err != nil || r.ReviewReason != "EXTRA_PAYMENT_REVIEW" {
		t.Fatalf("extra=%+v err=%v", r, err)
	}
	settlementTestCounts(t, s, 1)
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM payos_webhook_inbox`).Scan(&count); err != nil || count != 5 {
		t.Fatalf("reviews=%d err=%v", count, err)
	}
	var total int64
	if err := s.DB().QueryRow(`SELECT SUM(credit) FROM transactions`).Scan(&total); err != nil || total != 50000 {
		t.Fatalf("money changed=%d err=%v", total, err)
	}
}

func TestSettlePaymentRejectsUnverifiedBindingsDatesAndAccounts(t *testing.T) {
	cases := []struct {
		name, reason string
		bound        bool
		change       func(*SettlementInput)
	}{
		{"unknown", "UNKNOWN_ORDER", true, func(p *SettlementInput) { p.OrderCode++ }},
		{"channel", "PAYMENT_MISMATCH", true, func(p *SettlementInput) { p.ChannelID = "other" }},
		{"amount", "PAYMENT_MISMATCH", true, func(p *SettlementInput) { p.AmountVnd++ }},
		{"link", "PAYMENT_MISMATCH", true, func(p *SettlementInput) { p.PaymentLinkID = "other" }},
		{"effective-va", "PAYMENT_MISMATCH", true, func(p *SettlementInput) {
			p.AccountNumber = p.VirtualAccountNumber
			p.VirtualAccountNumber = "wrong-va"
		}},
		{"account-missing", "PAYMENT_DETAILS_PENDING", true, func(p *SettlementInput) { p.AccountNumber = ""; p.VirtualAccountNumber = "" }},
		{"date-missing", "INVALID_TRANSACTION_DATE", true, func(p *SettlementInput) { p.TransactionAt = time.Time{} }},
		{"reference-missing", "PAYMENT_MISMATCH", true, func(p *SettlementInput) { p.Reference = "" }},
		{"unbound", "AWAITING_ORDER_BIND", false, func(p *SettlementInput) {}},
		{"get-account-mismatch", "PAYMENT_MISMATCH", false, func(p *SettlementInput) {
			p.VerifiedGet = settlementTestEvidence(*p)
			p.VerifiedGet.VirtualAccountNumber = "wrong"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := paymentOrderTestStore(t)
			settlementTestEndpoint(t, s)
			_, in := settlementTestOrder(t, s, tc.name, 50000, tc.bound)
			tc.change(&in)
			in.VerifiedCallback = settlementTestCallback(in, "placeholder")
			r, err := s.SettlePayment(context.Background(), in)
			if err != nil || r.ReviewReason != tc.reason || r.Event != nil || r.InboxHash == "" {
				t.Fatalf("reject=%+v err=%v", r, err)
			}
			settlementTestCounts(t, s, 0)
			in.VerifiedCallback = nil
			_, err = s.SettlePayment(context.Background(), in)
			var review *SettlementReviewError
			if !errors.As(err, &review) || review.Reason != tc.reason {
				t.Fatalf("missing typed reason: %v", err)
			}
		})
	}
}

func TestSettlePaymentInboxBindRollbackAndLeasePriority(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
	settlementTestEndpoint(t, s)
	o, in := settlementTestOrder(t, s, "bind", 50000, false)
	in.VerifiedCallback = settlementTestCallback(in, "placeholder")
	review, err := s.SettlePayment(ctx, in)
	if err != nil || review.ReviewReason != "AWAITING_ORDER_BIND" {
		t.Fatalf("await=%+v err=%v", review, err)
	}
	queued, err := s.PaymentOrder(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	due, _ := time.Parse(time.RFC3339Nano, queued.NextReconcileAt)
	lease, _ := time.Parse(time.RFC3339Nano, o.OperationLeaseUntil)
	if due.Before(lease) {
		t.Fatalf("inbox woke before active lease: %s < %s", due, lease)
	}
	in.InboxHash = review.InboxHash
	in.VerifiedCallback = nil
	in.VerifiedGet = settlementTestEvidence(in)
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_settlement_journal BEFORE INSERT ON event_journal BEGIN SELECT RAISE(ABORT,'injected financial fault'); END`); err != nil {
		t.Fatal(err)
	}
	if r, err := s.SettlePayment(ctx, in); err == nil || r.Event != nil {
		t.Fatalf("fault acknowledged: %+v %v", r, err)
	}
	settlementTestCounts(t, s, 0)
	still, err := s.PaymentOrder(ctx, o.ID)
	if err != nil || !reflect.DeepEqual(still, queued) {
		t.Fatalf("rollback changed order: %+v err=%v", still, err)
	}
	var processed any
	if err := s.DB().QueryRow(`SELECT processed_at FROM payos_webhook_inbox WHERE payload_hash=?`, in.InboxHash).Scan(&processed); err != nil || processed != nil {
		t.Fatalf("failed inbox consumed=%v err=%v", processed, err)
	}
	if _, err := s.DB().Exec(`DROP TRIGGER fail_settlement_journal`); err != nil {
		t.Fatal(err)
	}
	r, err := s.SettlePayment(ctx, in)
	if err != nil || r.Order.Status != "PAID" || r.Order.AccountNumber != in.VirtualAccountNumber || r.Order.PaymentLinkID != in.PaymentLinkID || r.Order.OperationToken != o.OperationToken || r.Event == nil {
		t.Fatalf("bind=%+v err=%v", r, err)
	}
	if err := s.DB().QueryRow(`SELECT processed_at FROM payos_webhook_inbox WHERE payload_hash=?`, in.InboxHash).Scan(&processed); err != nil || processed == nil {
		t.Fatalf("inbox not consumed=%v err=%v", processed, err)
	}
	late, err := s.CompletePaymentOrderOperation(ctx, o.ID, o.OperationToken, PaymentOrderUpdate{Status: "CANCELLED", QRCode: "late-real-qr", PaymentLinkID: in.PaymentLinkID, AccountNumber: in.VirtualAccountNumber})
	if err != nil || late.Status != "PAID" || late.QRCode != "late-real-qr" || late.TransactionID != r.Order.TransactionID {
		t.Fatalf("late response won=%+v err=%v", late, err)
	}
	dup, err := s.SettlePayment(ctx, in)
	if err != nil || !dup.Duplicate || dup.Event != nil {
		t.Fatalf("inbox replay=%+v err=%v", dup, err)
	}
	settlementTestCounts(t, s, 1)
}

func TestSettlePaymentInboxEvidenceMustMatchAndTerminalNeedsGet(t *testing.T) {
	for _, status := range []string{"CANCELLED", "EXPIRED", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			s := paymentOrderTestStore(t)
			settlementTestEndpoint(t, s)
			o, in := settlementTestOrder(t, s, status, 50000, true)
			if _, err := s.DB().Exec(`UPDATE payment_orders SET status=? WHERE id=?`, status, o.ID); err != nil {
				t.Fatal(err)
			}
			in.VerifiedCallback = settlementTestCallback(in, "placeholder")
			r, err := s.SettlePayment(context.Background(), in)
			if err != nil || r.ReviewReason != "PAYMENT_DETAILS_PENDING" {
				t.Fatalf("terminal bypass=%+v err=%v", r, err)
			}
			in.VerifiedGet = settlementTestEvidence(in)
			in.VerifiedCallback = nil
			if r, err := s.SettlePayment(context.Background(), in); err != nil || r.Order.Status != "PAID" {
				t.Fatalf("late verified paid=%+v err=%v", r, err)
			}
			settlementTestCounts(t, s, 1)
		})
	}
	s := paymentOrderTestStore(t)
	settlementTestEndpoint(t, s)
	_, in := settlementTestOrder(t, s, "bad-inbox", 50000, false)
	callback := settlementTestCallback(in, "AWAITING_ORDER_BIND")
	callback.Data["amount"] = int64(49999)
	hash, err := s.SaveVerifiedPaymentCallback(context.Background(), *callback)
	if err != nil {
		t.Fatal(err)
	}
	in.InboxHash = hash
	in.VerifiedGet = settlementTestEvidence(in)
	r, err := s.SettlePayment(context.Background(), in)
	if err != nil || r.ReviewReason != "PAYMENT_MISMATCH" {
		t.Fatalf("mismatched inbox=%+v err=%v", r, err)
	}
	settlementTestCounts(t, s, 0)
	var reason string
	var processed any
	if err := s.DB().QueryRow(`SELECT reason,processed_at FROM payos_webhook_inbox WHERE payload_hash=?`, hash).Scan(&reason, &processed); err != nil || reason != "PAYMENT_MISMATCH" || processed != nil {
		t.Fatalf("review lost=%s %v err=%v", reason, processed, err)
	}
}

func TestSettlePaymentGateBlocksEveryFinancialOrReviewWrite(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
	settlementTestEndpoint(t, s)
	_, in := settlementTestOrder(t, s, "gate", 50000, true)
	gate, err := s.AcquireMutationGate(ctx, "settlement-test", time.Minute, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, unknown := range []bool{false, true} {
		candidate := in
		if unknown {
			candidate.OrderCode++
		}
		candidate.VerifiedCallback = settlementTestCallback(candidate, "placeholder")
		if _, err := s.SettlePayment(ctx, candidate); !errors.Is(err, ErrMutationGateLocked) {
			t.Fatalf("gate bypass=%v", err)
		}
	}
	settlementTestCounts(t, s, 0)
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM payos_webhook_inbox`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("gate inbox count=%d err=%v", n, err)
	}
	if err := s.ReleaseMutationGate(ctx, "settlement-test", gate.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettlePayment(ctx, in); err != nil {
		t.Fatal(err)
	}
	settlementTestCounts(t, s, 1)
}

func TestSettlePaymentPreservesHistoricalRowsAndLegacyEventDedupe(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
	settlementTestEndpoint(t, s)
	conn := historicalConnectionFixture(t, s, ctx, "***1234")
	for i, credit := range []int64{123456, 0} {
		txn, err := s.IngestTransaction(ctx, TransactionInput{ConnectionID: conn.ID, SemanticKey: fmt.Sprintf("ACB:%d", i), CanonicalHash: fmt.Sprintf("legacy-hash-%d", i), TransactionAt: "2026-09-10", EffectiveAt: "2026-09-10", Debit: int64(i) * 5000, Credit: credit, ParserVersion: "v1"})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			for range 2 {
				if _, err := s.EmitTransactionEvent(ctx, txn.TransactionID, "bank.transaction.credit", "acb", "ACB:0", map[string]any{"bank": "ACB", "credit": "123456"}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	queries := []string{`SELECT * FROM connections WHERE bank_code='ACB'`, `SELECT * FROM transactions WHERE connection_id<>'payos-klb'`, `SELECT * FROM events WHERE transaction_id IN (SELECT id FROM transactions WHERE connection_id<>'payos-klb')`, `SELECT * FROM deliveries WHERE event_id IN (SELECT id FROM events WHERE transaction_id IN (SELECT id FROM transactions WHERE connection_id<>'payos-klb'))`, `SELECT * FROM event_journal WHERE aggregate_id IN (SELECT id FROM transactions WHERE connection_id<>'payos-klb')`}
	before := make([][][]any, len(queries))
	for i, q := range queries {
		before[i] = paymentTestSQLSnapshot(t, s.DB(), q)
	}
	if len(before[4]) != 1 || len(before[3]) != 1 {
		t.Fatalf("legacy event duplicate produced journal/outbox %v %v", before[4], before[3])
	}
	_, in := settlementTestOrder(t, s, "conserve", 50000, true)
	if _, err := s.SettlePayment(ctx, in); err != nil {
		t.Fatal(err)
	}
	for i, q := range queries {
		if after := paymentTestSQLSnapshot(t, s.DB(), q); !reflect.DeepEqual(before[i], after) {
			t.Fatalf("history changed: %s before=%v after=%v", q, before[i], after)
		}
	}
	var credit, debit int64
	if err := s.DB().QueryRow(`SELECT SUM(credit),SUM(debit) FROM transactions`).Scan(&credit, &debit); err != nil || credit != 173456 || debit != 5000 {
		t.Fatalf("conservation credit=%d debit=%d err=%v", credit, debit, err)
	}
}

func TestPaymentInboxWakePromotionAndReplayPreservesBackoff(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
	o, in := settlementTestOrder(t, s, "wake", 50000, false)
	callback := settlementTestCallback(in, "UNKNOWN_ORDER")
	hash, err := s.SaveVerifiedPaymentCallback(ctx, *callback)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WakePendingPaymentCallbacks(ctx, "other-channel"); err != nil {
		t.Fatal(err)
	}
	items, err := s.PendingPaymentCallbacks(ctx, in.OrderCode)
	if err != nil || len(items) != 0 {
		t.Fatalf("cross-channel promotion=%+v err=%v", items, err)
	}
	if err := s.WakePendingPaymentCallbacks(ctx, in.ChannelID); err != nil {
		t.Fatal(err)
	}
	items, err = s.PendingPaymentCallbacks(ctx, in.OrderCode)
	if err != nil || len(items) != 1 || items[0].PayloadHash != hash || items[0].Reason != "AWAITING_ORDER_BIND" {
		t.Fatalf("local promotion=%+v err=%v", items, err)
	}
	backoff := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := s.DB().Exec(`UPDATE payment_orders SET next_reconcile_at=? WHERE id=?`, backoff, o.ID); err != nil {
		t.Fatal(err)
	}
	callback.Reason = "AWAITING_ORDER_BIND"
	if replay, err := s.SaveVerifiedPaymentCallback(ctx, *callback); err != nil || replay != hash {
		t.Fatalf("replay=%s err=%v", replay, err)
	}
	if err := s.WakePendingPaymentCallbacks(ctx, in.ChannelID); err != nil {
		t.Fatal(err)
	}
	order, err := s.PaymentOrder(ctx, o.ID)
	if err != nil || order.NextReconcileAt != backoff {
		t.Fatalf("replay erased backoff=%+v err=%v", order, err)
	}
	callback.Reason = "PAYMENT_MISMATCH"
	if _, err := s.SaveVerifiedPaymentCallback(ctx, *callback); err != nil {
		t.Fatal(err)
	}
	items, err = s.PendingPaymentCallbacks(ctx, in.OrderCode)
	if err != nil || len(items) != 0 {
		t.Fatalf("conflict still being consumed=%+v err=%v", items, err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM payos_webhook_inbox`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("replay bloated inbox=%d err=%v", n, err)
	}
}

func TestPaymentDetailsPendingEvidenceIsConsumedAtomically(t *testing.T) {
	ctx := context.Background()
	s := paymentOrderTestStore(t)
	settlementTestEndpoint(t, s)
	o, in := settlementTestOrder(t, s, "details", 50000, false)
	hash, err := s.SaveVerifiedPaymentCallback(ctx, *settlementTestCallback(in, "PAYMENT_DETAILS_PENDING"))
	if err != nil {
		t.Fatal(err)
	}
	order, err := s.PaymentOrder(ctx, o.ID)
	if err != nil || order.LastErrorCode != "PAYMENT_DETAILS_PENDING" {
		t.Fatalf("order missing diagnostic=%+v err=%v", order, err)
	}
	items, err := s.PendingPaymentCallbacks(ctx, in.OrderCode)
	if err != nil || len(items) != 1 {
		t.Fatalf("missing details consumer=%+v err=%v", items, err)
	}
	in.InboxHash = hash
	in.VerifiedGet = settlementTestEvidence(in)
	result, err := s.SettlePayment(ctx, in)
	if err != nil || result.Order.Status != "PAID" || result.Order.LastErrorCode != "" || result.Event == nil {
		t.Fatalf("settlement=%+v err=%v", result, err)
	}
	items, err = s.PendingPaymentCallbacks(ctx, in.OrderCode)
	if err != nil || len(items) != 0 {
		t.Fatalf("settled evidence unconsumed=%+v err=%v", items, err)
	}
	settlementTestCounts(t, s, 1)
}

func TestSettlePaymentInboxCannotBorrowDifferentTransactionEvidence(t *testing.T) {
	for _, field := range []string{"orderCode", "paymentLinkId", "reference", "accountNumber", "virtualAccountNumber", "transactionDateTime", "currency", "code"} {
		t.Run(field, func(t *testing.T) {
			s := paymentOrderTestStore(t)
			settlementTestEndpoint(t, s)
			_, in := settlementTestOrder(t, s, field, 50000, false)
			callback := settlementTestCallback(in, "AWAITING_ORDER_BIND")
			switch field {
			case "orderCode":
				callback.Data[field] = in.OrderCode + 1
			case "accountNumber":
				// accountNumber is irrelevant while a nonempty VA is present;
				// remove VA so the effective receiving account is tested.
				callback.Data["virtualAccountNumber"] = ""
				callback.Data[field] = "wrong-receiving-account"
			default:
				callback.Data[field] = "wrong-evidence"
			}
			hash, err := s.SaveVerifiedPaymentCallback(context.Background(), *callback)
			if err != nil {
				t.Fatal(err)
			}
			in.InboxHash = hash
			in.VerifiedGet = settlementTestEvidence(in)
			result, err := s.SettlePayment(context.Background(), in)
			if err != nil || result.ReviewReason != "PAYMENT_MISMATCH" || result.Event != nil {
				t.Fatalf("borrowed evidence=%+v err=%v", result, err)
			}
			settlementTestCounts(t, s, 0)
		})
	}
}

func TestSettlePaymentUsesVirtualAccountOrAccountFallbackOnly(t *testing.T) {
	for _, useVA := range []bool{false, true} {
		t.Run(strconv.FormatBool(useVA), func(t *testing.T) {
			s := paymentOrderTestStore(t)
			settlementTestEndpoint(t, s)
			o, in := settlementTestOrder(t, s, "receiving-account", 50000, false)
			if !useVA {
				in.AccountNumber = in.VirtualAccountNumber
				in.VirtualAccountNumber = ""
			}
			in.VerifiedGet = settlementTestEvidence(in)
			result, err := s.SettlePayment(context.Background(), in)
			if err != nil || result.Order.Status != "PAID" || result.Order.AccountNumber != "va-receiving-account" || result.Order.ID != o.ID {
				t.Fatalf("effective account bind=%+v err=%v", result, err)
			}
			settlementTestCounts(t, s, 1)
		})
	}
}

func TestSettlePaymentSuppliedNamespaceIsHashedNotRawChannel(t *testing.T) {
	s := paymentOrderTestStore(t)
	settlementTestEndpoint(t, s)
	_, in := settlementTestOrder(t, s, "namespace", 50000, true)
	h := sha256.Sum256([]byte(in.ChannelID))
	in.ChannelNamespace = hex.EncodeToString(h[:16])
	r, err := s.SettlePayment(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var semantic string
	if err := s.DB().QueryRow(`SELECT semantic_key FROM transactions WHERE id=?`, r.Order.TransactionID).Scan(&semantic); err != nil {
		t.Fatal(err)
	}
	if semantic != "PAYOS:"+in.ChannelNamespace+":"+in.Reference || strings.Contains(semantic, in.ChannelID) {
		t.Fatalf("invalid namespace: %s", semantic)
	}
}

func TestSettlePaymentReviewInsertFaultIsNotAcknowledged(t *testing.T) {
	s := paymentOrderTestStore(t)
	settlementTestEndpoint(t, s)
	_, in := settlementTestOrder(t, s, "review-fault", 50000, true)
	in.AmountVnd++
	in.VerifiedCallback = settlementTestCallback(in, "placeholder")
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_payment_review BEFORE INSERT ON payos_webhook_inbox BEGIN SELECT RAISE(ABORT,'injected review fault'); END`); err != nil {
		t.Fatal(err)
	}
	if r, err := s.SettlePayment(context.Background(), in); err == nil || r.ReviewReason != "" {
		t.Fatalf("review lost but acknowledged=%+v err=%v", r, err)
	}
	settlementTestCounts(t, s, 0)
	if _, err := s.DB().Exec(`DROP TRIGGER fail_payment_review`); err != nil {
		t.Fatal(err)
	}
	if r, err := s.SettlePayment(context.Background(), in); err != nil || r.ReviewReason != "PAYMENT_MISMATCH" || r.InboxHash == "" {
		t.Fatalf("review retry=%+v err=%v", r, err)
	}
}
