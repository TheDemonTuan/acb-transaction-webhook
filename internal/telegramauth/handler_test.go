package telegramauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/challenge"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type botFixture struct {
	mu              sync.Mutex
	requests        []string
	messages        []map[string]json.RawMessage
	edits           []map[string]json.RawMessage
	callbackAnswers []map[string]json.RawMessage
	updates         []Update
	messageID       int64
	webhook         string
	failCode        int
	retryAfter      int
	failEditCode    int
	afterSend       func()
	afterEdit       func()
	deletedIDs      []int64
}

func (f *botFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.requests = append(f.requests, method)
	if method == "editMessageText" && f.failEditCode != 0 {
		w.WriteHeader(f.failEditCode)
		fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"Bad Request: message can't be edited"}`, f.failEditCode)
		return
	}
	if f.failCode != 0 {
		w.WriteHeader(f.failCode)
		fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"secret token 123:secret and raw bank error","parameters":{"retry_after":%d}}`, f.failCode, f.retryAfter)
		return
	}
	result := any(true)
	switch method {
	case "getMe":
		result = map[string]any{"id": int64(42), "is_bot": true}
	case "getWebhookInfo":
		result = map[string]any{"url": f.webhook}
	case "editMessageText":
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		f.edits = append(f.edits, body)
		if f.afterEdit != nil {
			f.afterEdit()
		}
	case "sendMessage":
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		f.messages = append(f.messages, body)
		f.messageID++
		result = map[string]int64{"message_id": f.messageID}
		if f.afterSend != nil {
			f.afterSend()
		}
	case "deleteMessage":
		var body struct {
			MessageID int64 `json:"message_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		f.deletedIDs = append(f.deletedIDs, body.MessageID)
	case "answerCallbackQuery":
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		f.callbackAnswers = append(f.callbackAnswers, body)
	case "getUpdates":
		result = f.updates
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}
func (f *botFixture) lastAction(t *testing.T) (string, int64) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var keyboard inlineKeyboard
	if err := json.Unmarshal(f.messages[len(f.messages)-1]["reply_markup"], &keyboard); err != nil {
		t.Fatal(err)
	}
	return keyboard.Rows[0][0].Data, f.messageID
}
func testTelegram(t *testing.T) (context.Context, *storage.Store, *Handler, *botFixture, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "telegram.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	f := &botFixture{messageID: 100}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	client, err := NewClient("123:secret", ClientOptions{BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CheckConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TelegramAuthState(ctx, 42); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(HandlerOptions{Store: store, Client: client, ChatID: -123, UserID: 456, PublicOrigin: "https://bank.example"})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, handler, f, path
}
func command(text string) Update {
	return Update{Message: &Message{ID: 9, Chat: Chat{ID: -123, Type: "private"}, From: &User{ID: 456}, Text: text}}
}
func callback(data string, messageID int64) Update {
	return Update{ID: 20, Callback: &CallbackQuery{ID: "callback", From: User{ID: 456}, Message: &Message{ID: messageID, Chat: Chat{ID: -123, Type: "private"}, From: &User{ID: 42, IsBot: true}}, Data: data}}
}

func TestTelegramUnconfiguredNavigationNeverCreatesConsent(t *testing.T) {
	ctx, store, h, f, _ := testTelegram(t)
	mustExec(t, store, `DELETE FROM connections`)
	for _, data := range []string{"nav:menu", "nav:status", "nav:help", "nav:credentials", "nav:logout"} {
		if err := h.HandleUpdate(ctx, callback(data, 100)); err != nil {
			t.Fatal(err)
		}
	}
	var actions, attempts int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM telegram_auth_actions`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if actions != 0 || attempts != 0 {
		t.Fatal("onboarding navigation granted bank authority")
	}
	if len(f.messages) != 5 {
		t.Fatal("read-only navigation failed without connection")
	}
}

type replyRecorder struct {
	calls int
	value string
	err   error
}

func (r *replyRecorder) HandleReply(_ context.Context, _, _, _ int64, text string) error {
	r.calls++
	r.value = text
	return r.err
}

func mustExec(t *testing.T, s *storage.Store, query string, args ...any) {
	t.Helper()
	if _, err := s.DB().Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramOperatorCommands(t *testing.T) {
	t.Run("authorization before text and leading zero reply", func(t *testing.T) {
		ctx, _, h, _, _ := testTelegram(t)
		broker := &replyRecorder{}
		h.ReplyBroker = broker
		for _, mutate := range []func(*Message){
			func(m *Message) { m.From.ID++ }, func(m *Message) { m.Chat.Type = "group" }, func(m *Message) { m.Chat.ID++ }, func(m *Message) { m.From.IsBot = true }, func(m *Message) { m.ForwardOrigin = json.RawMessage(`{}`) }, func(m *Message) { m.ForwardFrom = json.RawMessage(`{}`) }, func(m *Message) { m.ViaBot = json.RawMessage(`{}`) }, func(m *Message) { m.Document = json.RawMessage(`{}`) }, func(m *Message) { m.Photo = json.RawMessage(`[{}]`) }, func(m *Message) { m.Contact = json.RawMessage(`{}`) }, func(m *Message) { m.EditDate = 1 },
		} {
			u := command("001234")
			u.Message.ReplyTo = &Message{ID: 77}
			mutate(u.Message)
			if err := h.HandleUpdate(ctx, u); err != nil {
				t.Fatal(err)
			}
		}
		if broker.calls != 0 {
			t.Fatal("unauthorized/media reply reached bank broker")
		}
		u := command("001234")
		u.Message.ReplyTo = &Message{ID: 77}
		if err := h.HandleUpdate(ctx, u); err != nil {
			t.Fatal(err)
		}
		if broker.calls != 1 || broker.value != "001234" {
			t.Fatal("authorized leading-zero OTP changed")
		}
		h.ReplyBroker = nil
		if err := h.HandleUpdate(ctx, u); err == nil {
			t.Fatal("missing broker accepted bank reply")
		}
	})
	t.Run("pause durable resume retains circuit", func(t *testing.T) {
		ctx, s, h, _, path := testTelegram(t)
		c, err := s.Connection(ctx)
		if err != nil {
			t.Fatal(err)
		}
		e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, s, `UPDATE connections SET state='MONITORING'`)
		mustExec(t, s, `UPDATE auth_recovery_episodes SET state='MANUAL_REQUIRED',attempt_count=3 WHERE id=?`, e.ID)
		if err := h.HandleUpdate(ctx, command("/acb_pause")); err != nil {
			t.Fatal(err)
		}
		restarted, err := storage.OpenRuntime(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer restarted.Close()
		state, err := restarted.TelegramAuthState(ctx, 42)
		if err != nil || !state.Paused {
			t.Fatalf("pause lost on restart: %v", err)
		}
		connection, err := restarted.Connection(ctx)
		if err != nil || connection.State != "MONITORING" {
			t.Fatal("pause stopped verified monitoring")
		}
		h.Store = restarted
		if err := h.HandleUpdate(ctx, command("/acb_resume")); err != nil {
			t.Fatal(err)
		}
		state, err = restarted.TelegramAuthState(ctx, 42)
		if err != nil || state.Paused {
			t.Fatal("resume not persisted")
		}
		current, err := restarted.AuthRecoveryEpisode(ctx, e.ID)
		if err != nil || current.State != "MANUAL_REQUIRED" || current.AttemptCount != 3 || current.BudgetStartCount != 0 {
			t.Fatal("resume reset circuit")
		}
	})
	t.Run("offset failure restart never cancels twice", func(t *testing.T) {
		ctx, s, h, f, path := testTelegram(t)
		c, err := s.Connection(ctx)
		if err != nil {
			t.Fatal(err)
		}
		e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, s, `INSERT INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES(?,1,X'00','fixture',?)`, c.ID, time.Now().UTC().Format(time.RFC3339Nano))
		action, err := s.CreateTelegramAuthAction(ctx, storage.TelegramAuthAction{BotID: 42, ChatID: -123, UserID: 456, EpisodeID: e.ID, ExpectedGeneration: e.Generation, Action: "LOGIN"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DeliverTelegramAuthAction(ctx, action.ID, 100); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ConsumeTelegramAuthAction(ctx, action.ID, 42, -123, 456, 100, time.Now()); err != nil {
			t.Fatal(err)
		}
		a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.HandleUpdate(ctx, command("/acb_cancel")); err != nil {
			t.Fatal(err)
		}
		data, id := f.lastAction(t)
		f.updates = []Update{callback(data, id)}
		mustExec(t, s, `CREATE TRIGGER crash_offset BEFORE UPDATE OF next_update_id ON telegram_auth_state BEGIN SELECT RAISE(ABORT,'injected offset failure'); END`)
		if err := h.Client.pollOnce(ctx, s, h); err == nil {
			t.Fatal("offset failure hidden")
		}
		state, _ := s.TelegramAuthState(ctx, 42)
		if state.NextUpdateID != 0 {
			t.Fatal("failed offset advanced")
		}
		var cancelled string
		if err := s.DB().QueryRowContext(ctx, `SELECT status FROM auth_attempts WHERE id=?`, a.ID).Scan(&cancelled); err != nil || cancelled != "CANCELLED" {
			t.Fatal("cancel was not durable before offset failure")
		}
		mustExec(t, s, `DROP TRIGGER crash_offset`)
		restarted, err := storage.OpenRuntime(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer restarted.Close()
		h.Store = restarted
		if err := h.Client.pollOnce(ctx, restarted, h); err != nil {
			t.Fatal(err)
		}
		state, err = restarted.TelegramAuthState(ctx, 42)
		if err != nil || state.NextUpdateID != 21 {
			t.Fatal("replayed disposition did not advance offset")
		}
		if err := restarted.DB().QueryRowContext(ctx, `SELECT status FROM auth_attempts WHERE id=?`, a.ID).Scan(&cancelled); err != nil || cancelled != "CANCELLED" {
			t.Fatal("callback replay changed durable cancellation")
		}
		count := len(f.messages)
		if err := h.Client.pollOnce(ctx, restarted, h); err != nil {
			t.Fatal(err)
		}
		if len(f.messages) != count {
			t.Fatal("persisted old update was processed again")
		}
	})
	t.Run("cancel before watcher suppresses automatic admission", func(t *testing.T) {
		ctx, s, h, f, _ := testTelegram(t)
		c, err := s.Connection(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var before int
		if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_recovery_episodes`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if before != 0 {
			t.Fatal("fixture already reconciled")
		}
		if err := h.HandleUpdate(ctx, command("/acb_cancel")); err != nil {
			t.Fatal(err)
		}
		data, id := f.lastAction(t)
		if err := h.HandleUpdate(ctx, callback(data, id)); err != nil {
			t.Fatal(err)
		}
		cancelled, err := s.LatestAuthRecoveryEpisode(ctx, c.ID)
		if err != nil || cancelled.State != "CANCELLED" || cancelled.FinishedAt == "" {
			t.Fatalf("pre-watcher cancel not durable: %v", err)
		}
		reconciled, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
		if err != nil || reconciled.ID != cancelled.ID || reconciled.State != "CANCELLED" {
			t.Fatalf("watcher recreated cancelled trigger: %v", err)
		}
		if _, err := s.StartRecoveryAuthAttempt(ctx, reconciled.ID, reconciled.Generation, time.Minute); err == nil {
			t.Fatal("cancelled trigger admitted automatic login")
		}
		var attempts int
		if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_attempts`).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts != 0 {
			t.Fatal("cancelled trigger persisted an authentication attempt")
		}
	})
	t.Run("catchup retry preserves session and ordinal", func(t *testing.T) {
		ctx, s, h, f, _ := testTelegram(t)
		c, err := s.Connection(ctx)
		if err != nil {
			t.Fatal(err)
		}
		e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
		if err != nil {
			t.Fatal(err)
		}
		run, _, err := s.EnsureRecoveryRunWithPlan(ctx, c.ID, c.Generation, "attempt:gap-tail:2026-10-02", storage.RecoveryRunPlan{RangeFrom: "2026-10-01", RangeTo: "2026-10-02", NextDay: "2026-10-02"})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		mustExec(t, s, `UPDATE connections SET state='MONITORING'`)
		mustExec(t, s, `INSERT INTO sessions(connection_id,generation,envelope,key_id,verified_at,updated_at) VALUES(?,?,?,'k1',?,?)`, c.ID, c.Generation, []byte("encrypted-fixture"), now, now)
		mustExec(t, s, `UPDATE recovery_runs SET status='FAILED',error_code='NETWORK_ERROR' WHERE id=?`, run.ID)
		mustExec(t, s, `UPDATE auth_recovery_episodes SET state='MANUAL_REQUIRED',recovery_run_id=?,attempt_count=3,reason_code='CATCHUP_FAILED' WHERE id=?`, run.ID, e.ID)
		if err := h.HandleUpdate(ctx, command("/acb_retry")); err != nil {
			t.Fatal(err)
		}
		data, id := f.lastAction(t)
		if err := h.HandleUpdate(ctx, callback(data, id)); err != nil {
			t.Fatal(err)
		}
		current, err := s.AuthRecoveryEpisode(ctx, e.ID)
		if err != nil || current.State != "CATCHING_UP" || current.AttemptCount != 3 || current.Generation != c.Generation {
			t.Fatal("catchup retry started login/reset budget")
		}
		after, err := s.GetRecoveryRun(ctx, run.ID)
		if err != nil || after.Status != storage.RecoveryRunStatusPending || after.NextDay != "2026-10-02" || after.RangeFrom != "2026-10-01" {
			t.Fatal("catchup range/cursor lost")
		}
		var attempts int
		if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_attempts`).Scan(&attempts); err != nil || attempts != 0 {
			t.Fatal("catchup retry admitted bank login")
		}
		if err := h.HandleUpdate(ctx, callback(data, id)); err != nil {
			t.Fatal(err)
		}
		after, err = s.GetRecoveryRun(ctx, run.ID)
		if err != nil || after.Status != storage.RecoveryRunStatusPending {
			t.Fatal("replay changed catchup disposition")
		}
	})
}
func TestTelegramTransportPreflightAndErrors(t *testing.T) {
	t.Run("webhook conflict never deletes webhook", func(t *testing.T) {
		f := &botFixture{webhook: "https://existing.example/webhook"}
		server := httptest.NewServer(http.HandlerFunc(f.serve))
		defer server.Close()
		c, err := NewClient("123:secret", ClientOptions{BaseURL: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.CheckConfig(context.Background())
		if err == nil || err.Error() != "TELEGRAM_WEBHOOK_CONFLICT" || c.Readiness().Ready {
			t.Fatal("webhook conflict accepted")
		}
		for _, method := range f.requests {
			if method == "deleteWebhook" || method == "getUpdates" {
				t.Fatal("preflight changed webhook/polled")
			}
		}
	})
	for _, code := range []int{429, 401, 403, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := &botFixture{failCode: code, retryAfter: 999}
			server := httptest.NewServer(http.HandlerFunc(f.serve))
			defer server.Close()
			c, err := NewClient("123:secret", ClientOptions{BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.CheckConfig(context.Background())
			var transport *TransportError
			if !errors.As(err, &transport) {
				t.Fatalf("expected safe transport error: %v", err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "raw bank") {
				t.Fatal("HTTP secret/error leaked")
			}
			if code == 429 && transport.RetryAfter != 300*time.Second {
				t.Fatal("retry_after not bounded")
			}
			if (code == 401 || code == 403) && !c.Readiness().Stopped {
				t.Fatal("auth rejection did not stop transport")
			}
			if delay := retryDelay(err, 99); delay > 300*time.Second {
				t.Fatal("retry unbounded")
			}
		})
	}
}
func TestTelegramNoticeBackoffSurvivesRestart(t *testing.T) {
	ctx, s, h, f, path := testTelegram(t)
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation); err != nil {
		t.Fatal(err)
	}
	f.failCode = 429
	f.retryAfter = 120
	if err := h.DeliverNotices(ctx); err == nil {
		t.Fatal("failed notice marked sent")
	}
	notices, err := s.PendingAuthRecoveryNotices(ctx)
	if err != nil || len(notices) != 1 {
		t.Fatal("notice lost")
	}
	when, err := time.Parse(time.RFC3339Nano, notices[0].NextAttemptAt)
	if err != nil || time.Until(when) < 110*time.Second {
		t.Fatal("notice backoff not durable")
	}
	restarted, err := storage.OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	h.Store = restarted
	f.failCode = 0
	before := len(f.requests)
	if err := h.DeliverNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != before {
		t.Fatal("restart skipped persisted notice backoff")
	}
	mustExec(t, restarted, `UPDATE auth_recovery_notices SET next_attempt_at=NULL`)
	if err := h.DeliverNotices(ctx); err != nil {
		t.Fatal(err)
	}
	notices, err = restarted.PendingAuthRecoveryNotices(ctx)
	if err != nil || len(notices) != 0 {
		t.Fatal("successful notice remained pending")
	}
}

func menuLogin(t *testing.T, f *botFixture) (string, int64) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var keyboard inlineKeyboard
	if err := json.Unmarshal(f.messages[len(f.messages)-1]["reply_markup"], &keyboard); err != nil {
		t.Fatal(err)
	}
	for _, row := range keyboard.Rows {
		for _, button := range row {
			if button.Text == "Đăng nhập" {
				return button.Data, f.messageID
			}
		}
	}
	t.Fatal("login control unavailable")
	return "", 0
}

func TestTelegramMenuOneClickConsentAndExpiredRefresh(t *testing.T) {
	ctx, s, h, f, _ := testTelegram(t)
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `INSERT INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES(?,1,X'00','fixture',?)`, c.ID, time.Now().UTC().Format(time.RFC3339Nano))
	if err := h.HandleUpdate(ctx, command("/menu")); err != nil {
		t.Fatal(err)
	}
	old, oldMessage := menuLogin(t, f)
	mustExec(t, s, `UPDATE telegram_auth_actions SET expires_at=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), strings.TrimPrefix(old, "ar:"))
	if err := h.HandleUpdate(ctx, callback(old, oldMessage)); err != nil {
		t.Fatal(err)
	}
	fresh, messageID := menuLogin(t, f)
	if fresh == old || !strings.HasPrefix(fresh, "ar:") {
		t.Fatal("expired button was reused")
	}
	var consentCount int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_recovery_episodes WHERE consent_action_id IS NOT NULL`).Scan(&consentCount); err != nil || consentCount != 0 {
		t.Fatal("expired click granted login consent")
	}
	if err := h.HandleUpdate(ctx, callback(fresh, messageID+1)); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_recovery_episodes WHERE consent_action_id IS NOT NULL`).Scan(&consentCount); err != nil || consentCount != 0 {
		t.Fatal("different message granted consent")
	}
	if err := h.HandleUpdate(ctx, callback(fresh, messageID)); err != nil {
		t.Fatal(err)
	}
	e, err := s.LatestAuthRecoveryEpisode(ctx, c.ID)
	if err != nil || e.ConsentActionID != strings.TrimPrefix(fresh, "ar:") {
		t.Fatal("one authorized menu click did not grant consent")
	}
	_, err = s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.HandleUpdate(ctx, callback(fresh, messageID)); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_attempts`).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatal("replayed click admitted another attempt")
	}
}

func TestTelegramNavigationDuringMutationFence(t *testing.T) {
	ctx, s, h, _, _ := testTelegram(t)
	if _, err := s.AcquireMutationGate(ctx, "fixture", time.Minute, "maintenance"); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"nav:menu", "nav:status", "nav:help", "nav:credentials", "nav:logout"} {
		if err := h.HandleUpdate(ctx, callback(data, 100)); err != nil {
			t.Fatal(err)
		}
	}
	var actions int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM telegram_auth_actions`).Scan(&actions); err != nil || actions != 0 {
		t.Fatal("maintenance navigation created a mutating action")
	}
}

func TestTelegramProgressUsesOneMessageAndCoalescesStaleNotices(t *testing.T) {
	ctx, s, h, f, _ := testTelegram(t)
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE auth_recovery_episodes SET state='STARTING' WHERE id=?`, e.ID)
	if err := h.DeliverNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != 0 {
		t.Fatal("delivery replayed obsolete session-loss notice")
	}
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	progress, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || progress.StatusMessageID != f.messageID {
		t.Fatal("progress message not persisted")
	}
	mustExec(t, s, `UPDATE auth_recovery_episodes SET state='VERIFYING' WHERE id=?`, e.ID)
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.edits) != 1 {
		t.Fatal("changed progress was not edited")
	}
	var editedID int64
	var editedText, initialText string
	if err := json.Unmarshal(f.edits[0]["message_id"], &editedID); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(f.edits[0]["text"], &editedText); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(f.messages[0]["text"], &initialText); err != nil {
		t.Fatal(err)
	}
	if editedID != progress.StatusMessageID || editedText == "" || editedText == initialText {
		t.Fatal("edited progress did not represent current durable state")
	}
	if len(f.messages) != 1 {
		t.Fatal("progress created a second message instead of editing")
	}
	if err := s.SetAuthRecoveryStatusMessage(ctx, e.ID, c.Generation+1, 999); !errors.Is(err, storage.ErrRecoverySuperseded) {
		t.Fatal("stale progress fence was accepted")
	}
	progress, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || progress.StatusMessageID == 999 {
		t.Fatal("stale CAS replaced progress message")
	}
}

func TestTelegramStaleClickAndNavigationKeepOneLiveProgress(t *testing.T) {
	ctx, s, h, f, _ := testTelegram(t)
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `INSERT INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES(?,1,X'00','fixture',?)`, c.ID, time.Now().UTC().Format(time.RFC3339Nano))
	if err := h.menu(ctx, false); err != nil {
		t.Fatal(err)
	}
	login, menuID := menuLogin(t, f)
	if err := h.HandleUpdate(ctx, callback(login, menuID)); err != nil {
		t.Fatal(err)
	}
	e, err := s.LatestAuthRecoveryEpisode(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	before := len(f.messages)
	if err := h.HandleUpdate(ctx, callback(login, menuID)); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != before {
		t.Fatal("stale click sent a second startup panel that cannot follow the attempt")
	}
	var feedback string
	if err := json.Unmarshal(f.callbackAnswers[len(f.callbackAnswers)-1]["text"], &feedback); err != nil || feedback == "" {
		t.Fatal("stale click omitted callback feedback", err)
	}
	mustExec(t, s, `UPDATE auth_recovery_episodes SET state='WAITING_CAPTCHA',ai_used=1 WHERE id=?`, e.ID)
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	for _, u := range []Update{command("/menu"), command("/acb_status"), callback("nav:menu", e.StatusMessageID), callback("nav:status", e.StatusMessageID)} {
		if err := h.HandleUpdate(ctx, u); err != nil {
			t.Fatal(err)
		}
		if len(f.messages) != before {
			t.Fatal("active navigation copied the current stage into an untracked message")
		}
	}
	// Expired controls need fresh nonces even when the displayed state is unchanged.
	var expiredKeyboard inlineKeyboard
	if err := json.Unmarshal(f.edits[len(f.edits)-1]["reply_markup"], &expiredKeyboard); err != nil {
		t.Fatal(err)
	}
	expired := expiredKeyboard.Rows[0][0].Data
	mustExec(t, s, `UPDATE telegram_auth_actions SET expires_at=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), strings.TrimPrefix(expired, "ar:"))
	if err := h.HandleUpdate(ctx, callback(expired, e.StatusMessageID)); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != before {
		t.Fatal("expired control sent an untracked progress copy")
	}
	latest, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || latest.StatusMessageID != e.StatusMessageID || latest.AttemptCount != 1 {
		t.Fatal("navigation changed the progress owner or admitted another attempt", err)
	}
	var id int64
	var keyboard inlineKeyboard
	edit := f.edits[len(f.edits)-1]
	if err := json.Unmarshal(edit["message_id"], &id); err != nil || id != e.StatusMessageID {
		t.Fatal("latest progress was not edited on its canonical message", err)
	}
	if err := json.Unmarshal(edit["reply_markup"], &keyboard); err != nil {
		t.Fatal(err)
	}
	for _, row := range keyboard.Rows {
		for _, button := range row {
			if strings.HasPrefix(button.Data, "ar:") {
				if _, disposition, err := s.ConsumeTelegramAuthAction(ctx, strings.TrimPrefix(button.Data, "ar:"), 42, h.ChatID, h.UserID, id, time.Now()); err != nil || disposition != "CANCEL" {
					t.Fatal("refreshed canonical control was not usable on its own message", err)
				}
				return
			}
		}
	}
	t.Fatal("canonical progress omitted a usable cancel control")
}

