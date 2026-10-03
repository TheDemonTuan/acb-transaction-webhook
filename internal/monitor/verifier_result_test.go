package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authsession"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type verificationLogCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *verificationLogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *verificationLogCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "ACB session verification result" {
		h.mu.Lock()
		h.records = append(h.records, r.Clone())
		h.mu.Unlock()
	}
	return nil
}
func (h *verificationLogCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *verificationLogCapture) WithGroup(string) slog.Handler      { return h }

func captureVerificationLogs(t *testing.T) *verificationLogCapture {
	t.Helper()
	h := &verificationLogCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return h
}

func (h *verificationLogCapture) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = nil
}

func (h *verificationLogCapture) assertResult(t *testing.T, generation int64, phase, code string, response, formValid, accountPresent, accountMatch bool) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) != 1 {
		t.Fatalf("verification must emit one result per call, got %d", len(h.records))
	}
	attrs := make(map[string]slog.Value)
	h.records[0].Attrs(func(a slog.Attr) bool {
		if _, duplicate := attrs[a.Key]; duplicate {
			t.Fatalf("duplicate result field %q", a.Key)
		}
		attrs[a.Key] = a.Value
		return true
	})
	wantCount := 3
	if response {
		wantCount = 8
	}
	if len(attrs) != wantCount || attrs["generation"].Int64() != generation || attrs["phase"].String() != phase || attrs["code"].String() != code {
		t.Fatalf("unsafe or incorrect finite result fields: %v", attrs)
	}
	if response {
		if attrs["status"].Int64() != http.StatusOK || attrs["form_valid"].Bool() != formValid || attrs["account_present"].Bool() != accountPresent || attrs["account_match"].Bool() != accountMatch {
			t.Fatalf("incorrect response diagnostic: %v", attrs)
		}
		switch fmt.Sprint(attrs["kind"].Any()) {
		case string(acb.AccountDetailPage), string(acb.HistoryPage), string(acb.LoginPage), string(acb.OTPChallenge), string(acb.CaptchaPage), string(acb.MaintenancePage), string(acb.UnknownPage):
		default:
			t.Fatalf("non-enum response kind: %v", attrs["kind"])
		}
	}
	for key, value := range attrs {
		switch key {
		case "generation", "phase", "code", "status", "kind", "form_valid", "account_present", "account_match":
		default:
			t.Fatalf("unsafe result field %q", key)
		}
		for _, secret := range []string{"222222222", "111111111", "***2222", "synthetic-session", "synthetic-cookie", "synthetic-private-body", "synthetic-secret-error", "private.example"} {
			if strings.Contains(fmt.Sprint(value.Any()), secret) {
				t.Fatalf("result exposed private fixture data in %q", key)
			}
		}
	}
}

func TestVerificationFailureMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"generation", fmt.Errorf("private generation: %w", storage.ErrGenerationFenceMismatch), "VERIFICATION_SUPERSEDED"},
		{"recovery", storage.ErrRecoverySuperseded, "VERIFICATION_SUPERSEDED"},
		{"auth", fmt.Errorf("private auth: %w", &acb.AuthFailure{Kind: acb.LoginPage, Reason: "synthetic-secret-error"}), "VERIFICATION_AUTH_REQUIRED"},
		{"canceled", context.Canceled, "VERIFICATION_UNAVAILABLE"},
		{"deadline", context.DeadlineExceeded, "VERIFICATION_UNAVAILABLE"},
		{"unknown", errors.New("synthetic-secret-error"), "VERIFICATION_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verificationFailure(tc.err)
			if authsession.VerificationCode(err) != tc.code || err.Error() != tc.code {
				t.Fatalf("unsafe classification: %v", err)
			}
		})
	}
}

