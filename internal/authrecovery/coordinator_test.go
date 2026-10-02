package authrecovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/challenge"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telegramauth"
)

type coordinatorFinalizer struct{ calls int }

func (f *coordinatorFinalizer) Complete(context.Context, storage.AuthAttempt) (storage.Connection, error) {
	f.calls++
	return storage.Connection{}, errors.New("synthetic verifier unavailable")
}

type coordinatorSolver struct {
	calls int
	fail  bool
}

func (f *coordinatorSolver) Solve(context.Context, []byte) (string, error) {
	f.calls++
	if f.fail {
		return "", errors.New("MODEL_SECRET_Ab12CD")
	}
	return "Ab12CD", nil
}

type coordinatorFixture struct {
	t                                                *testing.T
	ctx                                              context.Context
	path                                             string
	store                                            *storage.Store
	browser                                          *authbrowser.Client
	bot                                              *telegramauth.Client
	broker                                           *challenge.Broker
	finalizer                                        *coordinatorFinalizer
	solver                                           *coordinatorSolver
	controller                                       *Coordinator
	mu                                               sync.Mutex
	observation                                      authbrowser.AuthObservation
	starts, logins, captchas, otps, cancels, prompts int
	startFail, submitFail, aiReject                  bool
	workerFail                                       bool
	replyBodies                                      []string
	now                                              time.Time
}

