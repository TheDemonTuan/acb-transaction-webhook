package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
)

func sepayTestStore(t *testing.T) *Store {
	t.Helper()
	s, _ := setupTestStoreWithKeyring(t)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sepayTestInput(updateID, messageID int64, reference string) SePayNotificationInput {
	at := time.Date(2026, 10, 10, 0, 5, 0, 0, time.FixedZone("VN", 7*3600))
	in := SePayNotificationInput{StoreKey: "test-store", BankCode: "VCB", AccountNumber: "TEST123", Mode: "active", BotID: 900001, UpdateID: updateID, ChatID: -100900003, MessageID: messageID, ActivationAt: at.Add(-time.Hour), MessageAt: time.Now().UTC(), Credit: &SePayCredit{AmountVND: 50000, Reference: reference, TransactionAt: at}}
	sepayTestPayload(&in, "private sender memo")
	return in
}

func sepayTestPayload(in *SePayNotificationInput, content string) {
	kind := "message"
	if in.ReviewReason == "EDITED_MESSAGE" {
		kind = "edited_message"
	}
	message := map[string]any{"message_id": in.MessageID, "date": in.MessageAt.Unix(), "chat": map[string]any{"id": in.ChatID}, "from": map[string]any{"id": 900002, "is_bot": true}, "text": content}
	in.RawPayload, _ = json.Marshal(map[string]any{"update_id": in.UpdateID, kind: message})
}

func sepayTestCount(t *testing.T, s *Store, table string, expected int) {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != expected {
		t.Fatalf("%s count=%d want=%d err=%v", table, n, expected, err)
	}
}

func sepayTestFinancialCounts(t *testing.T, s *Store, expected int) {
	t.Helper()
	for _, table := range []string{"transactions", "sepay_receipts", "events", "deliveries", "event_journal"} {
		sepayTestCount(t, s, table, expected)
	}
	for _, table := range []string{"payment_orders", "payment_receipts"} {
		sepayTestCount(t, s, table, 0)
	}
}

func TestSePayAtomicCreditEncryptedEvidenceAndSafeEvent(t *testing.T) {
	s := sepayTestStore(t)
	settlementTestEndpoint(t, s)
	in := sepayTestInput(9007199254740993, 10, "SEPAY_TEST_001")
	result, err := s.IngestSePayNotification(context.Background(), in)
	if err != nil || result.Event == nil || result.Duplicate || result.ReviewReason != "" || result.Event.CommittedAt == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	sepayTestFinancialCounts(t, s, 1)
	var data map[string]any
	if err := json.Unmarshal(result.Event.Payload, &data); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"provider": "SEPAY", "bank": "VCB", "credit": "50000", "debit": "0", "source": "REALTIME", "transactionDay": "2026-10-10", "description": "Thanh toán QR cửa hàng", "transactionNumber": "SEPAY_TEST_001", "transactionId": result.Event.TransactionID} {
		if data[key] != want {
			t.Fatalf("%s=%v want=%v", key, data[key], want)
		}
	}
	for _, key := range []string{"orderCode", "paymentOrigin", "accountNumber", "balance", "chatId", "senderId", "rawPayload"} {
		if _, ok := data[key]; ok {
			t.Fatalf("unsafe/unrelated event field: %s", key)
		}
	}
	var credit, debit int64
	var balance sql.NullInt64
	var semantic, parser, day, source, inboxTransaction string
	var description, envelope []byte
	if err := s.DB().QueryRow(`SELECT credit,debit,balance,semantic_key,parser_version,transaction_day,ingest_source,description_envelope FROM transactions`).Scan(&credit, &debit, &balance, &semantic, &parser, &day, &source, &description); err != nil {
		t.Fatal(err)
	}
	if credit != 50000 || debit != 0 || balance.Valid || semantic != "SEPAY:test-store:SEPAY_TEST_001" || parser != "sepay-telegram-v1" || day != "2026-10-10" || source != "REALTIME" || string(description) != "Thanh toán QR cửa hàng" {
		t.Fatalf("unsafe credit row: credit=%d debit=%d balance=%v semantic=%s parser=%s day=%s source=%s description=%s", credit, debit, balance, semantic, parser, day, source, description)
	}
	if err := s.DB().QueryRow(`SELECT payload_envelope,transaction_id FROM sepay_telegram_inbox WHERE update_id=?`, in.UpdateID).Scan(&envelope, &inboxTransaction); err != nil || inboxTransaction != result.Event.TransactionID {
		t.Fatalf("inbox not tied to transaction: %s err=%v", inboxTransaction, err)
	}
	if bytes.Contains(envelope, []byte("private sender memo")) {
		t.Fatal("inbox retained plaintext payer memo")
	}
	plain, err := s.sepayDecrypt(envelope, sepayInboxAAD(in.BotID, in.UpdateID))
	if err != nil || !bytes.Equal(plain, in.RawPayload) {
		t.Fatalf("evidence not decryptable with required AAD: %v", err)
	}
	if _, err := s.sepayDecrypt(envelope, sepayInboxAAD(in.BotID, in.UpdateID+1)); err == nil {
		t.Fatal("evidence decrypts under another update's AAD")
	}
}

