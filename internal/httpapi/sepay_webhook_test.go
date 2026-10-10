package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/sepay"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

const sepayWebhookPath = "/api/integrations/sepay/telegram"

func sepayWebhookConfig() sepay.Config {
	return sepay.Config{
		Mode: sepay.ModeActive, StoreKey: "test-store", StoreName: "Test Store",
		BankCode: "TESTBANK", BankName: "Test Bank", AccountNumber: "VA123456", AccountName: "TEST RECEIVER",
		QRPayload: "0002010102116304ABCD", BotID: 900001, ChatID: -100900003, SenderBotID: 900002,
		WebhookSecret: base64.RawURLEncoding.EncodeToString([]byte("01234567890123456789012345678901")),
		ActivationAt:  time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
}

func sepayWebhookKeyring(t *testing.T, store *storage.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte("0123456789012345678901234567890123456789012345678901234567890123"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyring, err := security.LoadKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	store.WithKeyring(keyring)
}

func sepayWebhookSetup(t *testing.T, cfg sepay.Config) (*storage.Store, http.Handler, *atomic.Int64) {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "sepay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sepayWebhookKeyring(t, store)
	commits := &atomic.Int64{}
	service := sepay.NewService(cfg, store, func(event storage.EventNotification) {
		var receipts int
		if err := store.DB().QueryRow("SELECT count(*) FROM sepay_receipts WHERE transaction_id=?", event.TransactionID).Scan(&receipts); err != nil || receipts != 1 {
			t.Errorf("publish before durable commit: receipts=%d err=%v", receipts, err)
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload["provider"] != "SEPAY" || payload["orderCode"] != nil || payload["paymentOrigin"] != nil {
			t.Errorf("SePay event crossed payOS contract: %s err=%v", event.Payload, err)
		}
		commits.Add(1)
	})
	return store, New(config.Config{Production: true}, store).WithSePay(service).Handler(), commits
}

func sepayWebhookEnvelope(t *testing.T, updateID, messageID int64, reference string) map[string]any {
	t.Helper()
	cfg := sepayWebhookConfig()
	return map[string]any{
		"update_id": updateID,
		"message": map[string]any{
			"message_id": messageID, "date": time.Now().Unix(),
			"chat": map[string]any{"id": cfg.ChatID, "type": "supergroup"},
			"from": map[string]any{"id": cfg.SenderBotID, "is_bot": true, "first_name": "SePay"},
			"text": "SEPAY_STORE_V1\ndirection=vào\namount=50,000\naccount=VA123456\nbank=Test Bank\ntime=2026-10-10 10:05:00\nreference=" + reference + "\ncontent=private payer memo\namount=999999",
		},
	}
}

func sepayWebhookBody(t *testing.T, envelope map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sepayWebhookPost(handler http.Handler, secret string, raw []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://transactions.example.test"+sepayWebhookPath, bytes.NewReader(raw))
	r.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestSePayWebhookAuthenticatesBeforeParsing(t *testing.T) {
	cfg := sepayWebhookConfig()
	store, handler, _ := sepayWebhookSetup(t, cfg)
	for _, secret := range []string{"", "wrong", cfg.WebhookSecret + "x"} {
		w := sepayWebhookPost(handler, secret, []byte(strings.Repeat("x", maxSePayWebhookBytes+1)))
		if w.Code != http.StatusUnauthorized || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("secret not checked before parsing: %d", w.Code)
		}
	}
	if webhookTestCount(t, store, "sepay_telegram_inbox") != 0 {
		t.Fatal("unauthenticated payload persisted")
	}
}

func TestSePayWebhookStrictJSONAndSize(t *testing.T) {
	cfg := sepayWebhookConfig()
	store, handler, _ := sepayWebhookSetup(t, cfg)
	for _, body := range []string{
		"", "null", "[]", "1", "{", "{} {}", "{} trailing",
		`{"update_id":1,"update_id":2}`, `{"message":{"chat":{"id":1,"id":2}}}`,
		`{"update_id":1.5}`, `{"update_id":9223372036854775808}`,
		`{"message":{"chat":{"id":-1.5}}}`, `{"message":{"from":{"id":"900002"}}}`,
		`{"nested":` + strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66) + "}",
	} {
		if w := sepayWebhookPost(handler, cfg.WebhookSecret, []byte(body)); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid JSON accepted: %d body=%s", w.Code, body)
		}
	}
	if w := sepayWebhookPost(handler, cfg.WebhookSecret, []byte(strings.Repeat(" ", maxSePayWebhookBytes)+"{}")); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body accepted: %d", w.Code)
	}
	boundary := []byte("{}" + strings.Repeat(" ", maxSePayWebhookBytes-2))
	if w := sepayWebhookPost(handler, cfg.WebhookSecret, boundary); w.Code != http.StatusOK {
		t.Fatalf("64 KiB ignored object rejected: %d", w.Code)
	}
	if webhookTestCount(t, store, "sepay_telegram_inbox") != 0 {
		t.Fatal("invalid or unsupported update persisted")
	}
}

func TestSePayWebhookUnavailableRetries(t *testing.T) {
	cfg := sepayWebhookConfig()
	for _, server := range []*Server{
		New(config.Config{}, nil),
		New(config.Config{}, nil).WithSePay(sepay.NewService(sepay.Config{Mode: sepay.ModeDisabled}, nil, nil)),
		New(config.Config{}, nil).WithSePay(sepay.NewService(cfg, nil, nil)),
	} {
		w := sepayWebhookPost(server.Handler(), cfg.WebhookSecret, sepayWebhookBody(t, sepayWebhookEnvelope(t, 1, 1, "UNAVAILABLE")))
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "5" {
			t.Fatalf("missing service acknowledged money: %d retry=%q", w.Code, w.Header().Get("Retry-After"))
		}
	}
}

func TestSePayWebhookIgnoresUntrustedWithoutPII(t *testing.T) {
	cfg := sepayWebhookConfig()
	store, handler, commits := sepayWebhookSetup(t, cfg)
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { m["from"].(map[string]any)["is_bot"] = false },
		func(m map[string]any) { m["from"].(map[string]any)["id"] = int64(900004) },
		func(m map[string]any) { m["chat"].(map[string]any)["id"] = int64(-100900004) },
		func(m map[string]any) { m["message_thread_id"] = int64(42) },
		func(m map[string]any) { m["sender_chat"] = map[string]any{"id": cfg.ChatID} },
		func(m map[string]any) { m["forward_origin"] = map[string]any{"type": "user"} },
		func(m map[string]any) { m["via_bot"] = map[string]any{"id": cfg.SenderBotID, "is_bot": true} },
		func(m map[string]any) { m["reply_to_message"] = map[string]any{"message_id": 9} },
		func(m map[string]any) { m["new_chat_members"] = []any{map[string]any{"id": 9}} },
	} {
		envelope := sepayWebhookEnvelope(t, 1, 1, "UNTRUSTED")
		mutate(envelope["message"].(map[string]any))
		if w := sepayWebhookPost(handler, cfg.WebhookSecret, sepayWebhookBody(t, envelope)); w.Code != http.StatusOK {
			t.Fatalf("untrusted update not ignored: %d", w.Code)
		}
	}
	for _, table := range []string{"transactions", "sepay_telegram_inbox", "sepay_receipts", "event_journal"} {
		if webhookTestCount(t, store, table) != 0 {
			t.Fatalf("untrusted update persisted in %s", table)
		}
	}
	if commits.Load() != 0 {
		t.Fatal("untrusted update published")
	}
}