func newCoordinatorFixture(t *testing.T) *coordinatorFixture {
	t.Helper()
	f := &coordinatorFixture{t: t, ctx: context.Background(), path: filepath.Join(t.TempDir(), "recovery.db"), now: time.Now(), finalizer: &coordinatorFinalizer{}, solver: &coordinatorSolver{}}
	s, err := storage.Open(f.ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.store = s
	t.Cleanup(func() { f.store.Close() })
	conn, err := s.ConfigureConnection(f.ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.DB().ExecContext(f.ctx, `INSERT INTO auth_attempts(id,connection_id,generation,owner_subject,status,expires_at,created_at,finished_at) VALUES('prior',?,?,'operator','VERIFIED',?,?,?)`, conn.ID, conn.Generation, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	f.observation = authbrowser.AuthObservation{State: authbrowser.LoginForm, Revision: "login-1", ExpiresAt: time.Now().Add(15 * time.Minute)}
	var crop bytes.Buffer
	if err := png.Encode(&crop, image.NewRGBA(image.Rect(0, 0, 12, 8))); err != nil {
		t.Fatal(err)
	}
	browserServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/sessions":
			f.starts++
			if f.startFail {
				http.Error(w, "HTTP_SECRET_password", 503)
				return
			}
			var input struct {
				AttemptID string `json:"attemptId"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				http.Error(w, "invalid fixture request", 400)
				return
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(authbrowser.Session{AttemptID: input.AttemptID, Status: "RUNNING"})
		case r.Method == http.MethodDelete:
			f.cancels++
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/status"):
			json.NewEncoder(w).Encode(authbrowser.Session{AttemptID: strings.Split(r.URL.Path, "/")[2], Status: "RUNNING"})
		case strings.HasSuffix(r.URL.Path, "/observation"):
			json.NewEncoder(w).Encode(f.observation)
		case strings.HasSuffix(r.URL.Path, "/captcha") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "image/png")
			w.Write(crop.Bytes())
		case strings.HasSuffix(r.URL.Path, "/login"):
			f.logins++
			if f.submitFail {
				http.Error(w, "BROWSER_SECRET_001234", 503)
				return
			}
			f.observation.State = authbrowser.OTPRequired
			f.observation.Revision = "otp-1"
			json.NewEncoder(w).Encode(f.observation)
		case strings.HasSuffix(r.URL.Path, "/captcha"):
			f.captchas++
			if f.aiReject {
				f.observation.State = authbrowser.CaptchaRequired
				f.observation.Revision = fmt.Sprintf("captcha-next-%d", f.captchas)
			} else {
				f.observation.State = authbrowser.OTPRequired
				f.observation.Revision = "otp-1"
			}
			json.NewEncoder(w).Encode(f.observation)
		case strings.HasSuffix(r.URL.Path, "/otp"):
			f.otps++
			f.observation.State = authbrowser.Authenticated
			f.observation.Revision = "authenticated"
			json.NewEncoder(w).Encode(f.observation)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(browserServer.Close)
	f.browser = authbrowser.NewClient(browserServer.URL)
	botServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true}}`)
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			io.WriteString(w, `{"ok":true,"result":{"url":""}}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage") || strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			f.prompts++
			f.replyBodies = append(f.replyBodies, string(data))
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]int{"message_id": 100 + f.prompts}})
		default:
			io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(botServer.Close)
	f.bot, err = telegramauth.NewClient("123:synthetic", telegramauth.ClientOptions{BaseURL: botServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.bot.CheckConfig(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.TelegramAuthState(f.ctx, 1); err != nil {
		t.Fatal(err)
	}
	f.restart(false)
	return f
}
func (f *coordinatorFixture) restart(ai bool) {
	f.t.Helper()
	f.broker = &challenge.Broker{Store: f.store, Browser: f.browser, Sender: f.bot, Config: challenge.Config{ChatID: 22, CaptchaTTL: 180 * time.Second, OTPTTL: 120 * time.Second}}
	var err error
	f.controller, err = NewCoordinator(CoordinatorOptions{Config: Config{Enabled: true, AICaptchaEnabled: ai}, Store: f.store, Browser: f.browser, Broker: f.broker, Finalizer: f.finalizer, Telegram: f.bot, Solver: f.solver, Now: func() time.Time { return f.now }, Jitter: func() float64 { return 0 }, Credentials: func() (Credentials, error) {
		return Credentials{Username: "user-secret", Password: "password-secret", AccountNumber: "account-secret"}, nil
	}, WorkerReady: func(context.Context) error {
		if f.workerFail {
			return errors.New("worker unavailable")
		}
		return nil
	}})
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *coordinatorFixture) reconcile() {
	f.t.Helper()
	if err := f.controller.ReconcileOnce(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Second)
}
func (f *coordinatorFixture) episode() storage.AuthRecoveryEpisode {
	f.t.Helper()
	conn, err := f.store.Connection(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := f.store.LatestAuthRecoveryEpisode(f.ctx, conn.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}
func (f *coordinatorFixture) pending() storage.AuthChallenge {
	f.t.Helper()
	ch, err := f.store.ActiveAuthChallenge(f.ctx, f.episode().AttemptID)
	if err != nil {
		f.t.Fatal(err)
	}
	return ch
}

func TestRecoveryCoordinatorPendingAccountSelection(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.reconcile()
	e := f.episode()
	if err := f.controller.advance(f.ctx, e, authbrowser.AuthObservation{State: authbrowser.Unknown, ReasonCode: "ACCOUNT_SELECTION_PENDING"}); err != nil {
		t.Fatal(err)
	}
	if got := f.episode(); got.State != e.State || got.AttemptID != e.AttemptID || got.AttemptCount != 1 {
		t.Fatal("pending exact account selection terminated or restarted authentication")
	}
	if err := f.controller.advance(f.ctx, f.episode(), authbrowser.AuthObservation{State: authbrowser.Authenticated}); err != nil {
		t.Fatal(err)
	}
	if f.episode().State != "VERIFYING" || f.finalizer.calls != 1 || f.logins != 1 {
		t.Fatal("account selection did not resume verification in the same attempt")
	}
}

func TestRecoveryCoordinatorRestartWaitingOTP(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.reconcile()
	ch := f.pending()
	e := f.episode()
	if e.State != "WAITING_OTP" || f.starts != 1 || f.logins != 1 {
		t.Fatalf("did not reach OTP: state=%s starts=%d logins=%d", e.State, f.starts, f.logins)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := storage.Open(f.ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.store = s
	f.restart(false)
	f.reconcile()
	if got := f.pending(); got.ID != ch.ID || got.PromptMessageID != ch.PromptMessageID || f.prompts != 1 {
		t.Fatal("restart replaced valid prompt")
	}
	if err := f.broker.HandleReply(f.ctx, 22, ch.PromptMessageID, 900, "001234"); err != nil {
		t.Fatal(err)
	}
	if err := f.broker.HandleReply(f.ctx, 22, ch.PromptMessageID, 901, "001234"); !errors.Is(err, storage.ErrChallengeConsumed) {
		t.Fatalf("duplicate response accepted: %v", err)
	}
	if f.otps != 1 || f.logins != 1 || f.episode().State != "VERIFYING" {
		t.Fatal("OTP replay or new login after accepted OTP")
	}
	f.reconcile()
	if f.finalizer.calls != 1 {
		t.Fatal("verification retried before five seconds")
	}
	f.now = f.now.Add(5 * time.Second)
	f.reconcile()
	if f.finalizer.calls != 2 {
		t.Fatal("verification unavailable did not retry")
	}
}
func TestRecoveryCoordinatorConsumingCrashNeverReplays(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "authenticated"}[advanced], func(t *testing.T) {
			f := newCoordinatorFixture(t)
			f.reconcile()
			ch := f.pending()
			if _, err := f.store.ConsumeAuthChallenge(f.ctx, ch.ID, ch.Generation, ch.BrowserRevision, 22, ch.PromptMessageID, time.Now()); err != nil {
				t.Fatal(err)
			}
			if advanced {
				f.observation.State = authbrowser.Authenticated
				f.observation.Revision = "authenticated"
			}
			f.restart(false)
			f.reconcile()
			expected := "WAIT_OPERATOR"
			if advanced {
				expected = "VERIFYING"
			}
			if f.episode().State != expected || f.otps != 0 || f.starts != 1 {
				t.Fatalf("crash outcome state=%s submits=%d starts=%d", f.episode().State, f.otps, f.starts)
			}
			if err := f.broker.HandleReply(f.ctx, 22, ch.PromptMessageID, 901, "001234"); err == nil {
				t.Fatal("late consumed reply accepted")
			}
		})
	}
}
func TestRecoveryCoordinatorStaleFences(t *testing.T) {
	for _, change := range []string{"revision", "generation", "config"} {
		t.Run(change, func(t *testing.T) {
			f := newCoordinatorFixture(t)
			f.reconcile()
			ch := f.pending()
			switch change {
			case "revision":
				f.observation.Revision = "new-otp"
			case "generation":
				_, err := f.store.DB().ExecContext(f.ctx, `UPDATE connections SET generation=generation+1,state='MONITORING'`)
				if err != nil {
					t.Fatal(err)
				}
			case "config":
				_, err := f.store.DB().ExecContext(f.ctx, `UPDATE connections SET config_revision=config_revision+1,state='MONITORING'`)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := f.broker.HandleReply(f.ctx, 22, ch.PromptMessageID, 999, "001234"); err == nil {
				t.Fatal("stale reply accepted")
			}
			if f.otps != 0 {
				t.Fatal("stale reply reached browser")
			}
			f.reconcile()
			if change != "revision" && f.episode().State != "SUPERSEDED" {
				t.Fatal("external actor not superseded")
			}
		})
	}
}
func TestRecoveryCoordinatorAdmissionFences(t *testing.T) {
	for _, fence := range []string{"manual", "worker", "bot", "deploy", "paused", "onboarding"} {
		t.Run(fence, func(t *testing.T) {
			f := newCoordinatorFixture(t)
			switch fence {
			case "manual":
				if _, err := f.store.StartAuthAttempt(f.ctx, "operator", time.Minute); err != nil {
					t.Fatal(err)
				}
			case "worker":
				f.workerFail = true
			case "bot":
				bot, err := telegramauth.NewClient("123:not-ready", telegramauth.ClientOptions{})
				if err != nil {
					t.Fatal(err)
				}
				f.controller.Telegram = bot
			case "deploy":
				if _, err := f.store.AcquireMutationGate(f.ctx, "deploy", time.Minute, "fixture"); err != nil {
					t.Fatal(err)
				}
			case "paused":
				conn, err := f.store.Connection(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.ApplyTelegramAuthOperation(f.ctx, 1, "", conn.Generation, "PAUSE"); err != nil {
					t.Fatal(err)
				}
			case "onboarding":
				if _, err := f.store.DB().ExecContext(f.ctx, `DELETE FROM auth_attempts WHERE id='prior'`); err != nil {
					t.Fatal(err)
				}
			}
			_ = f.controller.ReconcileOnce(f.ctx)
			if f.starts != 0 || f.logins != 0 {
				t.Fatal("admission fence crossed")
			}
		})
	}
}
func TestRecoveryCoordinatorFreshAttemptAfterSubmittedLogin(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.reconcile()
	first := f.episode()
	if err := f.controller.retry(f.ctx, first, "BROWSER_UNAVAILABLE"); err != nil {
		t.Fatal(err)
	}
	// Advance durable wall-clock gates without sleeping; previous submission remains
	// part of the episode's cooldown, not a submission in the new browser attempt.
	past := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_recovery_episodes SET next_attempt_at=?,last_login_at=? WHERE id=?`, past, past, first.ID); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.observation.State = authbrowser.LoginForm
	f.observation.Revision = "fresh-login"
	f.mu.Unlock()
	f.restart(false)
	f.reconcile()
	if got := f.episode(); got.State != "WAITING_OTP" || got.AttemptCount != 2 || got.AttemptID == first.AttemptID || f.logins != 2 {
		t.Fatal("previous episode login timestamp prevented fresh-attempt submission")
	}
	f.mu.Lock()
	f.observation.State = authbrowser.LoginForm
	f.observation.Revision = "same-attempt-login-return"
	f.mu.Unlock()
	f.reconcile()
	if f.logins != 2 || f.episode().State != "WAIT_OPERATOR" {
		t.Fatal("same attempt replayed credentials after unknown login outcome")
	}
}

func TestRecoveryCoordinatorBudgetSurvivesRestart(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.startFail = true
	for i := 1; i <= 3; i++ {
		f.reconcile()
		e := f.episode()
		if e.AttemptCount != i || f.starts != i {
			t.Fatalf("attempt budget lost: attempts=%d starts=%d", e.AttemptCount, f.starts)
		}
		if e.Generation <= e.TriggerGeneration {
			t.Fatal("failed attempt did not advance generation")
		}
		if i < 3 {
			if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_recovery_episodes SET next_attempt_at=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), e.ID); err != nil {
				t.Fatal(err)
			}
			f.restart(false)
		}
	}
	f.restart(false)
	f.reconcile()
	if f.starts != 3 || f.episode().State != "MANUAL_REQUIRED" {
		t.Fatal("restart bypassed exhausted budget")
	}
}
func TestRecoveryCoordinatorAIFallbackAndCaptchaCap(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.observation.State = authbrowser.CaptchaRequired
	f.observation.Revision = "captcha-1"
	f.aiReject = true
	f.restart(true)
	f.reconcile()
	if f.solver.calls != 1 || f.captchas != 1 || f.pending().BrowserRevision != "captcha-next-1" {
		t.Fatal("AI rejection did not fall back within same attempt")
	}
	for i := range 2 {
		ch := f.pending()
		if err := f.broker.HandleReply(f.ctx, 22, ch.PromptMessageID, int64(900+i), "Ab12CD"); err != nil {
			t.Fatal(err)
		}
	}
	if f.solver.calls != 1 || f.starts != 1 || f.captchas != 3 || f.episode().State != "WAIT_OPERATOR" {
		t.Fatal("CAPTCHA cap or one-AI budget bypassed")
	}
}
func TestRecoveryCoordinatorInitialCaptchaBeforeLogin(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.observation.CaptchaRequired = true
	f.reconcile()
	ch := f.pending()
	if f.logins != 0 {
		t.Fatal("credentials submitted before initial CAPTCHA")
	}
	if err := f.broker.HandleReply(f.ctx, 22, ch.PromptMessageID, 900, "Ab12CD"); err != nil {
		t.Fatal(err)
	}
	if f.logins != 1 || f.captchas != 0 || f.episode().CaptchaSubmissions != 1 || f.pending().Kind != "OTP" {
		t.Fatal("initial CAPTCHA not submitted with credentials exactly once")
	}
}
func TestRecoverySecretRedaction(t *testing.T) {
	for _, fault := range []string{"http", "browser", "model"} {
		t.Run(fault, func(t *testing.T) {
			f := newCoordinatorFixture(t)
			var logs bytes.Buffer
			oldOutput := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(oldOutput)
			switch fault {
			case "http":
				f.startFail = true
			case "browser":
				f.submitFail = true
			case "model":
				f.observation.State = authbrowser.CaptchaRequired
				f.solver.fail = true
				f.restart(true)
			}
			err := f.controller.ReconcileOnce(f.ctx)
			output := ""
			if err != nil {
				output = err.Error()
			}
			for _, table := range []string{"auth_recovery_episodes", "auth_challenges", "auth_recovery_notices", "auth_attempts"} {
				rows, err := f.store.DB().QueryContext(f.ctx, `SELECT * FROM `+table)
				if err != nil {
					t.Fatal(err)
				}
				cols, err := rows.Columns()
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					values := make([]any, len(cols))
					dest := make([]any, len(cols))
					for i := range values {
						dest[i] = &values[i]
					}
					if err := rows.Scan(dest...); err != nil {
						t.Fatal(err)
					}
					for _, v := range values {
						switch v := v.(type) {
						case string:
							output += v
						case []byte:
							output += string(v)
						}
					}
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				rows.Close()
			}
			output += strings.Join(f.replyBodies, "\n") + logs.String()
			for _, secret := range []string{"user-secret", "password-secret", "account-secret", "HTTP_SECRET_password", "BROWSER_SECRET_001234", "MODEL_SECRET_Ab12CD", "123:synthetic"} {
				if strings.Contains(output, secret) {
					t.Fatalf("secret escaped through recovery metadata: fault=%s", fault)
				}
			}
		})
	}
}

func TestRecoveryCoordinatorDeliveringAndRevisionReplacement(t *testing.T) {
	for _, boundary := range []string{"DELIVERING", "REVISION"} {
		t.Run(boundary, func(t *testing.T) {
			f := newCoordinatorFixture(t)
			f.reconcile()
			old := f.pending()
			if boundary == "DELIVERING" {
				if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_challenges SET status='DELIVERING' WHERE id=?`, old.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				f.mu.Lock()
				f.observation.Revision = "new-otp"
				f.mu.Unlock()
			}
			f.restart(false)
			f.reconcile()
			replacement := f.pending()
			if replacement.ID == old.ID || replacement.PromptMessageID == old.PromptMessageID || f.logins != 1 || f.starts != 1 {
				t.Fatal("uncertain prompt was reused or login repeated")
			}
			if err := f.broker.HandleReply(f.ctx, 22, old.PromptMessageID, 900, "001234"); err == nil {
				t.Fatal("old prompt accepted")
			}
			if err := f.broker.HandleReply(f.ctx, 22, replacement.PromptMessageID, 901, "001234"); err != nil {
				t.Fatal(err)
			}
			if f.otps != 1 {
				t.Fatal("replacement did not accept exactly one response")
			}
		})
	}
}
func TestRecoveryCoordinatorConfirmedOnboardingOnly(t *testing.T) {
	f := newCoordinatorFixture(t)
	if _, err := f.store.DB().ExecContext(f.ctx, `DELETE FROM auth_attempts WHERE id='prior'`); err != nil {
		t.Fatal(err)
	}
	conn, err := f.store.Connection(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	e, err := f.store.EnsureAuthRecoveryEpisode(f.ctx, conn.ID, conn.Generation)
	if err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if f.starts != 0 {
		t.Fatal("unconfirmed onboarding started")
	}
	if err := f.store.RearmAuthRecovery(f.ctx, e.ID, e.Generation); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if f.starts != 1 || f.episode().State != "WAITING_OTP" {
		t.Fatal("confirmed onboarding did not proceed")
	}
}
func TestRecoveryCoordinatorCancelledAndShutdownStayDurable(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.reconcile()
	e := f.episode()
	ch := f.pending()
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if err := f.controller.ReconcileOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown result: %v", err)
	}
	unchanged := f.episode()
	if unchanged.Generation != e.Generation || unchanged.State != e.State || f.cancels != 0 {
		t.Fatal("shutdown finished live attempt")
	}
	if err := f.store.FinishRecoveryAuthAttempt(f.ctx, e.ID, e.Generation, "CANCELLED", "CANCELLED", "OPERATOR_CANCELLED", time.Time{}); err != nil {
		t.Fatal(err)
	}
	f.restart(false)
	f.reconcile()
	if f.starts != 1 || f.episode().State != "CANCELLED" {
		t.Fatal("cancelled episode auto-recreated")
	}
	if err := f.broker.HandleReply(f.ctx, 22, ch.PromptMessageID, 900, "001234"); err == nil || f.otps != 0 {
		t.Fatal("cancelled reply reached browser")
	}
}
func TestRecoveryCoordinatorUnknownAndOTPExpiryStop(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		f := newCoordinatorFixture(t)
		f.observation.State = authbrowser.Unknown
		for range 10 {
			f.reconcile()
		}
		if f.episode().State != "MANUAL_REQUIRED" || f.starts != 1 || f.logins != 0 {
			t.Fatal("unknown page loop exceeded cap")
		}
	})
	t.Run("otp-expiry", func(t *testing.T) {
		f := newCoordinatorFixture(t)
		f.reconcile()
		ch := f.pending()
		if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_challenges SET expires_at=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), ch.ID); err != nil {
			t.Fatal(err)
		}
		f.restart(false)
		f.reconcile()
		if f.episode().State != "WAIT_OPERATOR" || f.logins != 1 || f.otps != 0 {
			t.Fatal("expired OTP restarted login")
		}
	})
}
func TestRecoveryCoordinatorVerifierFailurePreservesOldSession(t *testing.T) {
	f := newCoordinatorFixture(t)
	conn, err := f.store.Connection(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	previous := []byte("previous-encrypted-envelope")
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := f.store.DB().ExecContext(f.ctx, `INSERT INTO sessions(connection_id,generation,envelope,key_id,verified_at,updated_at) VALUES(?,?,?,'k1',?,?)`, conn.ID, conn.Generation, previous, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	ch := f.pending()
	if err := f.broker.HandleReply(f.ctx, 22, ch.PromptMessageID, 900, "001234"); err != nil {
		t.Fatal(err)
	}
	var saved []byte
	if err := f.store.DB().QueryRowContext(f.ctx, `SELECT envelope FROM sessions WHERE connection_id=?`, conn.ID).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, previous) {
		t.Fatal("unverified attempt replaced prior session")
	}
	var runs int
	if err := f.store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM recovery_runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || f.episode().State != "VERIFYING" {
		t.Fatal("unverified recovery committed success")
	}
}

func TestRecoveryCoordinatorConcurrentConsumptionIsNotCrash(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.reconcile()
	ch := f.pending()
	consumed, err := f.store.ConsumeAuthChallenge(f.ctx, ch.ID, ch.Generation, ch.BrowserRevision, 22, ch.PromptMessageID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Deterministically exercise the schedule between Broker's atomic consume
	// and its call to the coordinator while reconcile owns the operation lock.
	f.reconcile()
	if f.episode().State != "WAITING_OTP" {
		t.Fatal("live response was mistaken for a crashed response")
	}
	if _, err := f.controller.SubmitChallenge(f.ctx, consumed, "001234"); err != nil {
		t.Fatal(err)
	}
	if f.otps != 1 || f.episode().State != "VERIFYING" {
		t.Fatal("live response did not submit exactly once")
	}
}
func TestRecoveryCoordinatorDisabledAndDatabaseFailClosed(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		c, err := NewCoordinator(CoordinatorOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := c.ReconcileOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := c.SubmitChallenge(context.Background(), storage.AuthChallenge{}, "001234"); !errors.Is(err, ErrRecoveryDisabled) {
			t.Fatal("disabled coordinator accepted a response")
		}
	})
	t.Run("database", func(t *testing.T) {
		f := newCoordinatorFixture(t)
		if err := f.store.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.controller.ReconcileOnce(f.ctx); err == nil {
			t.Fatal("database failure ignored")
		}
		if f.starts != 0 || f.logins != 0 || f.otps != 0 || f.prompts != 0 {
			t.Fatal("side effect performed without database fence")
		}
	})
}

func TestRecoveryCoordinatorMaintenanceAndCatchupGate(t *testing.T) {
	t.Run("maintenance", func(t *testing.T) {
		f := newCoordinatorFixture(t)
		f.observation.State = authbrowser.Maintenance
		f.reconcile()
		e := f.episode()
		deadline, err := time.Parse(time.RFC3339Nano, e.NextAttemptAt)
		if err != nil {
			t.Fatal(err)
		}
		if e.State != "MAINTENANCE_WAIT" || deadline.Sub(f.now.Add(-2*time.Second)) != 15*time.Minute || f.logins != 0 {
			t.Fatal("maintenance did not defer login fifteen minutes")
		}
		f.restart(false)
		f.reconcile()
		if f.starts != 1 {
			t.Fatal("maintenance deadline lost across restart")
		}
	})
	for _, reason := range []string{"HISTORY_RANGE_UNAVAILABLE", "INVALID_CHECKPOINT"} {
		t.Run("catchup-failed-"+reason, func(t *testing.T) {
			f := newCoordinatorFixture(t)
			f.reconcile()
			e := f.episode()
			stamp := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_attempts SET status='VERIFIED',finished_at=? WHERE id=?`, stamp, e.AttemptID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE connections SET state='MONITORING'`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.DB().ExecContext(f.ctx, `INSERT INTO sessions(connection_id,generation,envelope,key_id,verified_at,updated_at) VALUES(?,?,?,'k1',?,?)`, e.ConnectionID, e.Generation, []byte("verified-envelope"), stamp, stamp); err != nil {
				t.Fatal(err)
			}
			day := time.Now().In(time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)).Format("2006-01-02")
			run, _, err := f.store.EnsureRecoveryRunWithPlan(f.ctx, e.ConnectionID, e.Generation, e.AttemptID, storage.RecoveryRunPlan{Reason: storage.RecoveryReasonReauth, RangeFrom: day, RangeTo: day, NextDay: day})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_recovery_episodes SET state='CATCHING_UP',recovery_run_id=? WHERE id=?`, run.ID, e.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.ClaimRecoveryRun(f.ctx, run.ID, e.ConnectionID, e.Generation); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.UpdateRecoveryRunProgress(f.ctx, run.ID, e.ConnectionID, e.Generation, storage.RecoveryRunStatusFailed, "{}", reason, ""); err != nil {
				t.Fatal(err)
			}
			f.restart(false)
			f.reconcile()
			e = f.episode()
			blocked, err := f.store.HasBlockingAuthRecovery(f.ctx, e.ConnectionID, e.Generation)
			if err != nil {
				t.Fatal(err)
			}
			expected := "CATCHUP_FAILED"
			if reason == "INVALID_CHECKPOINT" {
				expected = reason
			}
			if e.State != "MANUAL_REQUIRED" || e.ReasonCode != expected || !blocked || f.starts != 1 || f.logins != 1 || f.otps != 0 {
				t.Fatal("catchup failure removed gate, changed circuit reason or restarted login")
			}
			if reason == "INVALID_CHECKPOINT" {
				if err := f.store.RetryAuthRecoveryCatchup(f.ctx, e.ID, e.Generation); !errors.Is(err, storage.ErrRecoveryCommitted) {
					t.Fatalf("invalid checkpoint retry permitted: %v", err)
				}
				f.restart(false)
				f.reconcile()
				if f.episode().ReasonCode != reason {
					t.Fatal("invalid checkpoint circuit reason lost on restart")
				}
			}
		})
	}
}
