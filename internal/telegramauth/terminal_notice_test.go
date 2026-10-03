package telegramauth

import (
	"strings"
	"testing"
	"time"
)

func TestTerminalNoticeIndependentOfCanonicalEdit(t *testing.T) {
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
		t.Fatal("uneditable canonical blocked fresh terminal result")
	}
	terminalID := f.messageID
	var sentID int64
	if err := s.DB().QueryRowContext(ctx, `SELECT message_id FROM auth_recovery_notices WHERE episode_id=? AND kind='MANUAL_REQUIRED' AND status='SENT'`, e.ID).Scan(&sentID); err != nil || sentID != terminalID {
		t.Fatal("terminal result not acknowledged", err)
	}
	current, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || current.StatusMessageID != e.StatusMessageID {
		t.Fatal("terminal replaced canonical identity", err)
	}
}

func TestTerminalNoticeRetryAndAtLeastOnceAckBoundary(t *testing.T) {
	for _, mode := range []string{"rate_limit", "transport", "accepted_before_ack", "bind_fence"} {
		t.Run(mode, func(t *testing.T) {
			ctx, s, h, f, e, _, _ := telegramOTPFlow(t)
			if err := s.FinishRecoveryAuthAttempt(ctx, e.ID, e.Generation, "FAILED", "MANUAL_REQUIRED", "VERIFICATION_ACCOUNT_MISMATCH", time.Time{}); err != nil {
				t.Fatal(err)
			}
			before := f.messageID
			switch mode {
			case "rate_limit":
				f.failCode, f.retryAfter = 429, 120
			case "transport":
				f.failCode = 503
			case "accepted_before_ack":
				mustExec(t, s, `CREATE TRIGGER fail_terminal_ack BEFORE UPDATE OF status ON auth_recovery_notices WHEN NEW.kind='MANUAL_REQUIRED' AND NEW.status='SENT' BEGIN SELECT RAISE(ABORT,'fixture ack unavailable'); END`)
			case "bind_fence":
				f.afterSend = func() { mustExec(t, s, `UPDATE connections SET generation=generation+1 WHERE id=?`, e.ConnectionID) }
			}
			if err := h.DeliverNotices(ctx); err == nil {
				t.Fatal("failed terminal delivery/ack hidden")
			}
			var status, next string
			if err := s.DB().QueryRowContext(ctx, `SELECT status,COALESCE(next_attempt_at,'') FROM auth_recovery_notices WHERE episode_id=? AND kind='MANUAL_REQUIRED'`, e.ID).Scan(&status, &next); err != nil || status != "PENDING" {
				t.Fatal("failed terminal marked SENT", err)
			}
			f.failCode, f.afterSend = 0, nil
			switch mode {
			case "rate_limit", "transport":
				retryAt, err := time.Parse(time.RFC3339Nano, next)
				if err != nil || !retryAt.After(time.Now()) {
					t.Fatal("retry deadline not durable", err)
				}
				if mode == "rate_limit" && time.Until(retryAt) < 110*time.Second {
					t.Fatal("rate limit ignored")
				}
				restarted, err := NewHandler(h.HandlerOptions)
				if err != nil {
					t.Fatal(err)
				}
				if err := restarted.DeliverNotices(ctx); err != nil || f.messageID != before {
					t.Fatal("restart bypassed transport backoff", err)
				}
				mustExec(t, s, `UPDATE auth_recovery_notices SET next_attempt_at=? WHERE episode_id=? AND kind='MANUAL_REQUIRED'`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), e.ID)
				if err := restarted.DeliverNotices(ctx); err != nil || f.messageID != before+1 {
					t.Fatal("eligible terminal retry not delivered", err)
				}
			case "accepted_before_ack":
				if f.messageID != before+1 {
					t.Fatal("fixture did not accept before failed ack")
				}
				mustExec(t, s, `DROP TRIGGER fail_terminal_ack`)
				restarted, err := NewHandler(h.HandlerOptions)
				if err != nil {
					t.Fatal(err)
				}
				if err := restarted.DeliverNotices(ctx); err != nil || f.messageID != before+2 {
					t.Fatal("pending accepted-before-ack result was lost", err)
				}
			case "bind_fence":
				deleted := false
				for _, request := range f.requests {
					deleted = deleted || request == "deleteMessage"
				}
				if !deleted {
					t.Fatal("unbound stale result was not deleted best-effort")
				}
				if err := h.DeliverNotices(ctx); err != nil || f.messageID != before+1 {
					t.Fatal("old generation notice replayed", err)
				}
			}
			serialized := ""
			for _, message := range f.messages {
				serialized += string(message["text"])
			}
			if strings.Contains(serialized, "001234") {
				t.Fatal("terminal delivery echoed OTP")
			}
		})
	}
}
