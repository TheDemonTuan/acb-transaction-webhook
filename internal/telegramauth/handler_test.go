package telegramauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type botFixture struct {
	mu         sync.Mutex
	requests   []string
	messages   []map[string]json.RawMessage
	updates    []Update
	messageID  int64
	webhook    string
	failCode   int
	retryAfter int
}

func (f *botFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.requests = append(f.requests, method)
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
	case "sendMessage":
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		f.messages = append(f.messages, body)
		f.messageID++
		result = map[string]int64{"message_id": f.messageID}
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

func TestTelegramUnconfiguredConfirmationDoesNotBlockPolling(t *testing.T) {
	ctx, store, h, f, _ := testTelegram(t)
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='UNCONFIGURED',account_masked=''`); err != nil {
		t.Fatal(err)
	}
	if err := h.HandleUpdate(ctx, command("/acb_login")); err != nil {
		t.Fatal(err)
	}
	data, messageID := f.lastAction(t)
	confirmation := callback(data, messageID)
	confirmation.ID = 21
	pause := command("/acb_pause")
	pause.ID = 22
	f.updates = []Update{confirmation, pause}
	if err := h.Client.pollOnce(ctx, store, h); err != nil {
		t.Fatal(err)
	}
	state, err := store.TelegramAuthState(ctx, 42)
	if err != nil || !state.Paused || state.NextUpdateID != 23 {
		t.Fatal("rejected onboarding confirmation blocked subsequent operator commands")
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

type cancelRecorder struct{ attempts []string }

func (r *cancelRecorder) Cancel(_ context.Context, id string) error {
	r.attempts = append(r.attempts, id)
	return nil
}

type scheduleRecorder struct{ keys []string }

func (r *scheduleRecorder) ScheduleRecovery(_ context.Context, _ string, _ int64, key string) error {
	r.keys = append(r.keys, key)
	return nil
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
	t.Run("confirmation TTL and wrong message", func(t *testing.T) {
		ctx, s, h, f, _ := testTelegram(t)
		if err := h.HandleUpdate(ctx, command("/acb_manual")); err != nil {
			t.Fatal(err)
		}
		data, id := f.lastAction(t)
		if err := h.HandleUpdate(ctx, callback(data, id+1)); err != nil {
			t.Fatal(err)
		}
		state, _ := s.TelegramAuthState(ctx, 42)
		if state.Paused {
			t.Fatal("wrong originating message authorized action")
		}
		mustExec(t, s, `UPDATE telegram_auth_actions SET expires_at=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), strings.TrimPrefix(data, "ar:"))
		if err := h.HandleUpdate(ctx, callback(data, id)); err != nil {
			t.Fatal(err)
		}
		state, _ = s.TelegramAuthState(ctx, 42)
		if state.Paused {
			t.Fatal("expired confirmation authorized action")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		answers := 0
		for _, method := range f.requests {
			if method == "answerCallbackQuery" {
				answers++
			}
		}
		if answers != 2 {
			t.Fatal("stale callback spinner unanswered")
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
		a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		canceller := &cancelRecorder{}
		h.Browser = canceller
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
		if len(canceller.attempts) != 1 || canceller.attempts[0] != a.ID {
			t.Fatal("wrong browser attempt cancelled")
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
		if len(canceller.attempts) != 1 {
			t.Fatal("consumed callback cancelled bank twice")
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
		scheduler := &scheduleRecorder{}
		h.Scheduler = scheduler
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
		if len(scheduler.keys) != 1 || scheduler.keys[0] != run.EventKey {
			t.Fatal("wrong immutable run rescheduled")
		}
		if err := h.HandleUpdate(ctx, callback(data, id)); err != nil {
			t.Fatal(err)
		}
		if len(scheduler.keys) != 1 {
			t.Fatal("replayed callback rescheduled twice")
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