func TestVerifyTaskFiniteOutcomes(t *testing.T) {
	for _, tc := range []struct {
		code    string
		outcome scheduler.StepOutcome
	}{
		{"VERIFICATION_AUTH_REQUIRED", scheduler.OutcomeAuth},
		{"VERIFICATION_ACCOUNT_MISMATCH", scheduler.OutcomeFatal},
		{"VERIFICATION_ACCOUNT_MISSING", scheduler.OutcomeFatal},
		{"VERIFICATION_FORM_INVALID", scheduler.OutcomeFatal},
		{"VERIFICATION_PAGE_UNSUPPORTED", scheduler.OutcomeFatal},
		{"VERIFICATION_MAINTENANCE", scheduler.OutcomeTransient},
		{"VERIFICATION_UNAVAILABLE", scheduler.OutcomeTransient},
		{"VERIFICATION_TIMEOUT", scheduler.OutcomeTransient},
		{"VERIFICATION_SUPERSEDED", scheduler.ClassifyOutcome(storage.ErrGenerationFenceMismatch, nil)},
		{"", scheduler.OutcomeSuccess},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var expected error
			if tc.code != "" {
				expected = &authsession.VerificationError{Code: tc.code}
			}
			task := &verifyTask{stepFn: func(context.Context) error { return expected }, done: make(chan error, 1)}
			result, err := task.Step(context.Background())
			if !result.Done || result.Outcome != tc.outcome || err != expected || result.Error != expected || <-task.done != expected {
				t.Fatalf("incorrect terminal task outcome: %+v, %v", result, err)
			}
		})
	}
}

func TestSessionVerifierFenceBeforeAndAfterBootstrap(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		t.Run(fmt.Sprint(inFlight), func(t *testing.T) {
			logs := captureVerificationLogs(t)
			store, conn, keyring, encoded := sessionFenceFixture(t)
			calls := 0
			client, err := acb.NewClient("https://online.acb.com.vn", verifierAccountTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					installLogoutFence(t, store, conn)
				}
				body := `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="acctDetailPage"><input name="dse_sessionId" value="fresh-session"><input name="AccountNbr" value="222222222"></form>`
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if !inFlight {
				installLogoutFence(t, store, conn)
			}
			verifier := NewSessionVerifier(NewSessionLoader(store, keyring, client), client)
			err = verifier.VerifySession(context.Background(), conn.ID, conn.Generation, encoded)
			if authsession.VerificationCode(err) != "VERIFICATION_SUPERSEDED" {
				t.Fatalf("stale result was accepted: %v", err)
			}
			phase := "RESTORE"
			if inFlight {
				phase = "BOOTSTRAP"
			}
			if !inFlight && calls != 0 || inFlight && calls == 0 {
				t.Fatalf("incorrect upstream activity for in-flight=%t: calls=%d", inFlight, calls)
			}
			logs.assertResult(t, conn.Generation, phase, "VERIFICATION_SUPERSEDED", false, false, false, false)
			var sessions int
			if err := store.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE connection_id=?`, conn.ID).Scan(&sessions); err != nil || sessions != 0 {
				t.Fatalf("stale verification persisted a session: count=%d err=%v", sessions, err)
			}
		})
	}
}

func TestSessionVerifierUnavailableEmitsSafeResult(t *testing.T) {
	logs := captureVerificationLogs(t)
	var verifier *SessionVerifier
	err := verifier.VerifySession(context.Background(), "synthetic-private-account", 1, []byte("synthetic-secret-error"))
	if authsession.VerificationCode(err) != "VERIFICATION_UNAVAILABLE" {
		t.Fatalf("unavailable verifier error=%v", err)
	}
	logs.assertResult(t, 1, "RESTORE", "VERIFICATION_UNAVAILABLE", false, false, false, false)
}

func TestSessionVerifierPausedQueueEmitsSafeResult(t *testing.T) {
	logs := captureVerificationLogs(t)
	store, conn, keyring, encoded := sessionFenceFixture(t)
	client, err := acb.NewClient("https://online.acb.com.vn", verifierAccountTransport(func(*http.Request) (*http.Response, error) {
		t.Error("paused queue must not issue an upstream request")
		return nil, errors.New("synthetic-secret-error")
	}))
	if err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New(nil)
	sched.Start(context.Background())
	defer sched.Stop()
	sched.Pause()
	verifier := NewSessionVerifier(NewSessionLoader(store, keyring, client), client, sched)
	err = verifier.VerifySession(context.Background(), conn.ID, conn.Generation, encoded)
	if authsession.VerificationCode(err) != "VERIFICATION_UNAVAILABLE" {
		t.Fatalf("queue admission error=%v", err)
	}
	logs.assertResult(t, conn.Generation, "RESTORE", "VERIFICATION_UNAVAILABLE", false, false, false, false)
}
