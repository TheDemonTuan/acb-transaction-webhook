package telegramauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// The fixture supplies an authoritative coordinator transition; delivery still
// uses the real Telegram Client over the local HTTP server in testTelegram.
func terminalNoticeFixture(t *testing.T, ctx context.Context, s *storage.Store, e storage.AuthRecoveryEpisode, kind string) {
	t.Helper()
	var finishedAt any
	if kind == "COMPLETED" || kind == "CANCELLED" || kind == "SUPERSEDED" {
		finishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	mustExec(t, s, `UPDATE auth_recovery_episodes SET state=?,finished_at=? WHERE id=?`, kind, finishedAt, e.ID)
	if kind == "COMPLETED" {
		mustExec(t, s, `UPDATE connections SET state='MONITORING' WHERE id=?`, e.ConnectionID)
		mustExec(t, s, `UPDATE auth_attempts SET status='VERIFIED' WHERE id=?`, e.AttemptID)
	}
	mustExec(t, s, `INSERT INTO auth_recovery_notices(id,episode_id,event_key,kind,status,created_at) VALUES(?,?,?,?,'PENDING',?)`, "terminal-"+kind, e.ID, fmt.Sprintf("%s:%s:%d", e.ID, kind, e.AttemptCount), kind, time.Now().UTC().Format(time.RFC3339Nano))
}

func assertCanonicalTerminal(t *testing.T, ctx context.Context, s *storage.Store, e storage.AuthRecoveryEpisode, kind string, id int64) {
	t.Helper()
	current, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || id <= 0 || current.StatusMessageID != id {
		t.Fatal("terminal panel is not the canonical status message", err)
	}
	var sentID int64
	if err := s.DB().QueryRowContext(ctx, `SELECT message_id FROM auth_recovery_notices WHERE episode_id=? AND kind=? AND status='SENT'`, e.ID, kind).Scan(&sentID); err != nil || sentID != id {
		t.Fatal("notice did not acknowledge the canonical panel", err)
	}
}

func assertTerminalControls(t *testing.T, ctx context.Context, s *storage.Store, body map[string]json.RawMessage, id int64) {
	t.Helper()
	var keyboard inlineKeyboard
	if err := json.Unmarshal(body["reply_markup"], &keyboard); err != nil {
		t.Fatal(err)
	}
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bound := 0
	for _, row := range keyboard.Rows {
		for _, button := range row {
			if !strings.HasPrefix(button.Data, "ar:") {
				continue
			}
			var matches bool
			if err := s.DB().QueryRowContext(ctx, `SELECT message_id=? AND expected_generation=? AND status='PENDING' FROM telegram_auth_actions WHERE id=?`, id, c.Generation, strings.TrimPrefix(button.Data, "ar:")).Scan(&matches); err != nil || !matches {
				t.Fatal("terminal control is not message/generation-bound", err)
			}
			bound++
		}
	}
	if bound == 0 {
		t.Fatal("terminal panel lost its actionable controls")
	}
}

func TestTelegramCompletedNoticeUsesCanonicalPanelHTTP(t *testing.T) {
	ctx, s, h, f, e, ch, b := telegramOTPFlow(t)
	current, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || current.StatusMessageID == 0 {
		t.Fatal("progress must exist before the OTP receipt", err)
	}
	canonicalID := current.StatusMessageID
	reply := command("001234")
	reply.Message.ReplyTo = &Message{ID: ch.PromptMessageID}
	if err := h.HandleUpdate(ctx, reply); err != nil {
		t.Fatal(err)
	}
	if b.submits != 1 || f.messageID <= canonicalID {
		t.Fatal("fixture did not submit OTP and deliver its separate receipt")
	}
	terminalNoticeFixture(t, ctx, s, e, "COMPLETED")
	before := len(f.messages)
	if err := h.DeliverNotices(ctx); err != nil {
		t.Fatal(err)
	}
	assertCanonicalTerminal(t, ctx, s, e, "COMPLETED", canonicalID)
	body := f.edits[len(f.edits)-1]
	var editedID int64
	if err := json.Unmarshal(body["message_id"], &editedID); err != nil || editedID != canonicalID {
		t.Fatal("completion did not edit the pre-OTP canonical panel", err)
	}
	assertTerminalControls(t, ctx, s, body, canonicalID)
	if strings.Contains(string(body["text"]), "001234") {
		t.Fatal("completion echoed OTP")
	}
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewHandler(h.HandlerOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.DeliverNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != before {
		t.Fatal("completion, progress, or restart sent a second terminal panel")
	}
	assertCanonicalTerminal(t, ctx, s, e, "COMPLETED", canonicalID)
}

func TestTerminalNoticesWithoutInitialPanelCreateOneCanonicalMessage(t *testing.T) {
	for _, kind := range []string{"COMPLETED", "WAIT_OPERATOR", "MANUAL_REQUIRED", "CANCELLED", "RETRY_WAIT", "MAINTENANCE_WAIT"} {
		t.Run(kind, func(t *testing.T) {
			ctx, s, h, f, _ := testTelegram(t)
			c, err := s.Connection(ctx)
			if err != nil {
				t.Fatal(err)
			}
			e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
			if err != nil {
				t.Fatal(err)
			}
			terminalNoticeFixture(t, ctx, s, e, kind)
			if err := h.DeliverNotices(ctx); err != nil {
				t.Fatal(err)
			}
			id := f.messageID
			assertCanonicalTerminal(t, ctx, s, e, kind, id)
			if err := h.deliverProgress(ctx); err != nil {
				t.Fatal(err)
			}
			restarted, err := NewHandler(h.HandlerOptions)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.DeliverNotices(ctx); err != nil {
				t.Fatal(err)
			}
			if err := restarted.deliverProgress(ctx); err != nil {
				t.Fatal(err)
			}
			if len(f.messages) != 1 || f.messageID != id {
				t.Fatal("terminal notice without initial progress did not create exactly one panel")
			}
			assertCanonicalTerminal(t, ctx, s, e, kind, id)
		})
	}
}

func TestTerminalNoticeUneditableReplacesCanonicalOnce(t *testing.T) {
	ctx, s, h, f, e, _, _ := telegramOTPFlow(t)
	if err := s.FinishRecoveryAuthAttempt(ctx, e.ID, e.Generation, "FAILED", "MANUAL_REQUIRED", "VERIFICATION_ACCOUNT_MISSING", time.Time{}); err != nil {
		t.Fatal(err)
	}
	before := f.messageID
	f.failEditCode = 400
	if err := h.DeliverNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if f.messageID != before+1 {
		t.Fatal("uneditable canonical panel was not replaced exactly once")
	}
	id := f.messageID
	assertCanonicalTerminal(t, ctx, s, e, "MANUAL_REQUIRED", id)
	assertTerminalControls(t, ctx, s, f.messages[len(f.messages)-1], id)
	f.failEditCode = 0
	if err := h.deliverProgress(ctx); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewHandler(h.HandlerOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.DeliverNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.deliverProgress(ctx); err != nil || f.messageID != id {
		t.Fatal("replacement or restart duplicated the terminal panel", err)
	}
}

func TestTerminalNoticeRetryAndCanonicalAckBoundary(t *testing.T) {
	for _, mode := range []string{"rate_limit", "transport", "transient_edit", "accepted_before_ack"} {
		t.Run(mode, func(t *testing.T) {
			ctx, s, h, f, e, _, _ := telegramOTPFlow(t)
			if err := s.FinishRecoveryAuthAttempt(ctx, e.ID, e.Generation, "FAILED", "MANUAL_REQUIRED", "VERIFICATION_ACCOUNT_MISMATCH", time.Time{}); err != nil {
				t.Fatal(err)
			}
			before := f.messageID
			beforeRequests := len(f.requests)
			switch mode {
			case "rate_limit":
				f.failCode, f.retryAfter = 429, 120
			case "transport":
				f.failCode = 503
			case "transient_edit":
				f.failEditCode = 503
			case "accepted_before_ack":
				mustExec(t, s, `CREATE TRIGGER fail_terminal_ack BEFORE UPDATE OF status ON auth_recovery_notices WHEN NEW.kind='MANUAL_REQUIRED' AND NEW.status='SENT' BEGIN SELECT RAISE(ABORT,'fixture ack unavailable'); END`)
			}
			if err := h.DeliverNotices(ctx); err == nil {
				t.Fatal("failed terminal delivery/ack hidden")
			}
			for _, request := range f.requests[beforeRequests:] {
				if request == "sendMessage" {
					t.Fatal("failed edit tried to send a duplicate terminal")
				}
			}
			var status, next string
			if err := s.DB().QueryRowContext(ctx, `SELECT status,COALESCE(next_attempt_at,'') FROM auth_recovery_notices WHERE episode_id=? AND kind='MANUAL_REQUIRED'`, e.ID).Scan(&status, &next); err != nil || status != "PENDING" {
				t.Fatal("failed terminal was consumed", err)
			}
			f.failCode, f.failEditCode = 0, 0
			restarted, err := NewHandler(h.HandlerOptions)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "accepted_before_ack" {
				mustExec(t, s, `DROP TRIGGER fail_terminal_ack`)
			} else {
				retryAt, err := time.Parse(time.RFC3339Nano, next)
				if err != nil || !retryAt.After(time.Now()) {
					t.Fatal("retry deadline not durable", err)
				}
				if mode == "rate_limit" && time.Until(retryAt) < 110*time.Second {
					t.Fatal("rate limit ignored")
				}
				beforeRequests = len(f.requests)
				if err := restarted.DeliverNotices(ctx); err != nil || len(f.requests) != beforeRequests {
					t.Fatal("restart bypassed persisted transport backoff", err)
				}
				if err := restarted.deliverProgress(ctx); err != nil || len(f.requests) != beforeRequests {
					t.Fatal("background progress bypassed persisted terminal backoff", err)
				}
				mustExec(t, s, `UPDATE auth_recovery_notices SET next_attempt_at=? WHERE episode_id=? AND kind='MANUAL_REQUIRED'`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), e.ID)
			}
			if err := restarted.DeliverNotices(ctx); err != nil || f.messageID != before {
				t.Fatal("pending notice replay created another terminal panel", err)
			}
			current, err := s.AuthRecoveryEpisode(ctx, e.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertCanonicalTerminal(t, ctx, s, e, "MANUAL_REQUIRED", current.StatusMessageID)
		})
	}
}

func TestTerminalNoticeRejectsStaleProgressWithoutDeletingExistingPanel(t *testing.T) {
	for _, fence := range []string{"generation", "config", "attempt", "attempt_count", "state", "canonical_id"} {
		t.Run(fence, func(t *testing.T) {
			ctx, s, h, f, e, _, _ := telegramOTPFlow(t)
			terminalNoticeFixture(t, ctx, s, e, "COMPLETED")
			before, beforeDeletes := f.messageID, len(f.deletedIDs)
			f.afterEdit = func() {
				switch fence {
				case "generation":
					mustExec(t, s, `UPDATE connections SET generation=generation+1 WHERE id=?`, e.ConnectionID)
				case "config":
					mustExec(t, s, `UPDATE auth_recovery_episodes SET config_revision=config_revision+1 WHERE id=?`, e.ID)
				case "attempt":
					mustExec(t, s, `UPDATE auth_recovery_episodes SET attempt_id=NULL WHERE id=?`, e.ID)
				case "attempt_count":
					mustExec(t, s, `UPDATE auth_recovery_episodes SET attempt_count=attempt_count+1 WHERE id=?`, e.ID)
				case "state":
					mustExec(t, s, `UPDATE auth_recovery_episodes SET state='WAIT_OPERATOR',finished_at=NULL WHERE id=?`, e.ID)
				case "canonical_id":
					mustExec(t, s, `UPDATE auth_recovery_episodes SET status_message_id=? WHERE id=?`, before+100, e.ID)
				}
			}
			if err := h.DeliverNotices(ctx); !errors.Is(err, storage.ErrRecoverySuperseded) {
				t.Fatal("stale terminal delivery was acknowledged", err)
			}
			if f.messageID != before || len(f.deletedIDs) != beforeDeletes {
				t.Fatal("stale edit sent another terminal or deleted an unrelated panel")
			}
			var status string
			if err := s.DB().QueryRowContext(ctx, `SELECT status FROM auth_recovery_notices WHERE episode_id=? AND kind='COMPLETED'`, e.ID).Scan(&status); err != nil || status != "PENDING" {
				t.Fatal("stale completion was consumed", err)
			}
		})
	}
}

func TestTerminalNoticeFencesNewPanelAndDeletesOnlyItsOwnSend(t *testing.T) {
	ctx, s, h, f, _ := testTelegram(t)
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
	if err != nil {
		t.Fatal(err)
	}
	terminalNoticeFixture(t, ctx, s, e, "COMPLETED")
	before := f.messageID
	f.afterSend = func() { mustExec(t, s, `UPDATE connections SET generation=generation+1 WHERE id=?`, c.ID) }
	if err := h.DeliverNotices(ctx); !errors.Is(err, storage.ErrRecoverySuperseded) {
		t.Fatal("new stale panel was accepted", err)
	}
	if f.messageID != before+1 || len(f.deletedIDs) != 1 || f.deletedIDs[0] != before+1 {
		t.Fatal("stale new panel cleanup touched another message")
	}
	f.afterSend = nil
	if err := h.DeliverNotices(ctx); err != nil || f.messageID != before+1 {
		t.Fatal("obsolete generation notice was replayed", err)
	}
}

func TestTerminalNoticeFencesBeforeTelegram(t *testing.T) {
	ctx, s, h, f, e, _, _ := telegramOTPFlow(t)
	terminalNoticeFixture(t, ctx, s, e, "COMPLETED")
	before := len(f.requests)
	h.AIDegraded = func() string {
		mustExec(t, s, `UPDATE auth_recovery_episodes SET state='WAIT_OPERATOR',finished_at=NULL WHERE id=?`, e.ID)
		return ""
	}
	if err := h.DeliverNotices(ctx); !errors.Is(err, storage.ErrRecoverySuperseded) {
		t.Fatal("planning used a superseded completion snapshot", err)
	}
	if len(f.requests) != before {
		t.Fatal("stale completion touched Telegram before its state fence")
	}
	var status string
	if err := s.DB().QueryRowContext(ctx, `SELECT status FROM auth_recovery_notices WHERE episode_id=? AND kind='COMPLETED'`, e.ID).Scan(&status); err != nil || status != "PENDING" {
		t.Fatal("stale planning consumed the completion notice", err)
	}
}

func TestCredentialsUpdatedNoticeRemainsSeparateFromCanonicalPanel(t *testing.T) {
	ctx, s, h, f, e, _, _ := telegramOTPFlow(t)
	terminalNoticeFixture(t, ctx, s, e, "COMPLETED")
	if err := h.DeliverNotices(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.messages)
	mustExec(t, s, `INSERT INTO auth_recovery_notices(id,episode_id,event_key,kind,status,created_at) VALUES('credentials-notice',?,'credentials-event','CREDENTIALS_UPDATED','PENDING',?)`, e.ID, time.Now().UTC().Format(time.RFC3339Nano))
	if err := h.DeliverNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != before+1 || f.messageID == current.StatusMessageID {
		t.Fatal("credential update was merged into the recovery panel")
	}
	var sentID int64
	if err := s.DB().QueryRowContext(ctx, `SELECT message_id FROM auth_recovery_notices WHERE id='credentials-notice' AND status='SENT'`).Scan(&sentID); err != nil || sentID != f.messageID {
		t.Fatal("credential notice did not acknowledge its separate message", err)
	}
	if err := h.DeliverNotices(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.deliverProgress(ctx); err != nil || len(f.messages) != before+1 {
		t.Fatal("separate credential notice was replayed", err)
	}
	assertCanonicalTerminal(t, ctx, s, e, "COMPLETED", current.StatusMessageID)
}