func TestSePayRetriesMessageIdentityReferenceDedupeAndJournalRecovery(t *testing.T) {
	ctx := context.Background()
	s := sepayTestStore(t)
	settlementTestEndpoint(t, s)
	in := sepayTestInput(1, 10, "reference-1")
	first, err := s.IngestSePayNotification(ctx, in)
	if err != nil || first.Event == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, in.RawPayload, "", "  "); err != nil {
		t.Fatal(err)
	}
	in.RawPayload = pretty.Bytes()
	for range 10 {
		r, err := s.IngestSePayNotification(ctx, in)
		if err != nil || !r.Duplicate || r.Event != nil {
			t.Fatalf("retry=%+v err=%v", r, err)
		}
	}
	for _, ids := range [][2]int64{{2, 10}, {3, 11}} {
		in.UpdateID, in.MessageID = ids[0], ids[1]
		sepayTestPayload(&in, "private sender memo")
		r, err := s.IngestSePayNotification(ctx, in)
		if err != nil || !r.Duplicate || r.Event != nil {
			t.Fatalf("message/reference dedupe=%+v err=%v", r, err)
		}
	}
	sepayTestFinancialCounts(t, s, 1)
	sepayTestCount(t, s, "sepay_telegram_inbox", 3)
	// Do not consume the returned hint: recovery must be possible solely from
	// the durable journal, including after opening a new process's Store.
	var path string
	if err := s.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.WithKeyring(s.keyring)
	entries, err := reopened.ReadJournalEvents(ctx, "ep1", 0, 100)
	if err != nil || len(entries) != 1 || entries[0].AggregateID != first.Event.TransactionID || entries[0].Seq != first.Event.JournalSeq || !bytes.Equal(entries[0].Payload, first.Event.Payload) {
		t.Fatalf("journal recovery=%+v err=%v", entries, err)
	}
	r, err := reopened.IngestSePayNotification(ctx, in)
	if err != nil || !r.Duplicate || r.Event != nil {
		t.Fatalf("restart replay=%+v err=%v", r, err)
	}
	sepayTestFinancialCounts(t, reopened, 1)
}

