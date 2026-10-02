package authrecovery

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/captchasolver"
	"github.com/thedemontuan/acb-transaction-webhook/internal/challenge"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telegramauth"
)

type RecoveryBrowser interface {
	Start(context.Context, string) (authbrowser.Session, error)
	Status(context.Context, string) (authbrowser.Session, error)
	Observe(context.Context, string) (authbrowser.AuthObservation, error)
	CaptureCaptcha(context.Context, string, string) ([]byte, error)
	SubmitLogin(context.Context, string, authbrowser.LoginInput) (authbrowser.AuthObservation, error)
	SubmitCaptcha(context.Context, string, authbrowser.ChallengeInput) (authbrowser.AuthObservation, error)
	SubmitOTP(context.Context, string, authbrowser.ChallengeInput) (authbrowser.AuthObservation, error)
	Cancel(context.Context, string) error
	Complete(context.Context, string) error
}
type Finalizer interface {
	Complete(context.Context, storage.AuthAttempt) (storage.Connection, error)
}
type TelegramReadiness interface {
	Readiness() telegramauth.TransportState
}
type NoticeDelivery interface{ DeliverNotices(context.Context) error }
type CoordinatorOptions struct {
	Config      Config
	Store       *storage.Store
	Browser     RecoveryBrowser
	Broker      *challenge.Broker
	Finalizer   Finalizer
	Telegram    TelegramReadiness
	Notices     NoticeDelivery
	WorkerReady func(context.Context) error
	Solver      captchasolver.Solver
	Credentials func() (Credentials, error)
	Now         func() time.Time
	// Jitter returns an additional fraction in [0,0.2].
	Jitter func() float64
}
type observationWindow struct {
	first time.Time
	count int
	next  time.Time
}
type Coordinator struct {
	CoordinatorOptions
	// Reconcile and the reply submitter share one operation lock. Never hold a
	// storage transaction over external I/O. Browser revisions fence other actors.
	mu             sync.Mutex
	windows        map[string]observationWindow
	verifyAfter    map[string]time.Time
	liveChallenges map[string]time.Time
	degradedMu     sync.RWMutex
	aiDegraded     string
}

