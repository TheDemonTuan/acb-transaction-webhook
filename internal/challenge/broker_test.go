package challenge

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type fixtureBrowser struct {
	observation authbrowser.AuthObservation
	observeErr  error
	observes    atomic.Int32
	captures    atomic.Int32
	capture     func() ([]byte, error)
}

func (f *fixtureBrowser) Observe(context.Context, string) (authbrowser.AuthObservation, error) {
	f.observes.Add(1)
	return f.observation, f.observeErr
}
func (f *fixtureBrowser) CaptureCaptcha(context.Context, string, string) ([]byte, error) {
	f.captures.Add(1)
	if f.capture != nil {
		return f.capture()
	}
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	return b.Bytes(), nil
}

type fixtureSender struct {
	reminders  atomic.Int32
	sends      atomic.Int32
	imageSends atomic.Int32
	sendErr    error
	onSend     func(storage.AuthChallenge)
	mu         sync.Mutex
	deleted    map[int64]int
	texts      []string
}

func (f *fixtureSender) SendChallenge(_ context.Context, c storage.AuthChallenge, _ []byte) (int64, error) {
	n := f.sends.Add(1)
	if f.onSend != nil {
		f.onSend(c)
	}
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	return 455 + int64(n), nil
}
func (f *fixtureSender) SendCaptchaImage(context.Context, int64, []byte) (int64, error) {
	f.imageSends.Add(1)
	return 678, f.sendErr
}
func (f *fixtureSender) DeleteMessage(_ context.Context, _, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleted == nil {
		f.deleted = make(map[int64]int)
	}
	f.deleted[id]++
	return nil
}
func (f *fixtureSender) wasDeleted(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleted[id] > 0
}
func (f *fixtureSender) SendText(_ context.Context, _ int64, text string, _ any) (int64, error) {
	f.reminders.Add(1)
	f.mu.Lock()
	f.texts = append(f.texts, text)
	f.mu.Unlock()
	return 457, nil
}

type fixtureSubmitter struct {
	submits atomic.Int32
	mu      sync.Mutex
	value   string
	fail    bool
	after   func()
}

func (f *fixtureSubmitter) SubmitChallenge(_ context.Context, _ storage.AuthChallenge, value string) (authbrowser.AuthObservation, error) {
	f.submits.Add(1)
	f.mu.Lock()
	f.value = value
	f.mu.Unlock()
	if f.after != nil {
		f.after()
	}
	if f.fail {
		return authbrowser.AuthObservation{}, errors.New("synthetic uncertain network")
	}
	return authbrowser.AuthObservation{State: "AUTHENTICATED", Revision: "new"}, nil
}