func TestSePayWebhookDurableRetryAndInt64IDs(t *testing.T) {
	cfg := sepayWebhookConfig()
	store, handler, commits := sepayWebhookSetup(t, cfg)
	const updateID int64 = 9007199254740993
	const messageID int64 = 9007199254740995
	raw := sepayWebhookBody(t, sepayWebhookEnvelope(t, updateID, messageID, "SEPAY_TEST_001"))
	for range 10 {
		if w := sepayWebhookPost(handler, cfg.WebhookSecret, raw); w.Code != http.StatusOK {
			t.Fatalf("durable update failed: %d %s", w.Code, w.Body.String())
		}
	}
	for _, table := range []string{"transactions", "sepay_telegram_inbox", "sepay_receipts", "event_journal"} {
		if count := webhookTestCount(t, store, table); count != 1 {
			t.Fatalf("retry duplicated %s: %d", table, count)
		}
	}
	var gotUpdate, gotMessage int64
	var reason string
	if err := store.DB().QueryRow("SELECT update_id,message_id,reason FROM sepay_telegram_inbox").Scan(&gotUpdate, &gotMessage, &reason); err != nil || gotUpdate != updateID || gotMessage != messageID || reason != "ACCEPTED" {
		t.Fatalf("numeric IDs lost precision: %d %d %s err=%v", gotUpdate, gotMessage, reason, err)
	}
	if commits.Load() != 1 {
		t.Fatalf("retry republished %d times", commits.Load())
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/v1/sepay-store", nil))
	var dto SePayStoreConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil || w.Code != 200 || dto.LastMessageAt == nil {
		t.Fatalf("public timestamp missing after durable receipt: %d %s err=%v", w.Code, w.Body.String(), err)
	}
	last, err := store.LastSePayMessageAt(context.Background(), cfg.StoreKey)
	if err != nil || last == nil || !dto.LastMessageAt.Equal(*last) {
		t.Fatal("public timestamp did not come from durable inbox")
	}
}

func TestSePayWebhookGateAndAtomicFailureRetry(t *testing.T) {
	cfg := sepayWebhookConfig()
	store, handler, commits := sepayWebhookSetup(t, cfg)
	raw := sepayWebhookBody(t, sepayWebhookEnvelope(t, 1, 1, "GATE_RETRY"))
	gate, err := store.AcquireMutationGate(context.Background(), "sepay-http-test", time.Minute, "upgrade")
	if err != nil {
		t.Fatal(err)
	}
	w := sepayWebhookPost(handler, cfg.WebhookSecret, raw)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "5" {
		t.Fatalf("gate swallowed update: %d", w.Code)
	}
	if err := store.ReleaseMutationGate(context.Background(), "sepay-http-test", gate.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec("CREATE TRIGGER sepay_fail_journal BEFORE INSERT ON event_journal BEGIN SELECT RAISE(ABORT,'fixture failure'); END"); err != nil {
		t.Fatal(err)
	}
	w = sepayWebhookPost(handler, cfg.WebhookSecret, raw)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "5" {
		t.Fatalf("DB failure swallowed update: %d", w.Code)
	}
	for _, table := range []string{"transactions", "sepay_telegram_inbox", "sepay_receipts", "event_journal"} {
		if webhookTestCount(t, store, table) != 0 {
			t.Fatalf("partial commit in %s", table)
		}
	}
	if commits.Load() != 0 {
		t.Fatal("rollback published credit")
	}
	if _, err := store.DB().Exec("DROP TRIGGER sepay_fail_journal"); err != nil {
		t.Fatal(err)
	}
	if w := sepayWebhookPost(handler, cfg.WebhookSecret, raw); w.Code != http.StatusOK || commits.Load() != 1 {
		t.Fatalf("retry failed after DB recovery: %d commits=%d", w.Code, commits.Load())
	}
}

func TestSePayWebhookReviewsAndObserveDoNotCreateMoney(t *testing.T) {
	for _, mode := range []string{sepay.ModeActive, sepay.ModeObserve} {
		t.Run(mode, func(t *testing.T) {
			cfg := sepayWebhookConfig()
			cfg.Mode = mode
			store, handler, commits := sepayWebhookSetup(t, cfg)
			envelope := sepayWebhookEnvelope(t, 1, 1, "REVIEW")
			if mode == sepay.ModeActive {
				envelope["message"].(map[string]any)["text"] = "SEPAY_STORE_V1\ndirection=vào\namount=50.00\naccount=VA123456\nbank=Test Bank\ntime=2026-10-10 10:05:00\nreference=REVIEW\ncontent=private"
			}
			if w := sepayWebhookPost(handler, cfg.WebhookSecret, sepayWebhookBody(t, envelope)); w.Code != http.StatusOK {
				t.Fatalf("review not durable: %d", w.Code)
			}
			if webhookTestCount(t, store, "sepay_telegram_inbox") != 1 || webhookTestCount(t, store, "transactions") != 0 || commits.Load() != 0 {
				t.Fatal("review/observe crossed money boundary")
			}
			last, err := store.LastSePayMessageAt(context.Background(), cfg.StoreKey)
			if err != nil || (last != nil) != (mode == sepay.ModeObserve) {
				t.Fatalf("invalid template affected last parsed message: %v err=%v", last, err)
			}
		})
	}
}

func TestSePayWebhookDoesNotSettleMatchingPayOSOrders(t *testing.T) {
	f := newPaymentHTTPFixture(t)
	sepayWebhookKeyring(t, f.store)
	cfg := sepayWebhookConfig()
	f.handler = New(f.cfg, f.store).WithPayments(f.service).WithSePay(sepay.NewService(cfg, f.store, nil)).Handler()
	for i := 1; i <= 2; i++ {
		w := paymentRequest(f.handler, http.MethodPost, "/api/public/v1/payments", `{"amountVnd":50000,"origin":"STATIC_URL"}`, paymentKey(i), false)
		if w.Code != http.StatusCreated {
			t.Fatalf("payOS create %d failed: %d", i, w.Code)
		}
	}
	if w := sepayWebhookPost(f.handler, cfg.WebhookSecret, sepayWebhookBody(t, sepayWebhookEnvelope(t, 1, 1, "SAME_AMOUNT"))); w.Code != http.StatusOK {
		t.Fatalf("SePay credit failed: %d", w.Code)
	}
	var pending int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM payment_orders WHERE amount_vnd=50000 AND status='PENDING'").Scan(&pending); err != nil || pending != 2 {
		t.Fatalf("SePay settled payOS orders: %d err=%v", pending, err)
	}
	if webhookTestCount(t, f.store, "payment_receipts") != 0 || webhookTestCount(t, f.store, "sepay_receipts") != 1 {
		t.Fatal("receipt provenance crossed providers")
	}
}

func TestSePayWebhookOnlyPost(t *testing.T) {
	_, handler, _ := sepayWebhookSetup(t, sepayWebhookConfig())
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, sepayWebhookPath, nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("callback accepts %s: %d", method, w.Code)
		}
	}
	for _, path := range []string{sepayWebhookPath + "/", sepayWebhookPath + "/child"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("callback accepts path %s: %d", path, w.Code)
		}
	}
}