func NewCoordinator(o CoordinatorOptions) (*Coordinator, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Jitter == nil {
		o.Jitter = func() float64 { return rand.Float64() * 0.2 }
	}
	if o.Credentials == nil {
		o.Credentials = o.Config.ReadCredentials
	}
	c := &Coordinator{CoordinatorOptions: o, windows: make(map[string]observationWindow), verifyAfter: make(map[string]time.Time), liveChallenges: make(map[string]time.Time)}
	if !o.Config.Enabled {
		return c, nil
	}
	if o.Store == nil || o.Browser == nil || o.Broker == nil || o.Finalizer == nil || o.Telegram == nil || o.WorkerReady == nil || o.Config.AICaptchaEnabled && o.Solver == nil {
		return nil, errors.New("RECOVERY_CONFIG_INVALID")
	}
	o.Broker.Submitter = c
	return c, nil
}
func (c *Coordinator) AIDegraded() string {
	c.degradedMu.RLock()
	defer c.degradedMu.RUnlock()
	return c.aiDegraded
}
func (c *Coordinator) Run(ctx context.Context) error {
	if !c.Config.Enabled {
		return nil
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		// Integration errors are contained in this process, not propagated to worker.
		reconcileErr := c.ReconcileOnce(ctx)
		interval := ReconcileInterval
		c.mu.Lock()
		for _, w := range c.windows {
			if !w.next.IsZero() {
				interval = 1500 * time.Millisecond
				break
			}
		}
		c.mu.Unlock()
		if reconcileErr != nil {
			interval = ReconcileInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
func (c *Coordinator) ReconcileOnce(ctx context.Context) error {
	if !c.Config.Enabled {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	reconcileErr := c.reconcile(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if c.Notices != nil && ctx.Err() == nil {
		if err := c.Notices.DeliverNotices(ctx); err != nil {
			return errors.New("RECOVERY_NOTICE_UNAVAILABLE")
		}
	}
	if reconcileErr != nil {
		return errors.New("RECOVERY_RECONCILE_UNAVAILABLE")
	}
	return nil
}
func (c *Coordinator) current(ctx context.Context, e storage.AuthRecoveryEpisode, active bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var matches bool
	if err := c.Store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connections WHERE id=? AND generation=? AND config_revision=?)`, e.ConnectionID, e.Generation, e.ConfigRevision).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return storage.ErrRecoverySuperseded
	}
	latest, err := c.Store.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		return err
	}
	if latest.FinishedAt != "" || latest.Generation != e.Generation || latest.AttemptID != e.AttemptID || latest.State != e.State {
		return storage.ErrRecoverySuperseded
	}
	if active {
		_, err = c.Store.AuthAttemptForOwner(ctx, e.AttemptID, AutomaticOwner)
		if err != nil {
			return err
		}
		var paused bool
		if err := c.Store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_auth_state WHERE paused=1)`).Scan(&paused); err != nil {
			return err
		}
		if paused {
			return storage.ErrRecoveryPaused
		}
		if err := c.Store.CheckMutationAllowed(ctx); err != nil {
			return err
		}
	}
	return nil
}
func (c *Coordinator) ready(ctx context.Context) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	state := c.Telegram.Readiness()
	if !state.Ready || state.Stopped || state.BotID == 0 {
		return false, nil
	}
	paused, err := c.Store.TelegramAuthState(ctx, state.BotID)
	if err != nil {
		return false, err
	}
	if paused.Paused {
		return false, nil
	}
	if err := c.Store.CheckMutationAllowed(ctx); err != nil {
		return false, err
	}
	if err := c.WorkerReady(ctx); err != nil {
		return false, nil
	}
	return ctx.Err() == nil, nil
}
func (c *Coordinator) reconcile(ctx context.Context) error {
	conn, err := c.Store.Connection(ctx)
	if err != nil {
		return err
	}
	e, err := c.Store.LatestAuthRecoveryEpisode(ctx, conn.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var revision int64
		if err := c.Store.DB().QueryRowContext(ctx, `SELECT config_revision FROM connections WHERE id=?`, conn.ID).Scan(&revision); err != nil {
			return err
		}
		if e.Generation != conn.Generation || e.ConfigRevision != revision {
			if e.FinishedAt == "" {
				if err := c.Store.SupersedeAuthRecovery(ctx, e.ID); err != nil {
					return err
				}
				if e.AttemptID != "" && ctx.Err() == nil {
					_ = c.Browser.Cancel(ctx, e.AttemptID)
				}
			}
			delete(c.windows, e.AttemptID)
			delete(c.verifyAfter, e.AttemptID)
			clear(c.liveChallenges)
			e = storage.AuthRecoveryEpisode{}
		}
	}
	if e.ID == "" {
		if conn.State != "AUTH_REQUIRED" {
			return nil
		}
		known, err := c.Store.HasPriorOperationalEvidence(ctx, conn.ID)
		if err != nil {
			return err
		}
		if !known {
			return nil
		}
		e, err = c.Store.EnsureAuthRecoveryEpisode(ctx, conn.ID, conn.Generation)
		if err != nil {
			return err
		}
	}
	if e.FinishedAt != "" {
		// CANCELLED at the current generation must not create another episode.
		if e.State == "COMPLETED" && e.AttemptID != "" && ctx.Err() == nil {
			_ = c.Browser.Complete(ctx, e.AttemptID)
		}
		delete(c.windows, e.AttemptID)
		delete(c.verifyAfter, e.AttemptID)
		clear(c.liveChallenges)
		return nil
	}
	if e.State == "CATCHING_UP" {
		return c.catchup(ctx, e)
	}
	if e.State == "WAIT_OPERATOR" || e.State == "MANUAL_REQUIRED" {
		delete(c.windows, e.AttemptID)
		delete(c.verifyAfter, e.AttemptID)
		clear(c.liveChallenges)
		return nil
	}
	if e.State == "DETECTED" || e.State == "RETRY_WAIT" || e.State == "MAINTENANCE_WAIT" {
		delete(c.windows, e.AttemptID)
		if e.ReasonCode != "OPERATOR_CONFIRMED" {
			known, err := c.Store.HasPriorOperationalEvidence(ctx, e.ConnectionID)
			if err != nil {
				return err
			}
			if !known {
				return nil
			}
		}
		ok, err := c.ready(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		active, err := c.Store.HasActiveAuthAttempt(ctx, conn.ID)
		if err != nil {
			return err
		}
		if active {
			return nil
		}
		a, err := c.Store.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, BrowserAttemptTTL)
		if errors.Is(err, storage.ErrRecoveryNotReady) || errors.Is(err, storage.ErrRecoveryCooldown) || errors.Is(err, storage.ErrAuthAttemptActive) || errors.Is(err, storage.ErrRecoveryPaused) {
			return nil
		}
		if err != nil {
			return err
		}
		e, err = c.Store.AuthRecoveryEpisode(ctx, e.ID)
		if err != nil {
			return err
		}
		if err := c.current(ctx, e, true); err != nil {
			return err
		}
		if _, err := c.Browser.Start(ctx, a.ID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if authbrowser.IsHTTPStatus(err, http.StatusConflict) {
				return c.finish(ctx, e, "WAIT_OPERATOR", "MANUAL_ACTIVE", time.Time{})
			}
			return c.retry(ctx, e, "BROWSER_UNAVAILABLE")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.Store.MarkAuthAttemptInProgress(ctx, a.ID); err != nil {
			return err
		}
		if err := c.transition(ctx, &e, "LOGIN", ""); err != nil {
			return err
		}
	}
	a, err := c.Store.AuthAttemptStatusForOwner(ctx, e.AttemptID, AutomaticOwner)
	if err != nil {
		return err
	}
	a.OwnerSubject = AutomaticOwner
	if a.Status == "VERIFIED" {
		// Complete is idempotent and discovers the committed run by event key;
		// it must not request a second browser handoff after a crash.
		if err := c.current(ctx, e, false); err != nil {
			return err
		}
		_, err = c.Finalizer.Complete(ctx, a)
		return c.finalizerError(ctx, e, err)
	}
	expiry, err := time.Parse(time.RFC3339Nano, a.ExpiresAt)
	if err != nil {
		return err
	}
	if !c.Now().Before(expiry) {
		state, reason := "RETRY_WAIT", "BROWSER_EXPIRED"
		if e.State == "WAITING_OTP" || e.State == "WAITING_CAPTCHA" || e.State == "VERIFYING" {
			state, reason = "WAIT_OPERATOR", "CHALLENGE_EXPIRED"
		}
		return c.finish(ctx, e, state, reason, c.retryTime(e))
	}
	if err := c.current(ctx, e, true); err != nil {
		return err
	}
	if a.Status == "STARTING" {
		// Never replay Start after an uncertain crash boundary; adopt only an
		// existing browser with this exact ID, otherwise finish against budget.
		if _, err := c.Browser.Status(ctx, a.ID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return c.retry(ctx, e, "BROWSER_UNAVAILABLE")
		}
		if err := c.Store.MarkAuthAttemptInProgress(ctx, a.ID); err != nil {
			return err
		}
		if err := c.transition(ctx, &e, "LOGIN", ""); err != nil {
			return err
		}
	}
	w := c.windows[a.ID]
	if c.Now().Before(w.next) {
		return nil
	}
	w.next = c.Now().Add(1500 * time.Millisecond)
	c.windows[a.ID] = w
	observation, err := c.Browser.Observe(ctx, a.ID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if authbrowser.IsHTTPStatus(err, http.StatusGone) && (e.State == "WAITING_OTP" || e.State == "WAITING_CAPTCHA" || e.State == "VERIFYING") {
			return c.finish(ctx, e, "WAIT_OPERATOR", "CHALLENGE_EXPIRED", time.Time{})
		}
		return c.retry(ctx, e, "BROWSER_UNAVAILABLE")
	}
	if err := c.current(ctx, e, true); err != nil {
		return err
	}
	hold, err := c.recoverChallenge(ctx, e, observation)
	if err != nil || hold {
		return err
	}
	return c.advance(ctx, e, observation)
}
func (c *Coordinator) transition(ctx context.Context, e *storage.AuthRecoveryEpisode, state, reason string) error {
	if e.State == state {
		return nil
	}
	if err := c.Store.TransitionAuthRecovery(ctx, e.ID, e.Generation, e.State, state, reason); err != nil {
		return err
	}
	e.State = state
	e.ReasonCode = reason
	return nil
}
func (c *Coordinator) retryTime(e storage.AuthRecoveryEpisode) time.Time {
	delay := 30 * time.Second
	if e.AttemptCount-e.BudgetStartCount >= 2 {
		delay = 120 * time.Second
	}
	jitter := c.Jitter()
	if jitter < 0 {
		jitter = 0
	}
	if jitter > 0.2 {
		jitter = 0.2
	}
	return c.Now().Add(delay + time.Duration(float64(delay)*jitter))
}
func (c *Coordinator) retry(ctx context.Context, e storage.AuthRecoveryEpisode, reason string) error {
	return c.finish(ctx, e, "RETRY_WAIT", reason, c.retryTime(e))
}
func (c *Coordinator) finish(ctx context.Context, e storage.AuthRecoveryEpisode, state, reason string, next time.Time) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := c.Store.FinishRecoveryAuthAttempt(ctx, e.ID, e.Generation, "FAILED", state, reason, next); err != nil {
		return err
	}
	delete(c.windows, e.AttemptID)
	delete(c.verifyAfter, e.AttemptID)
	clear(c.liveChallenges)
	if ctx.Err() == nil {
		_ = c.Browser.Cancel(ctx, e.AttemptID)
	}
	return nil
}
func (c *Coordinator) recoverChallenge(ctx context.Context, e storage.AuthRecoveryEpisode, o authbrowser.AuthObservation) (bool, error) {
	ch, err := c.Store.ActiveAuthChallenge(ctx, e.AttemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if ch.Status == "CONSUMING" {
		// The polling goroutine commits CONSUMING before entering SubmitChallenge.
		// A prompt seen in this process can be an in-flight reply, not a crash.
		// Give it the operation lock on the next turn, with a bounded wait.
		if since, known := c.liveChallenges[ch.ID]; known {
			if since.IsZero() {
				c.liveChallenges[ch.ID] = c.Now()
				return true, nil
			}
			if c.Now().Sub(since) < 30*time.Second {
				return true, nil
			}
		}
		delete(c.liveChallenges, ch.ID)
		if err := c.Store.FinishAuthChallenge(ctx, ch.ID, "INVALIDATED"); err != nil {
			return true, err
		}
		if o.State == authbrowser.Authenticated || ch.Kind == "CAPTCHA_TEXT" && o.State == authbrowser.OTPRequired {
			return false, nil
		}
		return true, c.finish(ctx, e, "WAIT_OPERATOR", "CHALLENGE_OUTCOME_UNKNOWN", time.Time{})
	}
	expiry, err := time.Parse(time.RFC3339Nano, ch.ExpiresAt)
	if err != nil {
		return true, err
	}
	if !c.Now().Before(expiry) {
		if err := c.Store.FinishAuthChallenge(ctx, ch.ID, "EXPIRED"); err != nil {
			return true, err
		}
		return true, c.finish(ctx, e, "WAIT_OPERATOR", "CHALLENGE_EXPIRED", time.Time{})
	}
	matches := ch.Generation == e.Generation && ch.BrowserRevision == o.Revision && ((ch.Kind == "OTP" && o.State == authbrowser.OTPRequired) || (ch.Kind == "CAPTCHA_TEXT" && (o.State == authbrowser.CaptchaRequired || o.State == authbrowser.LoginForm && o.CaptchaRequired)))
	if ch.Status == "PENDING" && matches {
		c.liveChallenges[ch.ID] = time.Time{}
		return true, nil
	}
	// DELIVERING is always uncertain after a restart. Only a persisted PENDING
	// prompt is accepted; invalidate before publishing its replacement.
	if err := c.Store.FinishAuthChallenge(ctx, ch.ID, "INVALIDATED"); err != nil {
		return true, err
	}
	delete(c.liveChallenges, ch.ID)
	return false, nil
}
func (c *Coordinator) advance(ctx context.Context, e storage.AuthRecoveryEpisode, o authbrowser.AuthObservation) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch o.State {
	case authbrowser.Authenticated:
		if err := c.transition(ctx, &e, "VERIFYING", ""); err != nil {
			return err
		}
		if c.Now().Before(c.verifyAfter[e.AttemptID]) {
			return nil
		}
		c.verifyAfter[e.AttemptID] = c.Now().Add(5 * time.Second)
		if err := c.current(ctx, e, true); err != nil {
			return err
		}
		a, err := c.Store.AuthAttemptForOwner(ctx, e.AttemptID, AutomaticOwner)
		if err != nil {
			return err
		}
		a.OwnerSubject = AutomaticOwner
		_, err = c.Finalizer.Complete(ctx, a)
		return c.finalizerError(ctx, e, err)
	case authbrowser.LoginForm:
		if o.CaptchaRequired {
			return c.captcha(ctx, e, o)
		}
		if e.LastLoginAt != "" {
			last, err := time.Parse(time.RFC3339Nano, e.LastLoginAt)
			if err != nil {
				return err
			}
			attempt, err := c.Store.AuthAttemptForOwner(ctx, e.AttemptID, AutomaticOwner)
			if err != nil {
				return err
			}
			created, err := time.Parse(time.RFC3339Nano, attempt.CreatedAt)
			if err != nil {
				return err
			}
			if !last.Before(created) {
				return c.finish(ctx, e, "WAIT_OPERATOR", "LOGIN_OUTCOME_UNKNOWN", time.Time{})
			}
		}
		if err := c.transition(ctx, &e, "LOGIN", ""); err != nil {
			return err
		}
		return c.submitLogin(ctx, e, o, "", false, "")
	case authbrowser.CaptchaRequired:
		return c.captcha(ctx, e, o)
	case authbrowser.OTPRequired:
		if e.OTPSubmissions >= MaxOTPSubmissions {
			return c.finish(ctx, e, "WAIT_OPERATOR", "OTP_REJECTED", time.Time{})
		}
		if err := c.transition(ctx, &e, "WAITING_OTP", ""); err != nil {
			return err
		}
		if err := c.current(ctx, e, true); err != nil {
			return err
		}
		prompt, err := c.Broker.Prompt(ctx, e, o, "OTP", nil)
		if err == nil {
			c.liveChallenges[prompt.ID] = time.Time{}
		}
		return err
	case authbrowser.LoginRejected:
		switch o.ReasonCode {
		case "CAPTCHA_REJECTED", "INVALID_CAPTCHA":
			return c.unknown(ctx, e)
		case "OTP_REJECTED", "OTP_EXPIRED", "INVALID_OTP":
			return c.finish(ctx, e, "WAIT_OPERATOR", "OTP_REJECTED", time.Time{})
		case "ACCOUNT_LOCKED":
			return c.finish(ctx, e, "MANUAL_REQUIRED", "ACCOUNT_LOCKED", time.Time{})
		case "CREDENTIALS_REJECTED":
			return c.finish(ctx, e, "MANUAL_REQUIRED", "CREDENTIALS_REJECTED", time.Time{})
		default:
			return c.unknown(ctx, e)
		}
	case authbrowser.Maintenance:
		return c.finish(ctx, e, "MAINTENANCE_WAIT", "BANK_MAINTENANCE", c.Now().Add(15*time.Minute))
	case authbrowser.UnsupportedChallenge:
		return c.finish(ctx, e, "MANUAL_REQUIRED", "UNSUPPORTED_CHALLENGE", time.Time{})
	default:
		if o.ReasonCode == "ACCOUNT_SELECTION_REQUIRED" || o.ReasonCode == "UNSAFE_CAPTCHA_CROP" || o.ReasonCode == "AMBIGUOUS_CONTROLS" {
			return c.finish(ctx, e, "MANUAL_REQUIRED", "UNSUPPORTED_PAGE", time.Time{})
		}
		return c.unknown(ctx, e)
	}
}
func (c *Coordinator) unknown(ctx context.Context, e storage.AuthRecoveryEpisode) error {
	w := c.windows[e.AttemptID]
	if w.first.IsZero() {
		w.first = c.Now()
	}
	w.count++
	c.windows[e.AttemptID] = w
	if w.count >= 10 || c.Now().Sub(w.first) >= 30*time.Second {
		return c.finish(ctx, e, "MANUAL_REQUIRED", "UNKNOWN_PAGE", time.Time{})
	}
	return nil
}
func (c *Coordinator) captcha(ctx context.Context, e storage.AuthRecoveryEpisode, o authbrowser.AuthObservation) error {
	if e.CaptchaSubmissions >= MaxCaptchaSubmissions {
		return c.finish(ctx, e, "WAIT_OPERATOR", "CAPTCHA_BUDGET_EXHAUSTED", time.Time{})
	}
	if err := c.transition(ctx, &e, "WAITING_CAPTCHA", ""); err != nil {
		return err
	}
	if o.State == authbrowser.LoginForm && e.LastLoginAt != "" {
		last, err := time.Parse(time.RFC3339Nano, e.LastLoginAt)
		if err != nil {
			return err
		}
		// Never consume a human answer before the durable login cooldown permits
		// submission. Wait without retaining any plaintext response.
		if c.Now().Before(last.Add(LoginCooldown)) {
			return nil
		}
	}
	if err := c.current(ctx, e, true); err != nil {
		return err
	}
	image, err := c.Browser.CaptureCaptcha(ctx, e.AttemptID, o.Revision)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return c.finish(ctx, e, "MANUAL_REQUIRED", "CAPTCHA_CAPTURE_UNAVAILABLE", time.Time{})
	}
	defer clear(image)
	if c.Config.AICaptchaEnabled && e.AIUsed == 0 && e.CaptchaSubmissions == 0 {
		if err := c.Store.ClaimRecoveryAI(ctx, e.ID, e.Generation); err != nil {
			return err
		}
		if c.Notices != nil {
			if err := c.Notices.DeliverNotices(ctx); err != nil {
				return errors.New("RECOVERY_NOTICE_UNAVAILABLE")
			}
		}
		if err := c.current(ctx, e, true); err != nil {
			return err
		}
		value, err := c.Solver.Solve(ctx, image)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && challenge.ValidResponse("CAPTCHA_TEXT", value) {
			if o.State == authbrowser.LoginForm {
				return c.submitLogin(ctx, e, o, value, true, "")
			}
			if err := c.current(ctx, e, true); err != nil {
				return err
			}
			if err := c.Store.ReserveRecoverySubmission(ctx, e.ID, e.Generation, "CAPTCHA_TEXT", false); err != nil {
				return err
			}
			next, err := c.Browser.SubmitCaptcha(ctx, e.AttemptID, authbrowser.ChallengeInput{Revision: o.Revision, Value: value})
			value = ""
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return c.finish(ctx, e, "WAIT_OPERATOR", "CHALLENGE_OUTCOME_UNKNOWN", time.Time{})
			}
			e, err = c.Store.AuthRecoveryEpisode(ctx, e.ID)
			if err != nil {
				return err
			}
			return c.advance(ctx, e, next)
		}
		value = ""
		c.degradedMu.Lock()
		c.aiDegraded = "AI_CAPTCHA_UNAVAILABLE"
		c.degradedMu.Unlock()
	}
	if err := c.current(ctx, e, true); err != nil {
		return err
	}
	prompt, err := c.Broker.Prompt(ctx, e, o, "CAPTCHA_TEXT", image)
	if err == nil {
		c.liveChallenges[prompt.ID] = time.Time{}
	}
	return err
}
func (c *Coordinator) submitLogin(ctx context.Context, e storage.AuthRecoveryEpisode, o authbrowser.AuthObservation, value string, ai bool, consumedID string) error {
	ok, err := c.ready(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return storage.ErrRecoveryNotReady
	}
	if err := c.current(ctx, e, true); err != nil {
		return err
	}
	credentials, err := c.Credentials()
	if err != nil {
		return c.finish(ctx, e, "MANUAL_REQUIRED", "CREDENTIALS_UNAVAILABLE", time.Time{})
	}
	if ai {
		err = c.Store.ReserveRecoverySubmission(ctx, e.ID, e.Generation, "CAPTCHA_TEXT", true)
	} else {
		err = c.Store.RecordRecoveryLogin(ctx, e.ID, e.Generation)
	}
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	next, err := c.Browser.SubmitLogin(ctx, e.AttemptID, authbrowser.LoginInput{Revision: o.Revision, Username: credentials.Username, Password: credentials.Password, AccountNumber: credentials.AccountNumber, Captcha: value})
	credentials = Credentials{}
	value = ""
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return c.finish(ctx, e, "WAIT_OPERATOR", "LOGIN_OUTCOME_UNKNOWN", time.Time{})
	}
	if consumedID != "" {
		if err := c.Store.FinishAuthChallenge(ctx, consumedID, "CONSUMED"); err != nil {
			return err
		}
	}
	delete(c.liveChallenges, consumedID)
	e, err = c.Store.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		return err
	}
	return c.advance(ctx, e, next)
}
func (c *Coordinator) SubmitChallenge(ctx context.Context, ch storage.AuthChallenge, value string) (authbrowser.AuthObservation, error) {
	if !c.Config.Enabled {
		return authbrowser.AuthObservation{}, ErrRecoveryDisabled
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Broker already consumed and counted the response. A stale actor must never
	// send it to another browser or reserve the human counter a second time.
	e, err := c.Store.AuthRecoveryEpisode(ctx, ch.EpisodeID)
	if err != nil {
		return authbrowser.AuthObservation{}, errors.New("RECOVERY_STORAGE_UNAVAILABLE")
	}
	if e.Generation != ch.Generation || e.AttemptID != ch.AttemptID || !challenge.ValidResponse(ch.Kind, value) {
		return authbrowser.AuthObservation{}, storage.ErrChallengeMismatch
	}
	if err := c.current(ctx, e, true); err != nil {
		return authbrowser.AuthObservation{}, storage.ErrChallengeMismatch
	}
	var consuming bool
	if err := c.Store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_challenges WHERE id=? AND status='CONSUMING' AND attempt_id=? AND generation=? AND browser_revision=?)`, ch.ID, ch.AttemptID, ch.Generation, ch.BrowserRevision).Scan(&consuming); err != nil {
		return authbrowser.AuthObservation{}, errors.New("RECOVERY_STORAGE_UNAVAILABLE")
	}
	if !consuming {
		return authbrowser.AuthObservation{}, storage.ErrChallengeConsumed
	}
	observation, err := c.Browser.Observe(ctx, ch.AttemptID)
	if err != nil {
		return authbrowser.AuthObservation{}, errors.New("RECOVERY_BROWSER_UNAVAILABLE")
	}
	if observation.Revision != ch.BrowserRevision {
		return authbrowser.AuthObservation{}, storage.ErrChallengeMismatch
	}
	if ch.Kind == "CAPTCHA_TEXT" && observation.State == authbrowser.LoginForm && observation.CaptchaRequired {
		err = c.submitLogin(ctx, e, observation, value, false, ch.ID)
		value = ""
		if err != nil {
			return authbrowser.AuthObservation{}, errors.New("RECOVERY_LOGIN_UNAVAILABLE")
		}
		return authbrowser.AuthObservation{}, nil
	}
	if ch.Kind == "OTP" && observation.State != authbrowser.OTPRequired || ch.Kind == "CAPTCHA_TEXT" && observation.State != authbrowser.CaptchaRequired {
		return authbrowser.AuthObservation{}, storage.ErrChallengeMismatch
	}
	if err := c.current(ctx, e, true); err != nil {
		return authbrowser.AuthObservation{}, storage.ErrChallengeMismatch
	}
	if ctx.Err() != nil {
		return authbrowser.AuthObservation{}, ctx.Err()
	}
	if ch.Kind == "OTP" {
		observation, err = c.Browser.SubmitOTP(ctx, ch.AttemptID, authbrowser.ChallengeInput{Revision: ch.BrowserRevision, Value: value})
	} else {
		observation, err = c.Browser.SubmitCaptcha(ctx, ch.AttemptID, authbrowser.ChallengeInput{Revision: ch.BrowserRevision, Value: value})
	}
	value = ""
	if err != nil {
		if ctx.Err() != nil {
			return observation, ctx.Err()
		}
		_ = c.finish(ctx, e, "WAIT_OPERATOR", "CHALLENGE_OUTCOME_UNKNOWN", time.Time{})
		return observation, errors.New("RECOVERY_CHALLENGE_OUTCOME_UNKNOWN")
	}
	if err := c.Store.FinishAuthChallenge(ctx, ch.ID, "CONSUMED"); err != nil {
		return observation, errors.New("RECOVERY_STORAGE_UNAVAILABLE")
	}
	delete(c.liveChallenges, ch.ID)
	if err := c.advance(ctx, e, observation); err != nil {
		return observation, errors.New("RECOVERY_ADVANCE_UNAVAILABLE")
	}
	return observation, nil
}
func (c *Coordinator) finalizerError(ctx context.Context, e storage.AuthRecoveryEpisode, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrRecoverySuperseded) {
		return err
	}
	// A committed session must not be finished/rolled back after a cleanup or
	// scheduling error. Discover it on the next reconcile via VERIFIED status.
	a, lookupErr := c.Store.AuthAttemptStatusForOwner(ctx, e.AttemptID, AutomaticOwner)
	if lookupErr != nil {
		return lookupErr
	}
	if a.Status == "VERIFIED" {
		return nil
	}
	// Worker RPC unavailable is retried at most once per five seconds, within TTL.
	return nil
}
func (c *Coordinator) catchup(ctx context.Context, e storage.AuthRecoveryEpisode) error {
	delete(c.windows, e.AttemptID)
	delete(c.verifyAfter, e.AttemptID)
	clear(c.liveChallenges)
	if err := c.current(ctx, e, false); err != nil {
		return err
	}
	if e.ReasonCode == "INVALID_CHECKPOINT" {
		return c.transition(ctx, &e, "MANUAL_REQUIRED", "INVALID_CHECKPOINT")
	}
	if e.AttemptID != "" && ctx.Err() == nil {
		_ = c.Browser.Complete(ctx, e.AttemptID)
	}
	run, err := c.Store.GetRecoveryRun(ctx, e.RecoveryRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return c.transition(ctx, &e, "MANUAL_REQUIRED", "CATCHUP_PROGRESS_MISSING")
	}
	if err != nil {
		return err
	}
	if run.ErrorCode == "INVALID_CHECKPOINT" {
		return c.transition(ctx, &e, "MANUAL_REQUIRED", "INVALID_CHECKPOINT")
	}
	if run.Status == storage.RecoveryRunStatusFailed || run.Status == storage.RecoveryRunStatusCanceled {
		return c.transition(ctx, &e, "MANUAL_REQUIRED", "CATCHUP_FAILED")
	}
	if run.RangeFrom == "" || run.RangeTo == "" || run.NextDay == "" {
		return c.transition(ctx, &e, "MANUAL_REQUIRED", "CATCHUP_PROGRESS_MISSING")
	}
	// Worker owns completion plus midnight extension and gate release atomically.
	return nil
}