func brokerFixture(t *testing.T, kind string) (*Broker, storage.AuthRecoveryEpisode, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	conn, err := s.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TelegramAuthState(ctx, 1); err != nil {
		t.Fatal(err)
	}
	e, err := s.EnsureAuthRecoveryEpisode(ctx, conn.ID, conn.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES(?,1,X'00','fixture',?)`, conn.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	action, err := s.CreateTelegramAuthAction(ctx, storage.TelegramAuthAction{BotID: 1, ChatID: 123, UserID: 456, EpisodeID: e.ID, ExpectedGeneration: e.Generation, Action: "LOGIN"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverTelegramAuthAction(ctx, action.ID, 100); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConsumeTelegramAuthAction(ctx, action.ID, 1, 123, 456, 100, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); err != nil {
		t.Fatal(err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, page := "WAITING_OTP", authbrowser.AuthPageState("OTP_REQUIRED")
	if kind == "CAPTCHA_TEXT" {
		state, page = "WAITING_CAPTCHA", authbrowser.AuthPageState("CAPTCHA_REQUIRED")
	}
	if err := s.TransitionAuthRecovery(ctx, e.ID, e.Generation, "STARTING", state, ""); err != nil {
		t.Fatal(err)
	}
	browser := &fixtureBrowser{observation: authbrowser.AuthObservation{State: page, Revision: "revision-one", ExpiresAt: time.Now().Add(time.Minute)}}
	return &Broker{Store: s, Browser: browser, Sender: &fixtureSender{}, Submitter: &fixtureSubmitter{}, Config: Config{ChatID: 123, OTPTTL: 30 * time.Second}}, e, path
}

func promptFixture(t *testing.T, b *Broker, e storage.AuthRecoveryEpisode, kind string) storage.AuthChallenge {
	t.Helper()
	ctx := context.Background()
	c, err := b.Prompt(ctx, e, b.Browser.(*fixtureBrowser).observation, kind)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	c, err = b.Store.ActiveAuthChallenge(ctx, e.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func challengeStatus(t *testing.T, b *Broker, id string) string {
	t.Helper()
	var status string
	if err := b.Store.DB().QueryRow(`SELECT status FROM auth_challenges WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestPromptCreateOnlyAndPendingRestartAdoption(t *testing.T) {
	b, e, path := brokerFixture(t, "OTP")
	ctx := context.Background()
	browser, sender := b.Browser.(*fixtureBrowser), b.Sender.(*fixtureSender)
	c, err := b.Prompt(ctx, e, browser.observation, "OTP")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != "DELIVERING" || c.PromptMessageID != 0 || browser.observes.Load() != 0 || browser.captures.Load() != 0 || sender.sends.Load() != 0 {
		t.Fatal("Prompt performed external I/O or bound a message")
	}
	if err := b.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	persisted, err := b.Store.ActiveAuthChallenge(ctx, e.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != "PENDING" || persisted.PromptMessageID != 456 || persisted.ExpiresAt != c.ExpiresAt {
		t.Fatal("delivery lost binding or extended TTL")
	}
	s, err := storage.OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restarted := &Broker{Store: s, Browser: browser, Sender: sender, Config: b.Config}
	if err := restarted.RecoverDelivery(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.sends.Load() != 1 || challengeStatus(t, restarted, c.ID) != "PENDING" {
		t.Fatal("restart resent or invalidated a valid bound prompt")
	}
}

func TestChallengeConsumeAndRestart(t *testing.T) {
	b, e, path := brokerFixture(t, "OTP")
	ctx := context.Background()
	c := promptFixture(t, b, e, "OTP")
	sender, submitter := b.Sender.(*fixtureSender), b.Submitter.(*fixtureSubmitter)
	for range 2 {
		if err := b.HandleReply(ctx, 123, c.PromptMessageID, 999, "12xx"); !errors.Is(err, ErrInvalidResponse) {
			t.Fatal(err)
		}
	}
	if !sender.wasDeleted(999) || sender.reminders.Load() != 1 || submitter.submits.Load() != 0 {
		t.Fatal("format rejection did not dispose reply or submitted it")
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = b.HandleReply(ctx, 123, c.PromptMessageID, 1000, " 001234 ") }()
	}
	wg.Wait()
	if submitter.submits.Load() != 1 || submitter.value != "001234" {
		t.Fatal("OTP repeated or leading zeros lost")
	}
	if !sender.wasDeleted(1000) || !sender.wasDeleted(c.PromptMessageID) {
		t.Fatal("terminal messages retained")
	}
	s, err := storage.OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b.Store = s
	if err := b.HandleReply(ctx, 123, c.PromptMessageID, 1000, "001234"); !errors.Is(err, storage.ErrChallengeConsumed) {
		t.Fatal(err)
	}
	if submitter.submits.Load() != 1 {
		t.Fatal("restart replayed OTP before offset commit")
	}
}

func TestUnboundDeliveryRestartInvalidates(t *testing.T) {
	b, e, _ := brokerFixture(t, "OTP")
	ctx := context.Background()
	c, err := b.Prompt(ctx, e, b.Browser.(*fixtureBrowser).observation, "OTP")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.RecoverDelivery(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if challengeStatus(t, b, c.ID) != "INVALIDATED" || b.Sender.(*fixtureSender).sends.Load() != 0 {
		t.Fatal("replayed unknown startup delivery")
	}
	fresh := promptFixture(t, b, e, "OTP")
	if fresh.ID == c.ID || fresh.Status != "PENDING" {
		t.Fatal("replacement prompt was not independently bound")
	}
}

type retryFixtureError struct{}

func (retryFixtureError) Error() string             { return "synthetic rate limit" }
func (retryFixtureError) RetryDelay() time.Duration { return 90 * time.Second }

func TestDeliveryFailureInvalidatesAndDefersReplacement(t *testing.T) {
	b, e, _ := brokerFixture(t, "OTP")
	ctx := context.Background()
	sender := b.Sender.(*fixtureSender)
	sender.sendErr = retryFixtureError{}
	c, err := b.Prompt(ctx, e, b.Browser.(*fixtureBrowser).observation, "OTP")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := b.DeliverPending(ctx); err == nil {
		t.Fatal("delivery failure hidden")
	}
	if challengeStatus(t, b, c.ID) != "INVALIDATED" || b.nextDelivery.Before(before.Add(90*time.Second)) {
		t.Fatal("ambiguous prompt replayable or retry_after ignored")
	}
	sender.sendErr = nil
	if _, err := b.Prompt(ctx, e, b.Browser.(*fixtureBrowser).observation, "OTP"); err != nil {
		t.Fatal(err)
	}
	if err := b.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.sends.Load() != 1 {
		t.Fatal("replacement sent during backoff")
	}
}

func TestDeliveryDatabaseFailureNeverReplaysSend(t *testing.T) {
	b, e, _ := brokerFixture(t, "OTP")
	ctx := context.Background()
	sender := b.Sender.(*fixtureSender)
	sender.onSend = func(storage.AuthChallenge) {
		if _, err := b.Store.DB().Exec(`CREATE TRIGGER fail_delivery BEFORE UPDATE OF status ON auth_challenges BEGIN SELECT RAISE(ABORT,'synthetic unavailable'); END`); err != nil {
			t.Fatal(err)
		}
	}
	c, err := b.Prompt(ctx, e, b.Browser.(*fixtureBrowser).observation, "OTP")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.DeliverPending(ctx); err == nil {
		t.Fatal("DB failure hidden")
	}
	if challengeStatus(t, b, c.ID) != "DELIVERING" || !sender.wasDeleted(456) {
		t.Fatal("lost CAS message not removed")
	}
	if _, err := b.Store.DB().Exec(`DROP TRIGGER fail_delivery`); err != nil {
		t.Fatal(err)
	}
	sender.onSend = nil
	if err := b.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.sends.Load() != 1 || challengeStatus(t, b, c.ID) != "INVALIDATED" {
		t.Fatal("sent twice after uncommitted delivery outcome")
	}
}

func TestLostDeliveryCASDeletesPrompt(t *testing.T) {
	b, e, _ := brokerFixture(t, "OTP")
	ctx := context.Background()
	sender := b.Sender.(*fixtureSender)
	sender.onSend = func(c storage.AuthChallenge) {
		if err := b.Store.FinishAuthChallenge(ctx, c.ID, "CANCELLED"); err != nil {
			t.Fatal(err)
		}
	}
	c, err := b.Prompt(ctx, e, b.Browser.(*fixtureBrowser).observation, "OTP")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.DeliverPending(ctx); !errors.Is(err, storage.ErrChallengeMismatch) {
		t.Fatal(err)
	}
	if !sender.wasDeleted(456) || challengeStatus(t, b, c.ID) != "CANCELLED" {
		t.Fatal("lost CAS retained orphan prompt or resurrected cancelled challenge")
	}
}

func TestDeliveryRejectsUnsafeCropAndChangedRevision(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsafe", true: "revision_changed"}[changed], func(t *testing.T) {
			b, e, _ := brokerFixture(t, "CAPTCHA_TEXT")
			ctx := context.Background()
			browser := b.Browser.(*fixtureBrowser)
			browser.capture = func() ([]byte, error) {
				if !changed {
					return []byte("full page private content"), nil
				}
				browser.observation.Revision = "new-captcha"
				var buf bytes.Buffer
				_ = png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)))
				return buf.Bytes(), nil
			}
			c, err := b.Prompt(ctx, e, browser.observation, "CAPTCHA_TEXT")
			if err != nil {
				t.Fatal(err)
			}
			err = b.DeliverPending(ctx)
			if !changed && (err == nil || err.Error() != "UNSAFE_CAPTCHA_CROP") {
				t.Fatal(err)
			}
			if changed && err != nil {
				t.Fatal(err)
			}
			if b.Sender.(*fixtureSender).sends.Load() != 0 || challengeStatus(t, b, c.ID) != "INVALIDATED" {
				t.Fatal("unsafe or stale image sent")
			}
		})
	}
}

