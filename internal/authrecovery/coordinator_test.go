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
	"github.com/thedemontuan/acb-transaction-webhook/internal/authsession"
	"github.com/thedemontuan/acb-transaction-webhook/internal/challenge"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
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

type coordinatorVerifier func(context.Context, string, int64, []byte) error

func (v coordinatorVerifier) VerifySession(ctx context.Context, connectionID string, generation int64, envelope []byte) error {
	return v(ctx, connectionID, generation, envelope)
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
	observes, credentialReads                        int
	startFail, submitFail, aiReject                  bool
	workerFail                                       bool
	replyBodies                                      []string
	now                                              time.Time
	loginReplies                                     []authbrowser.AuthObservation
}

func newUnconsentedCoordinatorFixture(t *testing.T) *coordinatorFixture {
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
	if _, err := s.DB().ExecContext(f.ctx, `INSERT INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES(?,1,?,'fixture',?)`, conn.ID, []byte("synthetic-ciphertext"), stamp); err != nil {
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
		case strings.HasSuffix(r.URL.Path, "/handoff"):
			if f.observation.State != authbrowser.Authenticated {
				http.Error(w, "not authenticated", 409)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"session": "synthetic-authenticated-session"})
		case strings.HasSuffix(r.URL.Path, "/status"):
			json.NewEncoder(w).Encode(authbrowser.Session{AttemptID: strings.Split(r.URL.Path, "/")[2], Status: "RUNNING"})
		case strings.HasSuffix(r.URL.Path, "/observation"):
			f.observes++
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
			if len(f.loginReplies) > 0 {
				f.observation = f.loginReplies[0]
				f.loginReplies = f.loginReplies[1:]
				json.NewEncoder(w).Encode(f.observation)
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
func newCoordinatorFixture(t *testing.T) *coordinatorFixture {
	f := newUnconsentedCoordinatorFixture(t)
	f.login()
	return f
}

func (f *coordinatorFixture) login() storage.TelegramAuthAction {
	f.t.Helper()
	conn, err := f.store.Connection(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := f.store.EnsureAuthRecoveryEpisode(f.ctx, conn.ID, conn.Generation)
	if err != nil {
		f.t.Fatal(err)
	}
	a, err := f.store.CreateTelegramAuthAction(f.ctx, storage.TelegramAuthAction{BotID: 1, ChatID: 22, UserID: 33, EpisodeID: e.ID, ExpectedGeneration: e.Generation, Action: "LOGIN"})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.DeliverTelegramAuthAction(f.ctx, a.ID, 77); err != nil {
		f.t.Fatal(err)
	}
	a, _, err = f.store.ConsumeTelegramAuthAction(f.ctx, a.ID, 1, 22, 33, 77, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}
func (f *coordinatorFixture) restart(ai bool) {
	f.t.Helper()
	f.broker = &challenge.Broker{Store: f.store, Browser: f.browser, Sender: f.bot, Config: challenge.Config{ChatID: 22, CaptchaTTL: 180 * time.Second, OTPTTL: 120 * time.Second}}
	if err := f.broker.RecoverDelivery(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	var err error
	f.controller, err = NewCoordinator(CoordinatorOptions{Config: Config{Enabled: true, AICaptchaEnabled: ai}, Store: f.store, Browser: f.browser, Broker: f.broker, Finalizer: f.finalizer, Telegram: f.bot, Solver: f.solver, Now: func() time.Time { return f.now }, Credentials: func(context.Context, string) (storage.ACBCredentials, error) {
		f.credentialReads++
		return storage.ACBCredentials{Username: "user-secret", Password: "password-secret", AccountNumber: "account-secret", Revision: 1}, nil
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
	if err := f.broker.DeliverPending(f.ctx); err != nil {
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
	if err := f.broker.DeliverPending(f.ctx); err != nil {
		f.t.Fatal(err)
	}
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
	for _, fence := range []string{"manual", "worker", "bot", "deploy", "paused", "no-consent"} {
		t.Run(fence, func(t *testing.T) {
			f := newUnconsentedCoordinatorFixture(t)
			if fence != "no-consent" {
				f.login()
			}
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
			}
			_ = f.controller.ReconcileOnce(f.ctx)
			if f.starts != 0 || f.logins != 0 || f.solver.calls != 0 || f.credentialReads != 0 {
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
	if f.starts != 1 || f.logins != 1 || f.episode().ConsentActionID != "" {
		t.Fatal("failure retained consent or started a browser without a new click")
	}
	f.login()
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

func TestRecoveryCoordinatorFailureRequiresFreshConsentAfterRestart(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.startFail = true
	f.reconcile()
	e := f.episode()
	if e.State != "WAIT_OPERATOR" || e.ConsentActionID != "" || e.ConsentConsumedAt != "" || f.starts != 1 {
		t.Fatal("browser failure did not terminate and clear button consent")
	}
	if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_recovery_episodes SET next_attempt_at=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), e.ID); err != nil {
		t.Fatal(err)
	}
	f.restart(false)
	for range 3 {
		f.reconcile()
	}
	if f.starts != 1 || f.logins != 0 || f.credentialReads != 0 {
		t.Fatal("elapsed backoff or restart authorized another attempt")
	}
	f.login()
	f.reconcile()
	if f.starts != 2 || f.episode().State != "WAIT_OPERATOR" {
		t.Fatal("fresh click did not authorize exactly one new browser attempt")
	}
}
func TestRecoveryCoordinatorAIRevisionAndCaptchaCap(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.observation.State = authbrowser.CaptchaRequired
	f.observation.Revision = "captcha-1"
	f.aiReject = true
	f.restart(true)
	f.reconcile()
	if f.solver.calls != 3 || f.starts != 1 || f.captchas != 3 || f.episode().State != "WAIT_OPERATOR" || f.episode().ReasonCode != "CAPTCHA_BUDGET_EXHAUSTED" {
		t.Fatal("distinct CAPTCHA revisions did not use bounded AI automation in one attempt")
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
				if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_challenges SET status='DELIVERING',prompt_message_id=NULL WHERE id=?`, old.ID); err != nil {
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
	f := newUnconsentedCoordinatorFixture(t)
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
	if f.starts != 0 {
		t.Fatal("reason-code rearm granted login consent")
	}
	f.login()
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
		if e.State != "WAIT_OPERATOR" || deadline.Sub(f.now.Add(-2*time.Second)) != 15*time.Minute || f.logins != 0 || e.ConsentActionID != "" {
			t.Fatal("maintenance did not defer login fifteen minutes")
		}
		f.restart(false)
		if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_recovery_episodes SET next_attempt_at=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), e.ID); err != nil {
			t.Fatal(err)
		}
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

func TestRecoveryCoordinatorWaitsForButtonDespiteOperationalEvidence(t *testing.T) {
	f := newUnconsentedCoordinatorFixture(t)
	f.observation.CaptchaRequired = true
	f.restart(true)
	for range 3 {
		f.reconcile()
		f.restart(true)
	}
	e := f.episode()
	notices, err := f.store.PendingAuthRecoveryNotices(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if e.State != "DETECTED" || len(notices) != 1 || notices[0].Kind != "DETECTED" {
		t.Fatal("session loss did not retain a single durable detection notice")
	}
	if f.starts != 0 || f.logins != 0 || f.observes != 0 || f.credentialReads != 0 || f.solver.calls != 0 {
		t.Fatal("operational evidence authorized browser, credentials or AI before consent")
	}
	a := f.login()
	f.reconcile()
	if f.starts != 1 || f.logins != 1 || f.solver.calls != 1 || f.credentialReads != 1 {
		t.Fatal("valid button did not admit the login pipeline")
	}
	if _, _, err := f.store.ConsumeTelegramAuthAction(f.ctx, a.ID, 1, 22, 33, 77, time.Now()); !errors.Is(err, storage.ErrChallengeConsumed) {
		t.Fatalf("duplicate button accepted: %v", err)
	}
	f.restart(true)
	f.reconcile()
	if f.starts != 1 || f.logins != 1 || f.solver.calls != 1 {
		t.Fatal("duplicate callback or restart replayed the consent")
	}
}

func TestRecoveryCoordinatorExpiredConsentDoesNotStart(t *testing.T) {
	f := newCoordinatorFixture(t)
	if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_recovery_episodes SET consent_expires_at=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	f.restart(true)
	for range 3 {
		f.reconcile()
	}
	if f.starts != 0 || f.observes != 0 || f.credentialReads != 0 || f.solver.calls != 0 {
		t.Fatal("expired button consent admitted bank side effects")
	}
}

func TestRecoveryCoordinatorResumeDoesNotGrantConsent(t *testing.T) {
	f := newUnconsentedCoordinatorFixture(t)
	f.reconcile()
	for _, operation := range []string{"PAUSE", "RESUME"} {
		e := f.episode()
		if _, err := f.store.ApplyTelegramAuthOperation(f.ctx, 1, e.ID, e.Generation, operation); err != nil {
			t.Fatal(err)
		}
	}
	f.restart(true)
	f.reconcile()
	if f.starts != 0 || f.credentialReads != 0 || f.solver.calls != 0 {
		t.Fatal("resume authorized a login without a button")
	}
}

func TestRecoveryCoordinatorRestartStartingAdoptsWithoutReplay(t *testing.T) {
	f := newCoordinatorFixture(t)
	e := f.episode()
	if _, err := f.store.StartRecoveryAuthAttempt(f.ctx, e.ID, e.Generation, BrowserAttemptTTL); err != nil {
		t.Fatal(err)
	}
	f.observation.State = authbrowser.OTPRequired
	f.observation.Revision = "existing-otp"
	f.restart(false)
	f.reconcile()
	if f.starts != 0 || f.logins != 0 || f.otps != 0 || f.credentialReads != 0 || f.pending().BrowserRevision != "existing-otp" {
		t.Fatal("starting crash replayed browser start or credentials instead of adopting observed progress")
	}
}

func TestRecoveryCoordinatorRestartReservedLoginNeverReplays(t *testing.T) {
	for _, captcha := range []bool{false, true} {
		t.Run(fmt.Sprintf("captcha-%t", captcha), func(t *testing.T) {
			f := newCoordinatorFixture(t)
			e := f.episode()
			a, err := f.store.StartRecoveryAuthAttempt(f.ctx, e.ID, e.Generation, BrowserAttemptTTL)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.MarkAuthAttemptInProgress(f.ctx, a.ID); err != nil {
				t.Fatal(err)
			}
			e = f.episode()
			if err := f.store.TransitionAuthRecovery(f.ctx, e.ID, e.Generation, e.State, "LOGIN", ""); err != nil {
				t.Fatal(err)
			}
			if err := f.store.RecordRecoveryLogin(f.ctx, e.ID, e.Generation); err != nil {
				t.Fatal(err)
			}
			f.observation.CaptchaRequired = captcha
			f.restart(true)
			f.reconcile()
			e = f.episode()
			if e.State != "WAIT_OPERATOR" || e.ReasonCode != "LOGIN_OUTCOME_UNKNOWN" || e.ConsentActionID != "" || f.starts != 0 || f.logins != 0 || f.credentialReads != 0 || f.solver.calls != 0 {
				t.Fatal("reserved login crash replayed plaintext or AI on a returned login form")
			}
		})
	}
}

func TestRecoveryCoordinatorActiveConsentFenceBlocksObservation(t *testing.T) {
	for _, fence := range []string{"consent", "credential-revision", "action"} {
		t.Run(fence, func(t *testing.T) {
			f := newCoordinatorFixture(t)
			f.reconcile()
			e := f.episode()
			switch fence {
			case "consent":
				_, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_recovery_episodes SET consent_consumed_at=NULL WHERE id=?`, e.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "credential-revision":
				_, err := f.store.DB().ExecContext(f.ctx, `UPDATE acb_credentials SET revision=revision+1 WHERE connection_id=?`, e.ConnectionID)
				if err != nil {
					t.Fatal(err)
				}
			case "action":
				_, err := f.store.DB().ExecContext(f.ctx, `UPDATE telegram_auth_actions SET attempt_id='other-attempt' WHERE id=?`, e.ConsentActionID)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := f.observes
			f.restart(true)
			if err := f.controller.ReconcileOnce(f.ctx); err == nil {
				t.Fatal("invalid consumed consent was ignored")
			}
			if f.observes != before || f.logins != 1 || f.credentialReads != 1 || f.solver.calls != 0 {
				t.Fatal("invalid consent or stale credential revision reached external I/O")
			}
		})
	}
}

func TestRecoveryCoordinatorFenceDuringCredentialReadBlocksLogin(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.controller.Credentials = func(context.Context, string) (storage.ACBCredentials, error) {
		e := f.episode()
		if _, err := f.store.ApplyTelegramAuthOperation(f.ctx, 1, e.ID, e.Generation, "PAUSE"); err != nil {
			return storage.ACBCredentials{}, err
		}
		return storage.ACBCredentials{Username: "user-secret", Password: "password-secret", AccountNumber: "account-secret", Revision: 1}, nil
	}
	if err := f.controller.ReconcileOnce(f.ctx); err == nil {
		t.Fatal("credential-read fence was ignored")
	}
	if f.starts != 1 || f.logins != 0 || f.otps != 0 || f.episode().ConsentActionID != "" {
		t.Fatal("revoked attempt submitted credentials after loading secrets")
	}
}

func (f *coordinatorFixture) expireLoginCooldown() {
	f.t.Helper()
	e := f.episode()
	last := time.Now().Add(-LoginCooldown - time.Second).UTC().Format(time.RFC3339Nano)
	created := time.Now().Add(-2 * LoginCooldown).UTC().Format(time.RFC3339Nano)
	if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_recovery_episodes SET last_login_at=? WHERE id=?`, last, e.ID); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE auth_attempts SET created_at=? WHERE id=?`, created, e.AttemptID); err != nil {
		f.t.Fatal(err)
	}
}

func TestRecoveryCoordinatorCaptchaRejectionRequiresEvidenceAndNewRevision(t *testing.T) {
	for _, reason := range []string{"", "UNRECOGNIZED_REJECTION", "CAPTCHA_REJECTED", "INVALID_CAPTCHA"} {
		for _, revision := range []string{"login-1", "login-2"} {
			t.Run(reason+"/"+revision, func(t *testing.T) {
				f := newCoordinatorFixture(t)
				f.observation.CaptchaRequired = true
				f.loginReplies = []authbrowser.AuthObservation{{State: authbrowser.LoginForm, CaptchaRequired: true, Revision: revision, ReasonCode: reason, ExpiresAt: f.observation.ExpiresAt}}
				f.restart(true)
				f.reconcile()
				explicit := reason == "CAPTCHA_REJECTED" || reason == "INVALID_CAPTCHA"
				if !explicit || revision == "login-1" {
					if e := f.episode(); e.State != "WAIT_OPERATOR" || e.ReasonCode != "LOGIN_OUTCOME_UNKNOWN" || f.logins != 1 || f.solver.calls != 1 {
						t.Fatal("changed form or repeated revision replayed credentials")
					}
					return
				}
				if f.episode().State != "WAITING_CAPTCHA" || f.logins != 1 || f.solver.calls != 1 {
					t.Fatal("explicit rejection bypassed login cooldown")
				}
				f.expireLoginCooldown()
				f.reconcile()
				if e := f.episode(); e.State != "WAITING_OTP" || e.AttemptCount != 1 || e.CaptchaSubmissions != 2 || f.starts != 1 || f.logins != 2 || f.solver.calls != 2 {
					t.Fatal("explicit new captcha revision did not retry inside the consent attempt")
				}
			})
		}
	}
}

func TestRecoveryCoordinatorFullLoginCaptchaRetryCapAndABA(t *testing.T) {
	for _, ending := range []string{"D", "login-1"} {
		t.Run(ending, func(t *testing.T) {
			f := newCoordinatorFixture(t)
			f.observation.CaptchaRequired = true
			for _, revision := range []string{"B", "C", ending} {
				f.loginReplies = append(f.loginReplies, authbrowser.AuthObservation{State: authbrowser.LoginForm, CaptchaRequired: true, Revision: revision, ReasonCode: "CAPTCHA_REJECTED", ExpiresAt: f.observation.ExpiresAt})
			}
			f.restart(true)
			f.reconcile()
			for range 2 {
				f.expireLoginCooldown()
				f.reconcile()
			}
			if e := f.episode(); e.State != "WAIT_OPERATOR" || e.CaptchaSubmissions != 3 || e.AIUsed != 3 || f.starts != 1 || f.logins != 3 || f.solver.calls != 3 {
				t.Fatal("credential retry cap or A-B-A revision fence bypassed")
			}
		})
	}
}

func TestRecoveryCoordinatorAIFailureBudgetFallsBackToHuman(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.observation.State = authbrowser.CaptchaRequired
	f.observation.Revision = "A"
	f.solver.fail = true
	f.restart(true)
	f.reconcile()
	for _, revision := range []string{"B", "A", "C", "D"} {
		f.observation.Revision = revision
		f.restart(true)
		f.reconcile()
	}
	ch := f.pending()
	if e := f.episode(); f.solver.calls != 3 || e.AIUsed != 3 || ch.BrowserRevision != "D" || f.captchas != 0 || f.logins != 0 {
		t.Fatal("AI failed revision replayed after restart or budget did not fall back")
	}
	if err := f.broker.HandleReply(f.ctx, 22, ch.PromptMessageID, 901, "AB12CD"); err != nil {
		t.Fatal(err)
	}
	if f.captchas != 1 || f.solver.calls != 3 || f.pending().Kind != "OTP" {
		t.Fatal("human fallback did not advance without restarting or invoking AI")
	}
}

func TestRecoveryCoordinatorLiveDeliveryIsNotCrash(t *testing.T) {
	f := newCoordinatorFixture(t)
	if err := f.controller.ReconcileOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	ch, err := f.store.ActiveAuthChallenge(f.ctx, f.episode().AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Status != "DELIVERING" || ch.PromptMessageID != 0 || f.prompts != 0 {
		t.Fatal("prompt sent under bank reconcile lock")
	}
	f.now = f.now.Add(2 * time.Second)
	if err := f.controller.ReconcileOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	current, err := f.store.ActiveAuthChallenge(f.ctx, f.episode().AttemptID)
	if err != nil || current.ID != ch.ID || current.Status != "DELIVERING" || f.logins != 1 {
		t.Fatal("in-flight delivery invalidated as a crash")
	}
	if err := f.broker.DeliverPending(f.ctx); err != nil {
		t.Fatal(err)
	}
	current, err = f.store.ActiveAuthChallenge(f.ctx, f.episode().AttemptID)
	if err != nil || current.ID != ch.ID || current.Status != "PENDING" || f.prompts != 1 {
		t.Fatal("delivery loop did not publish reserved prompt")
	}
}

func TestRecoveryCoordinatorOneConsentAICaptchaAndOTP(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.observation.CaptchaRequired = true
	f.restart(true)
	keyring, err := security.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.store.WithKeyring(keyring)
	verified := 0
	verifier := coordinatorVerifier(func(ctx context.Context, connectionID string, generation int64, encrypted []byte) error {
		var envelope security.Envelope
		if err := json.Unmarshal(encrypted, &envelope); err != nil {
			return err
		}
		plaintext, err := keyring.Decrypt(envelope, security.SessionAAD(connectionID, generation))
		if err != nil {
			return err
		}
		defer clear(plaintext)
		if string(plaintext) != "synthetic-authenticated-session" {
			return errors.New("unexpected fixture session")
		}
		verified++
		return nil
	})
	f.controller.Finalizer = authsession.NewFinalizer(authsession.Options{Store: f.store, Browser: f.browser, Keyring: keyring, Verifier: verifier})
	f.reconcile()
	ch := f.pending()
	if ch.Kind != "OTP" || f.solver.calls != 1 || f.logins != 1 || f.episode().CaptchaSubmissions != 1 {
		t.Fatal("single click did not automatically solve initial captcha and reach OTP")
	}
	if err := f.broker.HandleReply(f.ctx, 22, ch.PromptMessageID, 901, "001234"); err != nil {
		t.Fatal(err)
	}
	if err := f.broker.HandleReply(f.ctx, 22, ch.PromptMessageID, 902, "001234"); !errors.Is(err, storage.ErrChallengeConsumed) {
		t.Fatalf("OTP replay: %v", err)
	}
	e := f.episode()
	if e.State != "CATCHING_UP" || e.AttemptCount != 1 || e.OTPSubmissions != 1 || f.starts != 1 || f.otps != 1 || f.logins != 1 || verified != 1 {
		t.Fatal("OTP did not automatically commit verification and durable catchup in the single consent attempt")
	}
	blocked, err := f.store.HasBlockingAuthRecovery(f.ctx, e.ConnectionID, e.Generation)
	if err != nil || !blocked {
		t.Fatal("verified session opened monitoring before catchup")
	}
	run, err := f.store.GetRecoveryRun(f.ctx, e.RecoveryRunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimRecoveryRun(f.ctx, run.ID, e.ConnectionID, e.Generation); err != nil {
		t.Fatal(err)
	}
	from, err := time.Parse("2006-01-02", run.RangeFrom)
	if err != nil {
		t.Fatal(err)
	}
	to, err := time.Parse("2006-01-02", run.RangeTo)
	if err != nil {
		t.Fatal(err)
	}
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		if _, err := f.store.CommitRecoveryDay(f.ctx, storage.RecoveryDayCommit{RunID: run.ID, ConnectionID: e.ConnectionID, Generation: e.Generation, Day: day.Format("2006-01-02"), NextDay: day.AddDate(0, 0, 1).Format("2006-01-02"), CoverageFrom: e.RequiredFrom}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.UpdateRecoveryRunProgress(f.ctx, run.ID, e.ConnectionID, e.Generation, storage.RecoveryRunStatusCompleted, "{}", "", ""); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	conn, err := f.store.Connection(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err = f.store.HasBlockingAuthRecovery(f.ctx, e.ConnectionID, e.Generation)
	if err != nil || blocked || conn.State != "MONITORING" || f.episode().State != "COMPLETED" || f.starts != 1 || f.logins != 1 || f.otps != 1 || verified != 1 {
		t.Fatal("completed catchup did not release monitoring without another consent or bank submission")
	}
}

func TestRecoveryCoordinatorReservedAIRevisionCrashNeverReplays(t *testing.T) {
	f := newCoordinatorFixture(t)
	e := f.episode()
	a, err := f.store.StartRecoveryAuthAttempt(f.ctx, e.ID, e.Generation, BrowserAttemptTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkAuthAttemptInProgress(f.ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.TransitionAuthRecovery(f.ctx, e.ID, a.Generation, "STARTING", "WAITING_CAPTCHA", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ClaimRecoveryAI(f.ctx, e.ID, a.Generation, "reserved-captcha"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ReserveRecoverySubmission(f.ctx, e.ID, a.Generation, "CAPTCHA_TEXT", false); err != nil {
		t.Fatal(err)
	}
	f.observation.State = authbrowser.CaptchaRequired
	f.observation.Revision = "reserved-captcha"
	f.restart(true)
	f.reconcile()
	if e := f.episode(); e.State != "WAIT_OPERATOR" || e.ReasonCode != "CHALLENGE_OUTCOME_UNKNOWN" || f.solver.calls != 0 || f.captchas != 0 || f.prompts != 0 || f.starts != 0 {
		t.Fatal("reserved captcha revision was replayed or reprompted after crash")
	}
}

func TestCoordinatorThreeFailedClicksPermitFreshBrowserAttempt(t *testing.T) {
	f := newUnconsentedCoordinatorFixture(t)
	f.startFail = true
	for i := 1; i <= 3; i++ {
		f.login()
		f.reconcile()
		e := f.episode()
		expected := "WAIT_OPERATOR"
		if i == 3 {
			expected = "MANUAL_REQUIRED"
		}
		if f.starts != i || e.AttemptCount != i || e.State != expected {
			t.Fatalf("failed click %d: starts=%d state=%s attempts=%d", i, f.starts, e.State, e.AttemptCount)
		}
		if _, err := f.store.DB().Exec(`UPDATE auth_recovery_episodes SET next_attempt_at=NULL WHERE id=?`, e.ID); err != nil {
			t.Fatal(err)
		}
		f.reconcile()
		if f.starts != i {
			t.Fatal("failed attempt retried without fresh button")
		}
	}
	f.startFail = false
	f.login()
	f.reconcile()
	e := f.episode()
	if f.starts != 4 || e.AttemptCount != 4 || e.BudgetStartCount != 3 || e.State != "WAITING_OTP" || f.logins != 1 {
		t.Fatalf("fourth fresh button stuck: starts=%d logins=%d episode=%+v", f.starts, f.logins, e)
	}
}