func TestSePayConcurrentIndependentHandlesExactlyOneCredit(t *testing.T) {
	ctx := context.Background()
	first := sepayTestStore(t)
	settlementTestEndpoint(t, first)
	var path string
	if err := first.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.WithKeyring(first.keyring)
	base := sepayTestInput(1, 10, "race-reference")
	start := make(chan struct{})
	results := make(chan SePayIngestResult, 20)
	errs := make(chan error, 20)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			store := first
			if i%2 == 1 {
				store = second
			}
			in := base
			// Exercise both concurrent same-update and distinct-message retries.
			in.UpdateID, in.MessageID = int64(i/2+1), int64(i/2+10)
			sepayTestPayload(&in, "private sender memo")
			r, err := store.IngestSePayNotification(ctx, in)
			if err != nil {
				errs <- err
			} else {
				results <- r
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
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
	sepayTestFinancialCounts(t, first, 1)
	sepayTestCount(t, first, "sepay_telegram_inbox", 10)
}

func TestSePayConflictsEditsAndEncryptedCandidatesPreserveFirstMoney(t *testing.T) {
	for _, kind := range []string{"same-update", "same-message", "reference-amount", "reference-time", "edit"} {
		t.Run(kind, func(t *testing.T) {
			s := sepayTestStore(t)
			settlementTestEndpoint(t, s)
			in := sepayTestInput(1, 10, "first-reference")
			first, err := s.IngestSePayNotification(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			var originalHash, originalReason string
			var originalBlob []byte
			if err := s.DB().QueryRow(`SELECT payload_hash,reason,payload_envelope FROM sepay_telegram_inbox WHERE update_id=1`).Scan(&originalHash, &originalReason, &originalBlob); err != nil {
				t.Fatal(err)
			}
			candidate := in
			credit := *in.Credit
			candidate.Credit = &credit
			candidate.Credit.AmountVND = 120000
			reason := "REFERENCE_CONFLICT"
			switch kind {
			case "same-message":
				candidate.UpdateID = 2
			case "reference-amount":
				candidate.UpdateID, candidate.MessageID = 2, 11
			case "reference-time":
				candidate.UpdateID, candidate.MessageID = 2, 11
				candidate.Credit.AmountVND = 50000
				candidate.Credit.TransactionAt = candidate.Credit.TransactionAt.Add(time.Second)
			case "edit":
				candidate.UpdateID = 2
				candidate.Credit = nil
				candidate.ReviewReason, reason = "EDITED_MESSAGE", "EDITED_MESSAGE"
			}
			sepayTestPayload(&candidate, "edited private memo amount=120000")
			for range 2 {
				r, err := s.IngestSePayNotification(context.Background(), candidate)
				if err != nil || r.ReviewReason != reason || r.Event != nil {
					t.Fatalf("conflict=%+v want=%s err=%v", r, reason, err)
				}
			}
			sepayTestFinancialCounts(t, s, 1)
			sepayTestCount(t, s, "transaction_quarantine", 1)
			var currentHash, currentReason, transactionID string
			var currentBlob []byte
			if err := s.DB().QueryRow(`SELECT payload_hash,reason,payload_envelope,transaction_id FROM sepay_telegram_inbox WHERE update_id=1`).Scan(&currentHash, &currentReason, &currentBlob, &transactionID); err != nil {
				t.Fatal(err)
			}
			if currentHash != originalHash || currentReason != originalReason || !bytes.Equal(currentBlob, originalBlob) || transactionID != first.Event.TransactionID {
				t.Fatal("conflict replaced first observation")
			}
			if candidate.UpdateID != 1 {
				var reviewTransaction sql.NullString
				if err := s.DB().QueryRow(`SELECT transaction_id FROM sepay_telegram_inbox WHERE update_id=2`).Scan(&reviewTransaction); err != nil || reviewTransaction.Valid {
					t.Fatalf("review attached to financial row: %v err=%v", reviewTransaction, err)
				}
			}
			var quarantine []byte
			if err := s.DB().QueryRow(`SELECT candidate_envelope FROM transaction_quarantine`).Scan(&quarantine); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(quarantine, []byte("edited private memo")) {
				t.Fatal("quarantine leaks plaintext")
			}
			plain, err := s.sepayDecrypt(quarantine, sepayInboxAAD(candidate.BotID, candidate.UpdateID))
			if err != nil || !bytes.Equal(plain, candidate.RawPayload) {
				t.Fatalf("quarantine did not preserve candidate: %v", err)
			}
		})
	}
}

func TestSePayGateFailureAndAtomicJournalRollbackCanRetry(t *testing.T) {
	ctx := context.Background()
	s := sepayTestStore(t)
	settlementTestEndpoint(t, s)
	in := sepayTestInput(1, 10, "gate-reference")
	gate, err := s.AcquireMutationGate(ctx, "sepay-test", time.Minute, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, review := range []string{"", "INVALID_AMOUNT", "EDITED_MESSAGE"} {
		candidate := in
		candidate.ReviewReason = review
		if _, err := s.IngestSePayNotification(ctx, candidate); !errors.Is(err, ErrMutationGateLocked) {
			t.Fatalf("gate bypass: %v", err)
		}
	}
	sepayTestCount(t, s, "sepay_telegram_inbox", 0)
	if err := s.ReleaseMutationGate(ctx, "sepay-test", gate.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_sepay_journal BEFORE INSERT ON event_journal BEGIN SELECT RAISE(ABORT,'journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	r, err := s.IngestSePayNotification(ctx, in)
	if err == nil || r.Event != nil {
		t.Fatalf("commit failure exposed event: %+v err=%v", r, err)
	}
	sepayTestFinancialCounts(t, s, 0)
	sepayTestCount(t, s, "sepay_telegram_inbox", 0)
	var connections int
	if err := s.DB().QueryRow(`SELECT count(*) FROM connections WHERE id='sepay-store:test-store'`).Scan(&connections); err != nil || connections != 0 {
		t.Fatalf("connection escaped rollback: %d err=%v", connections, err)
	}
	if _, err := s.DB().Exec(`DROP TRIGGER fail_sepay_journal`); err != nil {
		t.Fatal(err)
	}
	r, err = s.IngestSePayNotification(ctx, in)
	if err != nil || r.Event == nil {
		t.Fatalf("recovered retry=%+v err=%v", r, err)
	}
	sepayTestFinancialCounts(t, s, 1)
}

func TestSePayObservePreActivationReviewsAndNoReplay(t *testing.T) {
	for _, reason := range []string{"OBSERVATION", "PRE_ACTIVATION", "INVALID_TEMPLATE", "OUTGOING", "ACCOUNT_MISMATCH", "INVALID_AMOUNT", "INVALID_TRANSACTION_DATE", "MISSING_REFERENCE", "EDITED_MESSAGE"} {
		t.Run(reason, func(t *testing.T) {
			s := sepayTestStore(t)
			in := sepayTestInput(1, 10, "review-reference")
			switch reason {
			case "OBSERVATION":
				in.Mode = "observe"
			case "PRE_ACTIVATION":
				in.ActivationAt = in.Credit.TransactionAt.Add(time.Second)
			default:
				in.ReviewReason, in.Credit = reason, nil
			}
			sepayTestPayload(&in, "private review memo")
			r, err := s.IngestSePayNotification(context.Background(), in)
			if err != nil || r.ReviewReason != reason || r.Event != nil {
				t.Fatalf("review=%+v err=%v", r, err)
			}
			sepayTestFinancialCounts(t, s, 0)
			sepayTestCount(t, s, "sepay_telegram_inbox", 1)
			last, err := s.LastSePayMessageAt(context.Background(), in.StoreKey)
			if err != nil || (last != nil) != (reason == "OBSERVATION") {
				t.Fatalf("last=%v reason=%s err=%v", last, reason, err)
			}
			in.Mode = "active"
			in.ActivationAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			r, err = s.IngestSePayNotification(context.Background(), in)
			if err != nil || !r.Duplicate || r.Event != nil || r.ReviewReason != reason {
				t.Fatalf("review replay=%+v err=%v", r, err)
			}
			sepayTestFinancialCounts(t, s, 0)
		})
	}
}

func TestSePayReceiverPinnedByObservationAndCannotRelabel(t *testing.T) {
	for _, field := range []string{"bank", "account"} {
		t.Run(field, func(t *testing.T) {
			s := sepayTestStore(t)
			in := sepayTestInput(1, 10, "identity-reference")
			in.Mode = "observe"
			if _, err := s.IngestSePayNotification(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			in.Mode, in.UpdateID, in.MessageID = "active", 2, 11
			if field == "bank" {
				in.BankCode = "BIDV"
			} else {
				in.AccountNumber = "OTHER123"
			}
			sepayTestPayload(&in, "other receiver memo")
			if _, err := s.IngestSePayNotification(context.Background(), in); !errors.Is(err, ErrSePayReceiverMismatch) {
				t.Fatalf("receiver relabeled: %v", err)
			}
			sepayTestCount(t, s, "sepay_telegram_inbox", 1)
			sepayTestFinancialCounts(t, s, 0)
			// A newly approved store key is independent, even for the same bank.
			in.StoreKey = "new-store"
			r, err := s.IngestSePayNotification(context.Background(), in)
			if err != nil || r.Event == nil {
				t.Fatalf("new store key cannot change receiver: %+v %v", r, err)
			}
		})
	}
}

func TestSePaySourceWindowAndTimestampScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		age    time.Duration
		source string
	}{{"current", 0, "REALTIME"}, {"future-allowed", -20 * time.Second, "REALTIME"}, {"old", 130 * time.Second, "CATCH_UP"}, {"future", -40 * time.Second, "CATCH_UP"}} {
		t.Run(tc.name, func(t *testing.T) {
			s := sepayTestStore(t)
			in := sepayTestInput(1, 10, "source-reference")
			in.MessageAt = time.Now().Add(-tc.age)
			sepayTestPayload(&in, "source memo")
			r, err := s.IngestSePayNotification(context.Background(), in)
			if err != nil || r.Event == nil {
				t.Fatalf("source ingest: %+v %v", r, err)
			}
			var payload map[string]any
			if err := json.Unmarshal(r.Event.Payload, &payload); err != nil || payload["source"] != tc.source {
				t.Fatalf("source=%v want=%s err=%v", payload["source"], tc.source, err)
			}
			last, err := s.LastSePayMessageAt(context.Background(), "test-store")
			if err != nil || last == nil {
				t.Fatalf("last=%v err=%v", last, err)
			}
			other, err := s.LastSePayMessageAt(context.Background(), "other-store")
			if err != nil || other != nil {
				t.Fatalf("other store last=%v err=%v", other, err)
			}
			// Reviews must not advance lastMessageAt; accepted retry also keeps
			// its original receive timestamp rather than simulating new traffic.
			review := in
			review.UpdateID, review.MessageID, review.ReviewReason, review.Credit = 2, 11, "INVALID_AMOUNT", nil
			sepayTestPayload(&review, "bad amount memo")
			if _, err := s.IngestSePayNotification(context.Background(), review); err != nil {
				t.Fatal(err)
			}
			if _, err := s.IngestSePayNotification(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			after, err := s.LastSePayMessageAt(context.Background(), "test-store")
			if err != nil || after == nil || !after.Equal(*last) {
				t.Fatalf("review/retry changed last: %v -> %v err=%v", last, after, err)
			}
		})
	}
}

func TestSePayRejectsMissingKeyringAndInvalidCreditCannotWriteMoney(t *testing.T) {
	s := sepayTestStore(t)
	keyring := s.keyring
	s.WithKeyring(nil)
	if _, err := s.IngestSePayNotification(context.Background(), sepayTestInput(1, 10, "key-reference")); err == nil {
		t.Fatal("missing keyring allowed ingest")
	}
	sepayTestCount(t, s, "sepay_telegram_inbox", 0)
	s.WithKeyring(keyring)
	for i, tc := range []struct {
		reason string
		credit SePayCredit
	}{{"INVALID_AMOUNT", SePayCredit{AmountVND: -1, Reference: "negative", TransactionAt: time.Now()}}, {"INVALID_AMOUNT", SePayCredit{AmountVND: 9007199254740992, Reference: "oversize", TransactionAt: time.Now()}}, {"INVALID_TRANSACTION_DATE", SePayCredit{AmountVND: 50000, Reference: "no-date"}}, {"MISSING_REFERENCE", SePayCredit{AmountVND: 50000, TransactionAt: time.Now()}}} {
		in := sepayTestInput(int64(i+1), int64(i+10), "invalid-credit")
		in.Credit = &tc.credit
		r, err := s.IngestSePayNotification(context.Background(), in)
		if err != nil || r.ReviewReason != tc.reason || r.Event != nil {
			t.Fatalf("invalid credit=%+v err=%v", r, err)
		}
	}
	sepayTestFinancialCounts(t, s, 0)
}

func TestSePaySameAmountDifferentReferencesRemainDistinct(t *testing.T) {
	s := sepayTestStore(t)
	settlementTestEndpoint(t, s)
	var firstID string
	for i := range 2 {
		in := sepayTestInput(int64(i+1), int64(i+10), fmt.Sprintf("same-amount-ref-%d", i))
		r, err := s.IngestSePayNotification(context.Background(), in)
		if err != nil || r.Event == nil || r.Duplicate || r.Event.TransactionID == firstID {
			t.Fatalf("distinct reference collapsed: %+v err=%v", r, err)
		}
		firstID = r.Event.TransactionID
	}
	sepayTestFinancialCounts(t, s, 2)
}

func TestSePayGateBlocksRetriesAndConflictCandidates(t *testing.T) {
	ctx := context.Background()
	s := sepayTestStore(t)
	settlementTestEndpoint(t, s)
	in := sepayTestInput(1, 10, "gated-existing")
	if _, err := s.IngestSePayNotification(ctx, in); err != nil {
		t.Fatal(err)
	}
	gate, err := s.AcquireMutationGate(ctx, "sepay-retry-test", time.Minute, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, conflict := range []bool{false, true} {
		candidate := in
		if conflict {
			sepayTestPayload(&candidate, "changed content while gate locked")
		}
		if _, err := s.IngestSePayNotification(ctx, candidate); !errors.Is(err, ErrMutationGateLocked) {
			t.Fatalf("existing-update gate bypass: %v", err)
		}
	}
	sepayTestFinancialCounts(t, s, 1)
	sepayTestCount(t, s, "sepay_telegram_inbox", 1)
	sepayTestCount(t, s, "transaction_quarantine", 0)
	if err := s.ReleaseMutationGate(ctx, "sepay-retry-test", gate.LeaseToken); err != nil {
		t.Fatal(err)
	}
	sepayTestPayload(&in, "changed content while gate locked")
	r, err := s.IngestSePayNotification(ctx, in)
	if err != nil || r.ReviewReason != "REFERENCE_CONFLICT" || r.Event != nil {
		t.Fatalf("recovered conflict=%+v err=%v", r, err)
	}
	sepayTestCount(t, s, "transaction_quarantine", 1)
}

func TestSePayCreditDoesNotSettleSameAmountPayOSOrders(t *testing.T) {
	s := sepayTestStore(t)
	orders := make([]PaymentOrder, 2)
	for i := range orders {
		orders[i], _ = settlementTestOrder(t, s, fmt.Sprintf("same-amount-%d", i), 50000, true)
	}
	in := sepayTestInput(1, 10, "sepay-only")
	if r, err := s.IngestSePayNotification(context.Background(), in); err != nil || r.Event == nil {
		t.Fatalf("ingest=%+v err=%v", r, err)
	}
	for _, order := range orders {
		current, err := s.PaymentOrder(context.Background(), order.ID)
		if err != nil || current.Status != "PENDING" || current.TransactionID != "" {
			t.Fatalf("SePay settled payOS: %+v err=%v", current, err)
		}
	}
	sepayTestCount(t, s, "payment_receipts", 0)
	sepayTestCount(t, s, "sepay_receipts", 1)
}

func TestSePayMigrationPreservesVersion16History(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v16.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,checksum TEXT NOT NULL,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.version >= 17 {
			break
		}
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("migration%d: %v", migration.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations VALUES(?,?,?)`, migration.version, migration.checksum, "2026-10-09T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO transactions(id,connection_id,semantic_key,canonical_hash,transaction_date,effective_date,credit,parser_version,first_seen_at,transaction_at_iso,transaction_day,date_precision,ingest_source) VALUES('old-txn','payos-klb','old-key','old-hash','2026-10-09T00:00:00Z','2026-10-09T00:00:00Z',50000,'payos-v1','2026-10-09T00:00:00Z','2026-10-09T00:00:00Z','2026-10-09','datetime','REALTIME')`); err != nil {
		t.Fatal(err)
	}
	var before []any
	row := db.QueryRow(`SELECT id,canonical_hash,credit,transaction_day,ingest_source FROM transactions WHERE id='old-txn'`)
	var id, hash, day, source string
	var credit int64
	if err := row.Scan(&id, &hash, &credit, &day, &source); err != nil {
		t.Fatal(err)
	}
	before = []any{id, hash, credit, day, source}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.DB().QueryRow(`SELECT id,canonical_hash,credit,transaction_day,ingest_source FROM transactions WHERE id='old-txn'`).Scan(&id, &hash, &credit, &day, &source); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, []any{id, hash, credit, day, source}) {
		t.Fatal("migration relabeled historical transaction")
	}
	var checksum string
	if err := s.DB().QueryRow(`SELECT checksum FROM schema_migrations WHERE version=17`).Scan(&checksum); err != nil || checksum != "2026-10-10-v17-sepay-store" {
		t.Fatalf("migration17 checksum=%s err=%v", checksum, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migration replay: %v", err)
	}
	for _, table := range []string{"sepay_telegram_inbox", "sepay_receipts"} {
		rows, err := s.DB().Query(`SELECT name,"notnull" FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			var notnull int
			if err := rows.Scan(&name, &notnull); err != nil {
				t.Fatal(err)
			}
			if notnull != 1 && !(table == "sepay_telegram_inbox" && name == "transaction_id") {
				t.Fatalf("%s.%s unexpectedly nullable", table, name)
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// A keyring can be installed after migration; migration requires no secrets.
	keyring, err := security.NewKeyring(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.WithKeyring(keyring)
	if _, err := s.IngestSePayNotification(ctx, sepayTestInput(1, 10, "post-migration")); err != nil {
		t.Fatal(err)
	}
}

func TestSePayReviewPaginationAllowlistAndPrivacy(t *testing.T) {
	s := sepayTestStore(t)
	ctx := context.Background()
	at := "2026-10-10T12:00:00Z"
	reasons := []string{"INVALID_TEMPLATE", "ACCOUNT_MISMATCH", "INVALID_AMOUNT", "INVALID_TRANSACTION_DATE", "MISSING_REFERENCE", "REFERENCE_CONFLICT", "EDITED_MESSAGE"}
	for i := range 110 {
		botID := int64(900001 + i%3)
		if _, err := s.DB().Exec(`INSERT INTO sepay_telegram_inbox(bot_id,update_id,chat_id,message_id,payload_hash,payload_envelope,store_key,reason,received_at) VALUES(?,?,-100900003,?,'private-hash',?,'test-store',?,?)`, botID, i, int64(9007199254740993)+int64(i), []byte("encrypted private payer evidence"), reasons[i%len(reasons)], at); err != nil {
			t.Fatal(err)
		}
	}
	for i, reason := range []string{"ACCEPTED", "DUPLICATE", "OBSERVATION", "PRE_ACTIVATION", "OUTGOING"} {
		if _, err := s.DB().Exec(`INSERT INTO sepay_telegram_inbox(bot_id,update_id,chat_id,message_id,payload_hash,payload_envelope,store_key,reason,received_at) VALUES(900001,?,-100900003,?,'private-hash',?,'test-store',?,?)`, 200+i, 200+i, []byte("private evidence"), reason, "2026-10-10T13:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	count, err := s.SePayReviewCount(ctx)
	if err != nil || count != 110 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	defaultPage, err := s.ListSePayReviews(ctx, "", 0)
	if err != nil || len(defaultPage.Items) != 50 || defaultPage.NextCursor == "" {
		t.Fatalf("default pagination: %+v err=%v", defaultPage, err)
	}
	maxPage, err := s.ListSePayReviews(ctx, "", 1000)
	if err != nil || len(maxPage.Items) != 100 || maxPage.NextCursor == "" {
		t.Fatalf("max pagination: %+v err=%v", maxPage, err)
	}
	var got []string
	cursor := ""
	for {
		page, err := s.ListSePayReviews(ctx, cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			got = append(got, item.MessageID)
			encoded, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 4 || fields["storeKey"] != "test-store" || fields["messageId"] != item.MessageID || fields["reason"] != item.Reason || fields["receivedAt"] != at {
				t.Fatalf("unsafe review projection: %s", encoded)
			}
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = page.NextCursor
	}
	var want []string
	for botOffset := 2; botOffset >= 0; botOffset-- {
		for updateID := 109; updateID >= 0; updateID-- {
			if updateID%3 == botOffset {
				want = append(want, fmt.Sprint(int64(9007199254740993)+int64(updateID)))
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tie pagination lost/repeated/reordered rows: got=%v want=%v", got, want)
	}
	for _, invalid := range []string{"!", encodeCursor(at, "900001"), encodeCursor(at, "900001:bad"), encodeCursor(at, "0:1"), encodeCursor(at, "900001:-1"), encodeCursor("not-time", "900001:1")} {
		if _, err := s.ListSePayReviews(ctx, invalid, 20); !errors.Is(err, ErrInvalidSePayReviewCursor) {
			t.Fatalf("invalid cursor accepted: %q err=%v", invalid, err)
		}
	}
}

func TestSePayReviewsPersistedConflictsAndEditsKeepFirstObservation(t *testing.T) {
	s := sepayTestStore(t)
	ctx := context.Background()
	first := sepayTestInput(1, 10, "first-reference")
	if _, err := s.IngestSePayNotification(ctx, first); err != nil {
		t.Fatal(err)
	}
	conflict := sepayTestInput(2, 20, "first-reference")
	conflict.Credit.AmountVND++
	if result, err := s.IngestSePayNotification(ctx, conflict); err != nil || result.ReviewReason != "REFERENCE_CONFLICT" {
		t.Fatalf("conflict=%+v err=%v", result, err)
	}
	edit := sepayTestInput(3, 10, "first-reference")
	edit.ReviewReason = "EDITED_MESSAGE"
	sepayTestPayload(&edit, "private edited candidate")
	if _, err := s.IngestSePayNotification(ctx, edit); err != nil {
		t.Fatal(err)
	}
	// Reused update conflicts are encrypted quarantine candidates, not a second
	// inbox decision; the original ACCEPTED row remains immutable.
	changed := first
	sepayTestPayload(&changed, "private changed update")
	if _, err := s.IngestSePayNotification(ctx, changed); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListSePayReviews(ctx, "", 50)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("reviews=%+v err=%v", page, err)
	}
	seen := map[string]bool{}
	for _, item := range page.Items {
		seen[item.Reason] = true
	}
	if !seen["REFERENCE_CONFLICT"] || !seen["EDITED_MESSAGE"] {
		t.Fatalf("missing persisted inbox candidates: %+v", page)
	}
	count, err := s.SePayReviewCount(ctx)
	if err != nil || count != 2 {
		t.Fatalf("review count=%d err=%v", count, err)
	}
	sepayTestCount(t, s, "transactions", 1)
	var reason string
	if err := s.DB().QueryRow(`SELECT reason FROM sepay_telegram_inbox WHERE bot_id=? AND update_id=?`, first.BotID, first.UpdateID).Scan(&reason); err != nil || reason != "ACCEPTED" {
		t.Fatalf("first observation replaced: %s err=%v", reason, err)
	}
}

func TestSePayReviewCursorOrdersTimestampBeforeTelegramIDs(t *testing.T) {
	s := sepayTestStore(t)
	for _, row := range []struct {
		at        string
		botID     int64
		updateID  int64
		messageID int64
	}{
		{"2026-10-10T11:00:00Z", 900003, 99, 1},
		{"2026-10-10T12:00:00Z", 900001, 1, 2},
	} {
		if _, err := s.DB().Exec(`INSERT INTO sepay_telegram_inbox(bot_id,update_id,chat_id,message_id,payload_hash,payload_envelope,store_key,reason,received_at) VALUES(?,?,-100900003,?,'private-hash',?,'test-store','INVALID_AMOUNT',?)`, row.botID, row.updateID, row.messageID, []byte("encrypted evidence"), row.at); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.ListSePayReviews(context.Background(), "", 1)
	if err != nil || len(first.Items) != 1 || first.Items[0].MessageID != "2" || first.NextCursor == "" {
		t.Fatalf("primary timestamp order: %+v err=%v", first, err)
	}
	last, err := s.ListSePayReviews(context.Background(), first.NextCursor, 1)
	if err != nil || len(last.Items) != 1 || last.Items[0].MessageID != "1" || last.NextCursor != "" {
		t.Fatalf("cross-timestamp cursor: %+v err=%v", last, err)
	}
}