func TestReplyObserveFailureRequiresDurableInvalidation(t *testing.T) {
	b, e, _ := brokerFixture(t, "OTP")
	ctx := context.Background()
	c := promptFixture(t, b, e, "OTP")
	b.Browser.(*fixtureBrowser).observeErr = errors.New("synthetic observe failure")
	if _, err := b.Store.DB().Exec(`CREATE TRIGGER fail_invalidation BEFORE UPDATE OF status ON auth_challenges WHEN NEW.status='INVALIDATED' BEGIN SELECT RAISE(ABORT,'synthetic unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleReply(ctx, 123, c.PromptMessageID, 999, "001234"); err == nil || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal("failed disposition ACKed")
	}
	if b.Sender.(*fixtureSender).wasDeleted(999) || challengeStatus(t, b, c.ID) != "PENDING" {
		t.Fatal("reply lost before durable disposition")
	}
	if _, err := b.Store.DB().Exec(`DROP TRIGGER fail_invalidation`); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleReply(ctx, 123, c.PromptMessageID, 999, "001234"); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	if !b.Sender.(*fixtureSender).wasDeleted(999) || !b.Sender.(*fixtureSender).wasDeleted(c.PromptMessageID) || challengeStatus(t, b, c.ID) != "INVALIDATED" || b.Submitter.(*fixtureSubmitter).submits.Load() != 0 {
		t.Fatal("unknown observation not safely disposed")
	}
}

func TestReplyDatabaseFailureBeforeReserveKeepsMessage(t *testing.T) {
	b, e, _ := brokerFixture(t, "OTP")
	ctx := context.Background()
	c := promptFixture(t, b, e, "OTP")
	if _, err := b.Store.DB().Exec(`CREATE TRIGGER fail_reserve BEFORE UPDATE OF status ON auth_challenges WHEN NEW.status='CONSUMING' BEGIN SELECT RAISE(ABORT,'synthetic unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleReply(ctx, 123, c.PromptMessageID, 999, "001234"); err == nil {
		t.Fatal("reserve failure hidden")
	}
	if b.Sender.(*fixtureSender).wasDeleted(999) || b.Submitter.(*fixtureSubmitter).submits.Load() != 0 {
		t.Fatal("reply deleted or bank called before reservation committed")
	}
	if _, err := b.Store.DB().Exec(`DROP TRIGGER fail_reserve`); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleReply(ctx, 123, c.PromptMessageID, 999, "001234"); err != nil {
		t.Fatal(err)
	}
	current, err := b.Store.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || current.OTPSubmissions != 1 || b.Submitter.(*fixtureSubmitter).submits.Load() != 1 {
		t.Fatal("failed reservation spent budget or retry did not submit once")
	}
}

func TestReplyDatabaseFailureAfterReserveNeverReplays(t *testing.T) {
	b, e, path := brokerFixture(t, "OTP")
	ctx := context.Background()
	c := promptFixture(t, b, e, "OTP")
	submitter := b.Submitter.(*fixtureSubmitter)
	submitter.after = func() {
		if _, err := b.Store.DB().Exec(`CREATE TRIGGER fail_finish BEFORE UPDATE OF status ON auth_challenges BEGIN SELECT RAISE(ABORT,'synthetic unavailable'); END`); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.HandleReply(ctx, 123, c.PromptMessageID, 999, "001234"); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	if challengeStatus(t, b, c.ID) != "CONSUMING" || !b.Sender.(*fixtureSender).wasDeleted(999) {
		t.Fatal("durable consuming not ACKable")
	}
	s, err := storage.OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b.Store = s
	if err := b.HandleReply(ctx, 123, c.PromptMessageID, 999, "001234"); !errors.Is(err, storage.ErrChallengeConsumed) {
		t.Fatal(err)
	}
	if submitter.submits.Load() != 1 {
		t.Fatal("crash between consume/delete/offset replayed OTP")
	}
}

func TestExpiredAndCancelledPromptsAreRemoved(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "expired"}[expired], func(t *testing.T) {
			b, e, _ := brokerFixture(t, "OTP")
			ctx := context.Background()
			c := promptFixture(t, b, e, "OTP")
			if expired {
				if _, err := b.Store.DB().Exec(`UPDATE auth_challenges SET expires_at=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), c.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := b.Store.FinishAuthChallenge(ctx, c.ID, "CANCELLED"); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.DeliverPending(ctx); err != nil {
				t.Fatal(err)
			}
			if !b.Sender.(*fixtureSender).wasDeleted(c.PromptMessageID) {
				t.Fatal("terminal prompt retained")
			}
			if expired && challengeStatus(t, b, c.ID) != "EXPIRED" {
				t.Fatal("expiry not durable")
			}
			if err := b.HandleReply(ctx, 123, c.PromptMessageID, 999, "001234"); !errors.Is(err, storage.ErrChallengeConsumed) {
				t.Fatal(err)
			}
			if !b.Sender.(*fixtureSender).wasDeleted(999) || b.Submitter.(*fixtureSubmitter).submits.Load() != 0 {
				t.Fatal("terminal reply submitted or retained")
			}
		})
	}
}

func TestCleanupAcknowledgementFailureDoesNotRepeatDeletion(t *testing.T) {
	b, e, _ := brokerFixture(t, "OTP")
	ctx := context.Background()
	c := promptFixture(t, b, e, "OTP")
	if err := b.Store.FinishAuthChallenge(ctx, c.ID, "CANCELLED"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Store.DB().Exec(`CREATE TRIGGER fail_cleanup BEFORE UPDATE OF prompt_deleted_at ON auth_challenges BEGIN SELECT RAISE(ABORT,'synthetic unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := b.DeliverPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	sender := b.Sender.(*fixtureSender)
	if sender.deleted[c.PromptMessageID] != 1 {
		t.Fatal("cleanup DB failure repeated external deletion")
	}
	if _, err := b.Store.DB().Exec(`DROP TRIGGER fail_cleanup`); err != nil {
		t.Fatal(err)
	}
	if err := b.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := &Broker{Store: b.Store, Browser: b.Browser, Sender: sender, Config: b.Config}
	if err := restarted.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.deleted[c.PromptMessageID] != 1 {
		t.Fatal("restart retried durably acknowledged cleanup")
	}
	persisted, err := b.Store.AuthChallengeForPrompt(ctx, 123, c.PromptMessageID)
	if err != nil || persisted.PromptDeletedAt == "" || persisted.Status != "CANCELLED" {
		t.Fatal("cleanup lost terminal reply correlation")
	}
}

func TestExpiredReplyIsDurablyDisposedBeforeDeletion(t *testing.T) {
	b, e, _ := brokerFixture(t, "OTP")
	ctx := context.Background()
	c := promptFixture(t, b, e, "OTP")
	if _, err := b.Store.DB().Exec(`UPDATE auth_challenges SET expires_at=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), c.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleReply(ctx, 123, c.PromptMessageID, 999, "001234"); !errors.Is(err, storage.ErrChallengeExpired) {
		t.Fatal(err)
	}
	if challengeStatus(t, b, c.ID) != "EXPIRED" || !b.Sender.(*fixtureSender).wasDeleted(999) || !b.Sender.(*fixtureSender).wasDeleted(c.PromptMessageID) || b.Submitter.(*fixtureSubmitter).submits.Load() != 0 {
		t.Fatal("expired reply not durably disposed")
	}
}

func TestCaptchaImageQueueDoesNotConsumeChallenge(t *testing.T) {
	b, e, _ := brokerFixture(t, "CAPTCHA_TEXT")
	ctx := context.Background()
	c := promptFixture(t, b, e, "CAPTCHA_TEXT")
	a := storage.TelegramAuthAction{ID: "image-one", Action: "CAPTCHA_IMAGE", Status: "CONSUMED", ChatID: 123, EpisodeID: e.ID, ExpectedGeneration: e.Generation, ExpectedConfigRevision: e.ConfigRevision, AttemptID: e.AttemptID, BrowserRevision: c.BrowserRevision}
	browser, sender := b.Browser.(*fixtureBrowser), b.Sender.(*fixtureSender)
	captures := browser.captures.Load()
	if err := b.RequestCaptchaImage(ctx, a); err != nil {
		t.Fatal(err)
	}
	if browser.captures.Load() != captures || sender.imageSends.Load() != 0 {
		t.Fatal("image request performed I/O outside delivery loop")
	}
	if err := b.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.imageSends.Load() != 1 || challengeStatus(t, b, c.ID) != "PENDING" {
		t.Fatal("image view consumed or replaced challenge")
	}
	current, err := b.Store.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || current.CaptchaSubmissions != 0 {
		t.Fatal("image view spent bank submission budget")
	}
	browser.observation = authbrowser.AuthObservation{State: "OTP_REQUIRED", Revision: "otp-next"}
	if err := b.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if !sender.wasDeleted(678) {
		t.Fatal("image retained after captcha changed")
	}
}

func TestCaptchaImageRejectsMovedRevisionAndUnsafePNG(t *testing.T) {
	for _, mode := range []string{"moved", "unsafe", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			b, e, _ := brokerFixture(t, "CAPTCHA_TEXT")
			ctx := context.Background()
			browser, sender := b.Browser.(*fixtureBrowser), b.Sender.(*fixtureSender)
			a := storage.TelegramAuthAction{ID: "image-one", Action: "CAPTCHA_IMAGE", Status: "CONSUMED", ChatID: 123, EpisodeID: e.ID, ExpectedGeneration: e.Generation, ExpectedConfigRevision: e.ConfigRevision, AttemptID: e.AttemptID, BrowserRevision: browser.observation.Revision}
			switch mode {
			case "unsafe":
				browser.capture = func() ([]byte, error) { return []byte("private full page"), nil }
			case "oversized":
				browser.capture = func() ([]byte, error) {
					var buf bytes.Buffer
					_ = png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)))
					return append(buf.Bytes(), make([]byte, (512<<10)+1-buf.Len())...), nil
				}
			case "moved":
				browser.observation.Revision = "new-captcha"
			}
			if err := b.RequestCaptchaImage(ctx, a); err != nil {
				t.Fatal(err)
			}
			err := b.DeliverPending(ctx)
			if mode != "moved" && (err == nil || err.Error() != "UNSAFE_CAPTCHA_CROP") {
				t.Fatal(err)
			}
			if mode == "moved" && (err != nil || sender.reminders.Load() != 1 || browser.captures.Load() != 0) {
				t.Fatal("moved captcha captured or not explained")
			}
			if sender.imageSends.Load() != 0 {
				t.Fatal("unsafe or moved image sent")
			}
			var count int
			if err := b.Store.DB().QueryRow(`SELECT count(*) FROM auth_challenges`).Scan(&count); err != nil || count != 0 {
				t.Fatal("image view created a challenge")
			}
		})
	}
}

func TestChallengeFormatPreservesCaseAndLeadingZeros(t *testing.T) {
	for _, tc := range []struct {
		kind, value string
		valid       bool
	}{{"OTP", "001234", true}, {"OTP", "１２３４", false}, {"OTP", "123", false}, {"OTP", "12345678901", false}, {"CAPTCHA_TEXT", "Ab12CD", true}, {"CAPTCHA_TEXT", "A B", false}, {"CAPTCHA_TEXT", "<script>", false}, {"CAPTCHA_TEXT", "", false}} {
		if got := ValidResponse(tc.kind, tc.value); got != tc.valid {
			t.Errorf("kind=%s validity=%t", tc.kind, got)
		}
	}
}

type receiptSender struct {
	*fixtureSender
	received chan string
}

func (s *receiptSender) SendText(_ context.Context, _ int64, text string, _ any) (int64, error) {
	s.received <- text
	return 457, nil
}

func TestAcceptedReplyAcknowledgesBeforeWaitingForBank(t *testing.T) {
	for _, kind := range []string{"CAPTCHA_TEXT", "OTP"} {
		t.Run(kind, func(t *testing.T) {
			b, e, _ := brokerFixture(t, kind)
			c := promptFixture(t, b, e, kind)
			sender := &receiptSender{fixtureSender: b.Sender.(*fixtureSender), received: make(chan string, 4)}
			b.Sender = sender
			entered, release := make(chan struct{}), make(chan struct{})
			b.Submitter.(*fixtureSubmitter).after = func() { close(entered); <-release }
			done := make(chan error, 1)
			go func() { done <- b.HandleReply(context.Background(), 123, c.PromptMessageID, 1000, "001234") }()
			defer func() { close(release); <-done }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("bank submission did not start")
			}
			select {
			case text := <-sender.received:
				if strings.Contains(text, "001234") || challengeStatus(t, b, c.ID) != "CONSUMING" {
					t.Fatal("receipt exposed the code or preceded durable consumption")
				}
			default:
				t.Fatal("accepted reply was silent while waiting for the bank")
			}
		})
	}
}

func assertOTPChallengeState(t *testing.T, b *Broker, e storage.AuthRecoveryEpisode, c storage.AuthChallenge, status string, submissions int) {
	t.Helper()
	current, err := b.Store.AuthRecoveryEpisode(context.Background(), e.ID)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := b.Store.AuthChallengeForPrompt(context.Background(), c.ChatID, c.PromptMessageID)
	if err != nil {
		t.Fatal(err)
	}
	if current.OTPSubmissions != submissions || persisted.Status != status || (persisted.ConsumedAt != "") != (submissions > 0) {
		t.Fatal("durable OTP consumption or submission budget changed unexpectedly")
	}
	if persisted.BrowserRevision != c.BrowserRevision || persisted.ExpiresAt != c.ExpiresAt || persisted.PromptMessageID != c.PromptMessageID {
		t.Fatal("OTP correction changed the revision, expiry, or prompt binding")
	}
}

func TestObservedOTPLengthCorrectionKeepsPromptAndConsumesOnce(t *testing.T) {
	b, e, path := brokerFixture(t, "OTP")
	ctx := context.Background()
	browser := b.Browser.(*fixtureBrowser)
	browser.observation.OTPLength = 6
	c := promptFixture(t, b, e, "OTP")
	sender, submitter := b.Sender.(*fixtureSender), b.Submitter.(*fixtureSubmitter)
	for i, value := range []string{"0023", "00123", "0012345", "12xx"} {
		incomingID := int64(1000 + i)
		err := b.HandleReply(ctx, 123, c.PromptMessageID, incomingID, value)
		if !errors.Is(err, ErrInvalidResponse) || strings.Contains(err.Error(), value) {
			t.Fatal("length rejection was not a private format correction")
		}
		assertOTPChallengeState(t, b, e, c, "PENDING", 0)
		if !sender.wasDeleted(incomingID) || sender.wasDeleted(c.PromptMessageID) || submitter.submits.Load() != 0 {
			t.Fatal("length rejection retained the reply, destroyed the prompt, or submitted to the bank")
		}
	}
	if sender.reminders.Load() != 1 || len(sender.texts) != 1 {
		t.Fatal("format corrections repeated the prompt reminder")
	}
	submitter.after = func() { assertOTPChallengeState(t, b, e, c, "CONSUMING", 1) }
	if err := b.HandleReply(ctx, 123, c.PromptMessageID, 1010, " 001234 "); err != nil {
		t.Fatal(err)
	}
	assertOTPChallengeState(t, b, e, c, "CONSUMED", 1)
	if submitter.submits.Load() != 1 || !sender.wasDeleted(1010) || !sender.wasDeleted(c.PromptMessageID) {
		t.Fatal("corrected OTP did not submit once and dispose terminal messages")
	}
	for _, text := range sender.texts {
		for _, value := range []string{"0023", "00123", "0012345", "12xx", "001234"} {
			if strings.Contains(text, value) {
				t.Fatal("OTP correction or receipt exposed a private code")
			}
		}
	}
	s, err := storage.OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restarted := &Broker{Store: s, Browser: browser, Sender: sender, Submitter: submitter, Config: b.Config}
	if err := restarted.HandleReply(ctx, 123, c.PromptMessageID, 1011, "001234"); !errors.Is(err, storage.ErrChallengeConsumed) {
		t.Fatal(err)
	}
	assertOTPChallengeState(t, restarted, e, c, "CONSUMED", 1)
	if submitter.submits.Load() != 1 || !sender.wasDeleted(1011) {
		t.Fatal("restart replayed the corrected OTP or retained the replay")
	}
}

func TestObservedOTPLengthSharesGenericFormatReminder(t *testing.T) {
	b, e, _ := brokerFixture(t, "OTP")
	b.Browser.(*fixtureBrowser).observation.OTPLength = 6
	c := promptFixture(t, b, e, "OTP")
	for i, value := range []string{"12xx", "00123"} {
		if err := b.HandleReply(context.Background(), 123, c.PromptMessageID, int64(1000+i), value); !errors.Is(err, ErrInvalidResponse) {
			t.Fatal(err)
		}
	}
	assertOTPChallengeState(t, b, e, c, "PENDING", 0)
	if b.Sender.(*fixtureSender).reminders.Load() != 1 || b.Submitter.(*fixtureSubmitter).submits.Load() != 0 {
		t.Fatal("generic and observed-length errors used separate reminder or submission paths")
	}
}

func TestObservedOTPLengthUsesCurrentMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		length int
		value  string
	}{{"generic_short", 0, "0023"}, {"generic_long", 0, "0012345678"}, {"exact_four", 4, "0023"}, {"exact_ten", 10, "0012345678"}} {
		t.Run(tc.name, func(t *testing.T) {
			b, e, _ := brokerFixture(t, "OTP")
			browser := b.Browser.(*fixtureBrowser)
			browser.observation.OTPLength = 6
			c := promptFixture(t, b, e, "OTP")
			browser.observation.OTPLength = tc.length
			if err := b.HandleReply(context.Background(), 123, c.PromptMessageID, 1000, tc.value); err != nil {
				t.Fatal(err)
			}
			assertOTPChallengeState(t, b, e, c, "CONSUMED", 1)
			if b.Submitter.(*fixtureSubmitter).submits.Load() != 1 {
				t.Fatal("current observed length did not control the single submission")
			}
		})
	}
}

func TestObservedOTPLengthRejectsMalformedMetadata(t *testing.T) {
	for _, length := range []int{-1, 11} {
		b, e, _ := brokerFixture(t, "OTP")
		c := promptFixture(t, b, e, "OTP")
		b.Browser.(*fixtureBrowser).observation.OTPLength = length
		if err := b.HandleReply(context.Background(), 123, c.PromptMessageID, 1000, "001234"); !errors.Is(err, storage.ErrChallengeMismatch) {
			t.Fatal(err)
		}
		assertOTPChallengeState(t, b, e, c, "INVALIDATED", 0)
		sender := b.Sender.(*fixtureSender)
		if b.Submitter.(*fixtureSubmitter).submits.Load() != 0 || !sender.wasDeleted(1000) || !sender.wasDeleted(c.PromptMessageID) {
			t.Fatal("malformed OTP metadata fell back to a bank submission or retained private messages")
		}
	}
}

func TestObservedOTPLengthCannotBypassReplyFences(t *testing.T) {
	for _, mode := range []string{"revision", "page_expired", "prompt_expired"} {
		t.Run(mode, func(t *testing.T) {
			b, e, _ := brokerFixture(t, "OTP")
			ctx := context.Background()
			browser := b.Browser.(*fixtureBrowser)
			browser.observation.OTPLength = 6
			c := promptFixture(t, b, e, "OTP")
			wantErr, wantStatus := storage.ErrChallengeMismatch, "INVALIDATED"
			switch mode {
			case "revision":
				browser.observation.Revision = "next-otp"
			case "page_expired":
				browser.observation.ExpiresAt = time.Now().Add(-time.Second)
			case "prompt_expired":
				c.ExpiresAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
				if _, err := b.Store.DB().Exec(`UPDATE auth_challenges SET expires_at=? WHERE id=?`, c.ExpiresAt, c.ID); err != nil {
					t.Fatal(err)
				}
				wantErr, wantStatus = storage.ErrChallengeExpired, "EXPIRED"
			}
			if err := b.HandleReply(ctx, 123, c.PromptMessageID, 1000, "00123"); !errors.Is(err, wantErr) {
				t.Fatal(err)
			}
			assertOTPChallengeState(t, b, e, c, wantStatus, 0)
			sender := b.Sender.(*fixtureSender)
			if sender.reminders.Load() != 0 || !sender.wasDeleted(1000) || !sender.wasDeleted(c.PromptMessageID) || b.Submitter.(*fixtureSubmitter).submits.Load() != 0 {
				t.Fatal("length correction revived a fenced prompt or submitted a stale OTP")
			}
		})
	}
}