func TestSePayWebhookReferenceReplayAndEditedReview(t *testing.T) {
	cfg := sepayWebhookConfig()
	store, handler, commits := sepayWebhookSetup(t, cfg)
	for _, ids := range [][2]int64{{1, 1}, {2, 2}} {
		raw := sepayWebhookBody(t, sepayWebhookEnvelope(t, ids[0], ids[1], "REFERENCE_REPLAY"))
		if w := sepayWebhookPost(handler, cfg.WebhookSecret, raw); w.Code != http.StatusOK {
			t.Fatalf("reference replay failed: %d", w.Code)
		}
	}
	if webhookTestCount(t, store, "transactions") != 1 || webhookTestCount(t, store, "sepay_receipts") != 1 || commits.Load() != 1 {
		t.Fatal("same reference created another credit")
	}
	var duplicateReason string
	if err := store.DB().QueryRow("SELECT reason FROM sepay_telegram_inbox WHERE update_id=2").Scan(&duplicateReason); err != nil || duplicateReason != "DUPLICATE" {
		t.Fatalf("new message reference not deduped: %s err=%v", duplicateReason, err)
	}
	edited := sepayWebhookEnvelope(t, 3, 1, "REFERENCE_REPLAY")
	message := edited["message"].(map[string]any)
	message["text"] = strings.Replace(message["text"].(string), "amount=50,000", "amount=100,000", 1)
	edited["edited_message"] = message
	delete(edited, "message")
	if w := sepayWebhookPost(handler, cfg.WebhookSecret, sepayWebhookBody(t, edited)); w.Code != http.StatusOK {
		t.Fatalf("edited source not durably reviewed: %d", w.Code)
	}
	var reviewReason string
	var transactionID *string
	if err := store.DB().QueryRow("SELECT reason,transaction_id FROM sepay_telegram_inbox WHERE update_id=3").Scan(&reviewReason, &transactionID); err != nil || reviewReason != "EDITED_MESSAGE" || transactionID != nil {
		t.Fatalf("edited message became financial evidence: %s tx=%v err=%v", reviewReason, transactionID, err)
	}
	if webhookTestCount(t, store, "transactions") != 1 || webhookTestCount(t, store, "event_journal") != 1 || commits.Load() != 1 {
		t.Fatal("edited message changed first credit")
	}
}