func TestTelegramStoppedProgressKeepsSafeReasonAndUsableControls(t *testing.T) {
	ctx, s, h, f, _ := testTelegram(t)
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `INSERT INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES(?,1,X'00','fixture',?)`, c.ID, time.Now().UTC().Format(time.RFC3339Nano))
	if err := h.menu(ctx, false); err != nil {
		t.Fatal(err)
	}
	data, messageID := menuLogin(t, f)
	if err := h.HandleUpdate(ctx, callback(data, messageID)); err != nil {
		t.Fatal(err)
	}
	e, err := s.LatestAuthRecoveryEpisode(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE auth_recovery_episodes SET state='LOGIN' WHERE id=?`, e.ID)
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	progress, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.ReplyBroker = &replyRecorder{}
	reply := command("AB12CD")
	reply.Message.ReplyTo = &Message{ID: 77}
	beforeReplyMessages := len(f.messages)
	if err := h.HandleUpdate(ctx, reply); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != beforeReplyMessages {
		t.Fatal("accepted reply created a stale progress copy above the canonical terminal result")
	}
	var beforeActions int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM telegram_auth_actions`).Scan(&beforeActions); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE auth_attempts SET created_at=? WHERE id=?`, time.Now().Add(-30*time.Second).UTC().Format(time.RFC3339Nano), a.ID)
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	var afterActions int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM telegram_auth_actions`).Scan(&afterActions); err != nil || afterActions != beforeActions {
		t.Fatal("elapsed-time refresh created new actions")
	}
	if err := s.FinishRecoveryAuthAttempt(ctx, e.ID, a.Generation, "FAILED", "MANUAL_REQUIRED", "FRAME_UNSUPPORTED", time.Time{}); err != nil {
		t.Fatal(err)
	}
	beforeMessages := len(f.messages)
	if err := h.DeliverNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != beforeMessages {
		t.Fatal("terminal event duplicated the canonical progress panel")
	}
	edited := f.edits[len(f.edits)-1]
	var id int64
	var text string
	var keyboard inlineKeyboard
	if err := json.Unmarshal(edited["message_id"], &id); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(edited["text"], &text); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(edited["reply_markup"], &keyboard); err != nil {
		t.Fatal(err)
	}
	if id != progress.StatusMessageID || !strings.Contains(text, reasonLabel("FRAME_UNSUPPORTED")) || strings.Contains(text, e.ID) || strings.Contains(text, c.ID) || strings.Contains(text, a.ID) {
		t.Fatal("stopped progress lost safe actionable reason or exposed internal IDs")
	}
	c, err = s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	login := ""
	for _, row := range keyboard.Rows {
		for _, button := range row {
			if !strings.HasPrefix(button.Data, "ar:") {
				continue
			}
			var operation string
			if err := s.DB().QueryRowContext(ctx, `SELECT action FROM telegram_auth_actions WHERE id=? AND message_id=? AND expected_generation=?`, strings.TrimPrefix(button.Data, "ar:"), id, c.Generation).Scan(&operation); err != nil {
				t.Fatal("progress control was not bound to displayed message/generation", err)
			}
			if operation == "LOGIN" {
				login = button.Data
			}
			if operation == "CANCEL" {
				t.Fatal("stopped attempt retained irrelevant cancellation")
			}
		}
	}
	if login == "" {
		t.Fatal("stopped attempt has no usable login next step")
	}
	beforeEdits := len(f.edits)
	if err := h.deliverProgress(ctx); err != nil || len(f.edits) != beforeEdits {
		t.Fatal("unchanged terminal progress looped edits", err)
	}
	u := callback(login, id)
	u.Callback.Message.EditDate = 1
	if err := h.HandleUpdate(ctx, u); err != nil {
		t.Fatal(err)
	}
	current, err := s.LatestAuthRecoveryEpisode(ctx, c.ID)
	if err != nil || current.ConsentActionID != strings.TrimPrefix(login, "ar:") {
		t.Fatal("own edited progress message rejected the next login click", err)
	}
}

func TestTelegramConfirmationNavigationRequiresExactPrivateOperator(t *testing.T) {
	ctx, s, h, _, _ := testTelegram(t)
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `INSERT INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES(?,1,X'00','fixture',?)`, c.ID, time.Now().UTC().Format(time.RFC3339Nano))
	for _, mutate := range []func(*CallbackQuery){
		func(q *CallbackQuery) { q.From.ID++ },
		func(q *CallbackQuery) { q.Message.Chat.ID++ },
		func(q *CallbackQuery) { q.Message.Chat.Type = "group" },
		func(q *CallbackQuery) { q.InlineMessageID = "inline" },
		func(q *CallbackQuery) { q.Message.EditDate = 1; q.Message.From.ID++ },
		func(q *CallbackQuery) { q.Message.ForwardOrigin = json.RawMessage(`{}`) },
	} {
		u := callback("nav:credentials", 100)
		mutate(u.Callback)
		if err := h.HandleUpdate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	var actions int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM telegram_auth_actions`).Scan(&actions); err != nil || actions != 0 {
		t.Fatal("untrusted callback created confirmation authority")
	}
	for _, data := range []string{"nav:credentials", "nav:logout"} {
		if err := h.HandleUpdate(ctx, callback(data, 100)); err != nil {
			t.Fatal(err)
		}
	}
	var updateActions, logoutActions, consents, grants, jobs int
	for _, q := range []struct {
		query       string
		destination *int
	}{
		{`SELECT count(*) FROM telegram_auth_actions WHERE action='UPDATE_CREDENTIALS' AND status='PENDING' AND message_id IS NOT NULL`, &updateActions},
		{`SELECT count(*) FROM telegram_auth_actions WHERE action='LOGOUT' AND status='PENDING' AND message_id IS NOT NULL`, &logoutActions},
		{`SELECT count(*) FROM auth_recovery_episodes WHERE consent_action_id IS NOT NULL`, &consents},
		{`SELECT count(*) FROM acb_credential_grants`, &grants},
		{`SELECT count(*) FROM acb_logout_jobs`, &jobs},
	} {
		if err := s.DB().QueryRowContext(ctx, q.query).Scan(q.destination); err != nil {
			t.Fatal(err)
		}
	}
	if updateActions != 1 || logoutActions != 1 || consents != 0 || grants != 0 || jobs != 0 {
		t.Fatal("navigation performed an operation instead of rendering confirmation")
	}
}

func TestFreshLoginConsentStartsNewProgressMessage(t *testing.T) {
	ctx, s, h, f, _ := testTelegram(t)
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `INSERT INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES(?,1,X'00','fixture',?)`, c.ID, time.Now().UTC().Format(time.RFC3339Nano))
	var previousMessage int64
	for range 2 {
		if err := h.menu(ctx, false); err != nil {
			t.Fatal(err)
		}
		data, id := menuLogin(t, f)
		beforeMessages := len(f.messages)
		if err := h.HandleUpdate(ctx, callback(data, id)); err != nil {
			t.Fatal(err)
		}
		e, err := s.LatestAuthRecoveryEpisode(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.messages) != beforeMessages+1 || e.StatusMessageID <= previousMessage {
			t.Fatal("fresh consent silently edited a previous attempt instead of sending its own progress")
		}
		a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRecoveryAuthAttempt(ctx, e.ID, a.Generation, "FAILED", "MANUAL_REQUIRED", "UNSUPPORTED_CHALLENGE", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := h.deliverProgress(ctx); err != nil {
			t.Fatal(err)
		}
		var editedID int64
		if err := json.Unmarshal(f.edits[len(f.edits)-1]["message_id"], &editedID); err != nil || editedID != e.StatusMessageID {
			t.Fatal("terminal result updated another attempt's progress", err)
		}
		previousMessage = e.StatusMessageID
	}
}

type replyFlowBrowser struct {
	observation authbrowser.AuthObservation
	observeErr  error
	submits     int
	onSubmit    func(storage.AuthChallenge)
	submitErr   error
}

func (b *replyFlowBrowser) Observe(context.Context, string) (authbrowser.AuthObservation, error) {
	return b.observation, b.observeErr
}
func (b *replyFlowBrowser) CaptureCaptcha(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("unexpected captcha capture in OTP flow")
}
func (b *replyFlowBrowser) SubmitChallenge(_ context.Context, ch storage.AuthChallenge, _ string) (authbrowser.AuthObservation, error) {
	b.submits++
	if b.onSubmit != nil {
		b.onSubmit(ch)
	}
	return authbrowser.AuthObservation{State: authbrowser.Unknown}, b.submitErr
}

func telegramOTPFlow(t *testing.T) (context.Context, *storage.Store, *Handler, *botFixture, storage.AuthRecoveryEpisode, storage.AuthChallenge, *replyFlowBrowser) {
	t.Helper()
	ctx, s, h, f, _ := testTelegram(t)
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `INSERT INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES(?,1,X'00','fixture',?)`, c.ID, time.Now().UTC().Format(time.RFC3339Nano))
	if err := h.menu(ctx, false); err != nil {
		t.Fatal(err)
	}
	data, id := menuLogin(t, f)
	if err := h.HandleUpdate(ctx, callback(data, id)); err != nil {
		t.Fatal(err)
	}
	e, err := s.LatestAuthRecoveryEpisode(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE auth_recovery_episodes SET state='WAITING_OTP',reason_code='OTP_REQUEST_SENT' WHERE id=?`, e.ID)
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.CreateAuthChallenge(ctx, storage.AuthChallenge{EpisodeID: e.ID, ConnectionID: e.ConnectionID, Generation: e.Generation, AttemptID: e.AttemptID, BrowserRevision: "otp-revision", Kind: "OTP", ChatID: h.ChatID, ExpiresAt: time.Now().Add(45 * time.Second).UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	id, err = h.Client.SendChallenge(ctx, ch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverAuthChallenge(ctx, ch.ID, h.ChatID, id); err != nil {
		t.Fatal(err)
	}
	ch, err = s.AuthChallengeForPrompt(ctx, h.ChatID, id)
	if err != nil {
		t.Fatal(err)
	}
	b := &replyFlowBrowser{observation: authbrowser.AuthObservation{State: authbrowser.OTPRequired, Revision: ch.BrowserRevision, OTPLength: 6}}
	h.ReplyBroker = &challenge.Broker{Store: s, Browser: b, Sender: h.Client, Submitter: b, Config: challenge.Config{ChatID: h.ChatID}}
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx, s, h, f, e, ch, b
}

func TestTelegramAcceptedOTPUsesDurablePendingAndCanonicalTerminal(t *testing.T) {
	ctx, s, h, f, e, ch, b := telegramOTPFlow(t)
	waiting := h.progressText
	id := h.progressEpisode
	var receiptID int64
	b.onSubmit = func(consuming storage.AuthChallenge) {
		f.mu.Lock()
		receiptID = f.messageID
		f.mu.Unlock()
		if consuming.Status != "CONSUMING" {
			t.Fatal("bank submit did not receive a durable reservation")
		}
		if err := h.deliverProgress(ctx); err != nil {
			t.Fatal(err)
		}
		if h.progressText == waiting {
			t.Fatal("CONSUMING retained the wait-for-code stage")
		}
	}
	reply := command("001234")
	reply.Message.ReplyTo = &Message{ID: ch.PromptMessageID}
	if err := h.HandleUpdate(ctx, reply); err != nil {
		t.Fatal(err)
	}
	consumed, err := s.AuthChallengeForPrompt(ctx, h.ChatID, ch.PromptMessageID)
	if err != nil || consumed.Status != "CONSUMED" || consumed.PromptDeletedAt == "" {
		t.Fatal("accepted OTP did not preserve durable consumption and prompt cleanup", err)
	}
	current, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || current.OTPSubmissions != 1 || h.progressText == waiting || b.submits != 1 {
		t.Fatal("accepted OTP lost consumption or reverted to waiting", err)
	}
	f.mu.Lock()
	receiptDeleted := receiptID > 0 && receiptID != current.StatusMessageID && slices.Contains(f.deletedIDs, receiptID)
	canonicalDeleted := slices.Contains(f.deletedIDs, current.StatusMessageID)
	f.mu.Unlock()
	if !receiptDeleted || canonicalDeleted {
		t.Fatal("accepted OTP retained its receipt or deleted the canonical panel")
	}
	pending := h.progressText
	before := len(f.messages)
	if err := h.HandleUpdate(ctx, command("/acb_login")); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != before || h.progressText != pending {
		t.Fatal("consumed OTP offered another login/code action instead of pending progress")
	}
	mustExec(t, s, `UPDATE auth_recovery_episodes SET state='VERIFYING' WHERE id=?`, e.ID)
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	if h.progressText == pending || h.progressText == waiting || h.progressEpisode != id {
		t.Fatal("verification did not advance the canonical pending stage")
	}
	if err := s.FinishRecoveryAuthAttempt(ctx, e.ID, e.Generation, "FAILED", "MANUAL_REQUIRED", "VERIFICATION_ACCOUNT_MISMATCH", time.Time{}); err != nil {
		t.Fatal(err)
	}
	beforeTerminal := len(f.messages)
	if err := h.DeliverNotices(ctx); err != nil {
		t.Fatal(err)
	}
	terminalID := current.StatusMessageID
	if len(f.messages) != beforeTerminal {
		t.Fatal("terminal result duplicated the panel after the accepted-OTP receipt")
	}
	var resultText string
	var resultKeyboard inlineKeyboard
	if err := json.Unmarshal(f.edits[len(f.edits)-1]["text"], &resultText); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(f.edits[len(f.edits)-1]["reply_markup"], &resultKeyboard); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resultText, "001234") || strings.Contains(resultText, e.ID) {
		t.Fatal("new result exposed OTP or internal identity")
	}
	terminalConnection, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	boundLogin := false
	for _, row := range resultKeyboard.Rows {
		for _, button := range row {
			if !strings.HasPrefix(button.Data, "ar:") {
				continue
			}
			var operation string
			if err := s.DB().QueryRowContext(ctx, `SELECT action FROM telegram_auth_actions WHERE id=? AND message_id=? AND expected_generation=?`, strings.TrimPrefix(button.Data, "ar:"), terminalID, terminalConnection.Generation).Scan(&operation); err != nil {
				t.Fatal("new terminal control not bound to result message", err)
			}
			boundLogin = boundLogin || operation == "LOGIN"
		}
	}
	if !boundLogin {
		t.Fatal("terminal failure omitted actionable next-attempt control")
	}
	var sentID int64
	if err := s.DB().QueryRowContext(ctx, `SELECT message_id FROM auth_recovery_notices WHERE episode_id=? AND kind='MANUAL_REQUIRED' AND status='SENT'`, e.ID).Scan(&sentID); err != nil || sentID != terminalID {
		t.Fatal("terminal ack did not persist canonical message ID", err)
	}
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.DeliverNotices(ctx); err != nil || len(f.messages) != beforeTerminal {
		t.Fatal("next delivery tick replayed SENT terminal", err)
	}
	restarted, err := NewHandler(h.HandlerOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.DeliverNotices(ctx); err != nil || len(f.messages) != beforeTerminal {
		t.Fatal("restart replayed SENT terminal", err)
	}
	if err := restarted.deliverProgress(ctx); err != nil || len(f.messages) != beforeTerminal {
		t.Fatal("restart progress duplicated the canonical terminal", err)
	}
	terminal := h.progressText
	before = len(f.messages)
	if err := h.HandleUpdate(ctx, reply); err != nil {
		t.Fatal(err)
	}
	if b.submits != 1 || h.progressText != terminal || len(f.messages) != before+1 {
		t.Fatal("replay changed the bank submission or terminal stage")
	}
	var explanation string
	if err := json.Unmarshal(f.messages[len(f.messages)-1]["text"], &explanation); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(explanation, terminal) || strings.Contains(explanation, waiting) || strings.Contains(explanation, "001234") || len(f.messages[len(f.messages)-1]["reply_markup"]) > 0 {
		t.Fatal("replay explanation created a newer copied stage, menu or code echo")
	}
	latest, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || latest.StatusMessageID != current.StatusMessageID || latest.OTPSubmissions != 1 {
		t.Fatal("replay replaced durable canonical progress or OTP fence", err)
	}
}

func TestTelegramRejectedOTPRepliesDoNotCopyActiveStage(t *testing.T) {
	for _, outcome := range []string{"expired", "revision changed", "observe outcome unknown", "submit outcome unknown", "invalid format"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, s, h, f, e, ch, b := telegramOTPFlow(t)
			waiting := h.progressText
			reply := command("001234")
			reply.Message.ReplyTo = &Message{ID: ch.PromptMessageID}
			wantStatus, wantSubmits, wantMessages := "INVALIDATED", 0, 1
			switch outcome {
			case "expired":
				mustExec(t, s, `UPDATE auth_challenges SET expires_at=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), ch.ID)
				wantStatus = "EXPIRED"
			case "revision changed":
				b.observation.Revision = "new-revision"
			case "observe outcome unknown":
				b.observeErr = errors.New("observation unavailable")
			case "submit outcome unknown":
				b.submitErr = errors.New("response unavailable")
				wantSubmits, wantMessages = 1, 2 // Broker receipt, then finite disposition.
			case "invalid format":
				reply.Message.Text = "123"
				wantStatus = "PENDING"
			}
			before := len(f.messages)
			if err := h.HandleUpdate(ctx, reply); err != nil {
				t.Fatal(err)
			}
			current, err := s.AuthChallengeForPrompt(ctx, h.ChatID, ch.PromptMessageID)
			if err != nil || current.Status != wantStatus || b.submits != wantSubmits || len(f.messages) != before+wantMessages {
				t.Fatal("reply disposition did not preserve challenge/submission boundaries", err)
			}
			if outcome == "invalid format" {
				if h.progressText != waiting || current.PromptDeletedAt != "" {
					t.Fatal("format correction invalidated an active OTP prompt")
				}
			} else if h.progressText == waiting {
				t.Fatal("disposed OTP still instructed operator to answer the inactive prompt")
			}
			for _, message := range f.messages[before:] {
				var text string
				if err := json.Unmarshal(message["text"], &text); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(text, waiting) || strings.Contains(text, "001234") || len(message["reply_markup"]) > 0 {
					t.Fatal("reply created a copied stage, new menu or code echo")
				}
			}
			latest, err := s.AuthRecoveryEpisode(ctx, e.ID)
			if err != nil || latest.OTPSubmissions != wantSubmits {
				t.Fatal("reply changed OTP reservation without a bank submit", err)
			}
		})
	}
}

func TestTelegramCatchupFailureAdvancesCanonicalBeforeEpisodePoll(t *testing.T) {
	ctx, s, h, f, _ := testTelegram(t)
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.EnsureRecoveryRunWithPlan(ctx, c.ID, c.Generation, "telegram-catchup", storage.RecoveryRunPlan{RangeFrom: "2026-10-01", RangeTo: "2026-10-02", NextDay: "2026-10-01"})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE connections SET state='MONITORING' WHERE id=?`, c.ID)
	mustExec(t, s, `UPDATE auth_recovery_episodes SET state='CATCHING_UP',recovery_run_id=? WHERE id=?`, run.ID, e.ID)
	if _, err := s.ClaimRecoveryRun(ctx, run.ID, c.ID, c.Generation); err != nil {
		t.Fatal(err)
	}
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	running := h.progressText
	before := len(f.messages)
	if _, err := s.UpdateRecoveryRunProgress(ctx, run.ID, c.ID, c.Generation, storage.RecoveryRunStatusFailed, "{}", "UPSTREAM_UNAVAILABLE", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	if h.progressText == running || len(f.messages) != before {
		t.Fatal("durable failed run kept the running snapshot or created another message")
	}
	current, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || current.State != "CATCHING_UP" {
		t.Fatal("fixture did not retain the pre-poll episode boundary", err)
	}
	var editedID int64
	if err := json.Unmarshal(f.edits[len(f.edits)-1]["message_id"], &editedID); err != nil || editedID != current.StatusMessageID {
		t.Fatal("failure did not edit the authoritative progress message", err)
	}
	var keyboard inlineKeyboard
	if err := json.Unmarshal(f.edits[len(f.edits)-1]["reply_markup"], &keyboard); err != nil {
		t.Fatal(err)
	}
	retry := false
	for _, row := range keyboard.Rows {
		for _, button := range row {
			if !strings.HasPrefix(button.Data, "ar:") {
				continue
			}
			var operation string
			if err := s.DB().QueryRowContext(ctx, `SELECT action FROM telegram_auth_actions WHERE id=? AND message_id=?`, strings.TrimPrefix(button.Data, "ar:"), current.StatusMessageID).Scan(&operation); err != nil {
				t.Fatal(err)
			}
			if operation == "LOGIN" {
				t.Fatal("catchup failure offered another bank login")
			}
			retry = retry || operation == "RETRY"
		}
	}
	if !retry {
		t.Fatal("catchup failure lost its session-preserving retry action")
	}
}

func TestTelegramOTPRequestReservationEditsWithoutCreatingPrompt(t *testing.T) {
	ctx, s, h, f, _ := testTelegram(t)
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	mustExec(t, s, `UPDATE auth_recovery_episodes SET state='LOGIN',last_login_at=?,consent_consumed_at=? WHERE id=?`, stamp, stamp, e.ID)
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	login := h.progressText
	mustExec(t, s, `UPDATE auth_recovery_episodes SET reason_code='OTP_REQUEST_SENT' WHERE id=?`, e.ID)
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	request := h.progressText
	if request == login || len(f.messages) != 1 || len(f.edits) != 1 {
		t.Fatal("same-state OTP request reservation did not advance canonical progress")
	}
	mustExec(t, s, `UPDATE auth_recovery_episodes SET state='WAIT_OPERATOR',reason_code='OTP_REQUEST_OUTCOME_UNKNOWN' WHERE id=?`, e.ID)
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	if h.progressText == request || len(f.messages) != 1 || len(f.edits) != 2 {
		t.Fatal("lost OTP request outcome did not replace the canonical pending stage")
	}
	var prompts, attempts int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_challenges`).Scan(&prompts); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if prompts != 0 || attempts != 0 {
		t.Fatal("progress delivery requested a code or began bank authentication")
	}
}
