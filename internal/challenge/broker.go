package challenge

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image/png"
	"strings"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type Broker struct {
	Store         *storage.Store
	Browser       Browser
	Sender        Sender
	Submitter     Submitter
	Config        Config
	mu            sync.Mutex
	reminded      map[string]bool
	deleted       map[int64]bool
	attempted     map[string]bool
	images        []storage.TelegramAuthAction
	imageMessages []captchaImageMessage
	nextDelivery  time.Time
}

type captchaImageMessage struct {
	id        int64
	promptID  int64
	challenge storage.AuthChallenge
}

func ValidResponse(kind, value string) bool {
	if kind == "OTP" {
		if len(value) < 4 || len(value) > 10 {
			return false
		}
		for i := range len(value) {
			if value[i] < '0' || value[i] > '9' {
				return false
			}
		}
		return true
	}
	if kind != "CAPTCHA_TEXT" || len(value) < 1 || len(value) > 16 {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return false
		}
	}
	return true
}

func validPNG(image []byte) bool {
	if len(image) == 0 || len(image) > 512<<10 || !bytes.HasPrefix(image, []byte("\x89PNG\r\n\x1a\n")) {
		return false
	}
	dimensions, err := png.DecodeConfig(bytes.NewReader(image))
	return err == nil && dimensions.Width > 0 && dimensions.Height > 0 && dimensions.Width <= 4096 && dimensions.Height <= 4096
}

func observationMatches(c storage.AuthChallenge, o authbrowser.AuthObservation) bool {
	if o.Revision != c.BrowserRevision {
		return false
	}
	if c.Kind == "OTP" {
		return o.State == authbrowser.AuthPageState("OTP_REQUIRED")
	}
	return c.Kind == "CAPTCHA_TEXT" && (o.State == authbrowser.AuthPageState("CAPTCHA_REQUIRED") || o.State == authbrowser.AuthPageState("LOGIN_FORM") && o.CaptchaRequired)
}

// Prompt commits an unbound challenge. Only the delivery loop performs browser
// capture or Telegram I/O; callers may hold the bank operation mutex here.
func (b *Broker) Prompt(ctx context.Context, e storage.AuthRecoveryEpisode, observation authbrowser.AuthObservation, kind string) (storage.AuthChallenge, error) {
	if b.Store == nil || b.Config.ChatID == 0 || e.AttemptID == "" {
		return storage.AuthChallenge{}, storage.ErrChallengeMismatch
	}
	c := storage.AuthChallenge{EpisodeID: e.ID, ConnectionID: e.ConnectionID, Generation: e.Generation, AttemptID: e.AttemptID, BrowserRevision: observation.Revision, Kind: kind, ChatID: b.Config.ChatID}
	if !observationMatches(c, observation) {
		return c, storage.ErrChallengeMismatch
	}
	attempt, err := b.Store.AuthAttemptForOwner(ctx, e.AttemptID, storage.RecoveryOwner)
	if err != nil {
		return c, err
	}
	sessionExpiry, err := time.Parse(time.RFC3339Nano, attempt.ExpiresAt)
	if err != nil {
		return c, storage.ErrChallengeExpired
	}
	ttl, capTTL := b.Config.CaptchaTTL, 180*time.Second
	if kind == "OTP" {
		ttl, capTTL = b.Config.OTPTTL, 120*time.Second
	}
	if ttl <= 0 || ttl > capTTL {
		ttl = capTTL
	}
	expires := time.Now().UTC().Add(ttl)
	if sessionExpiry.Before(expires) {
		expires = sessionExpiry
	}
	if !observation.ExpiresAt.IsZero() && observation.ExpiresAt.Before(expires) {
		expires = observation.ExpiresAt
	}
	c.ExpiresAt = expires.Format(time.RFC3339Nano)
	return b.Store.CreateAuthChallenge(ctx, c)
}

// RecoverDelivery must run once before starting coordinator and delivery loops.
// PENDING messages retain their binding and are revalidated by DeliverPending.
func (b *Broker) RecoverDelivery(ctx context.Context) error {
	return b.Store.RecoverAuthChallengeDelivery(ctx)
}

func finiteFence(err error) bool {
	return errors.Is(err, storage.ErrChallengeExpired) || errors.Is(err, storage.ErrChallengeMismatch) || errors.Is(err, storage.ErrRecoverySuperseded) || errors.Is(err, storage.ErrRecoveryConsentRequired) || errors.Is(err, sql.ErrNoRows)
}

func (b *Broker) deletePrompt(ctx context.Context, c storage.AuthChallenge) {
	if c.PromptMessageID == 0 || c.PromptDeletedAt != "" {
		return
	}
	b.mu.Lock()
	deleted := b.deleted[c.PromptMessageID]
	b.mu.Unlock()
	if !deleted {
		if b.Sender.DeleteMessage(ctx, c.ChatID, c.PromptMessageID) != nil {
			return
		}
		b.mu.Lock()
		if b.deleted == nil {
			b.deleted = make(map[int64]bool)
		}
		b.deleted[c.PromptMessageID] = true
		b.mu.Unlock()
	}
	if b.Store.MarkAuthChallengePromptDeleted(ctx, c) != nil {
		return
	}
	b.mu.Lock()
	delete(b.deleted, c.PromptMessageID)
	delete(b.reminded, c.ID)
	b.mu.Unlock()
}

func (b *Broker) invalidate(ctx context.Context, c storage.AuthChallenge, status string) error {
	if err := b.Store.FinishAuthChallenge(ctx, c.ID, status); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.attempted, c.ID)
	b.mu.Unlock()
	b.deletePrompt(ctx, c)
	return nil
}

func (b *Broker) deferDelivery(err error) {
	delay := 30 * time.Second
	var retry interface{ RetryDelay() time.Duration }
	if errors.As(err, &retry) && retry.RetryDelay() > delay {
		delay = retry.RetryDelay()
	}
	b.mu.Lock()
	b.nextDelivery = time.Now().Add(delay)
	b.mu.Unlock()
}

// DeliverPending is called by one loop only. No raw image is retained after I/O.
func (b *Broker) DeliverPending(ctx context.Context) error {
	if b.Store == nil || b.Browser == nil || b.Sender == nil {
		return storage.ErrChallengeMismatch
	}
	challenges, err := b.Store.AuthChallengesForDelivery(ctx)
	if err != nil {
		return err
	}
	for _, c := range challenges {
		if c.ChatID != b.Config.ChatID {
			continue
		}
		b.mu.Lock()
		attempted := b.attempted[c.ID]
		b.mu.Unlock()
		if c.Status == "DELIVERING" && attempted {
			if err := b.invalidate(ctx, c, "INVALIDATED"); err != nil {
				return err
			}
			continue
		}
		if c.Status != "DELIVERING" && c.Status != "PENDING" {
			if c.Status != "CONSUMING" {
				b.deletePrompt(ctx, c)
			}
			continue
		}
		if err := b.Store.ValidateAuthChallenge(ctx, c, time.Now()); err != nil {
			if !finiteFence(err) {
				return err
			}
			status := "INVALIDATED"
			if errors.Is(err, storage.ErrChallengeExpired) {
				status = "EXPIRED"
			}
			if err := b.invalidate(ctx, c, status); err != nil {
				return err
			}
			continue
		}
		b.mu.Lock()
		deferred := time.Now().Before(b.nextDelivery)
		b.mu.Unlock()
		if deferred {
			continue
		}
		o, err := b.Browser.Observe(ctx, c.AttemptID)
		if err != nil {
			if err := b.invalidate(ctx, c, "INVALIDATED"); err != nil {
				return err
			}
			b.deferDelivery(err)
			return errors.New("challenge observation unavailable")
		}
		if !observationMatches(c, o) || !o.ExpiresAt.IsZero() && !time.Now().Before(o.ExpiresAt) {
			if err := b.invalidate(ctx, c, "INVALIDATED"); err != nil {
				return err
			}
			continue
		}
		if c.Status == "PENDING" {
			continue
		}
		var image []byte
		if c.Kind == "CAPTCHA_TEXT" {
			image, err = b.Browser.CaptureCaptcha(ctx, c.AttemptID, c.BrowserRevision)
			if err != nil || !validPNG(image) {
				if finishErr := b.invalidate(ctx, c, "INVALIDATED"); finishErr != nil {
					return finishErr
				}
				return errors.New("UNSAFE_CAPTCHA_CROP")
			}
		}
		// Capture can overlap cancellation or a browser step change.
		if err := b.Store.ValidateAuthChallenge(ctx, c, time.Now()); err != nil {
			if !finiteFence(err) {
				return err
			}
			if err := b.invalidate(ctx, c, "INVALIDATED"); err != nil {
				return err
			}
			continue
		}
		if c.Kind == "CAPTCHA_TEXT" {
			o, err = b.Browser.Observe(ctx, c.AttemptID)
			if err != nil || !observationMatches(c, o) || !o.ExpiresAt.IsZero() && !time.Now().Before(o.ExpiresAt) {
				if finishErr := b.invalidate(ctx, c, "INVALIDATED"); finishErr != nil {
					return finishErr
				}
				continue
			}
		}
		b.mu.Lock()
		if b.attempted == nil {
			b.attempted = make(map[string]bool)
		}
		b.attempted[c.ID] = true
		b.mu.Unlock()
		id, sendErr := b.Sender.SendChallenge(ctx, c, image)
		image = nil
		if sendErr != nil {
			// The transport may already have sent it. Never bind or resend this ID.
			b.deferDelivery(sendErr)
			if id > 0 {
				_ = b.Sender.DeleteMessage(ctx, c.ChatID, id)
			}
			if err := b.invalidate(ctx, c, "INVALIDATED"); err != nil {
				return err
			}
			return sendErr
		}
		if err := b.Store.DeliverAuthChallenge(ctx, c.ID, c.ChatID, id); err != nil {
			_ = b.Sender.DeleteMessage(ctx, c.ChatID, id)
			if finishErr := b.invalidate(ctx, c, "INVALIDATED"); finishErr != nil {
				return finishErr
			}
			return err
		}
		b.mu.Lock()
		delete(b.attempted, c.ID)
		b.mu.Unlock()
	}
	return b.deliverCaptchaImages(ctx)
}

func (b *Broker) HandleReply(ctx context.Context, chatID, promptID, incomingID int64, text string) error {
	if b.Store == nil || b.Browser == nil || b.Sender == nil || b.Submitter == nil || chatID != b.Config.ChatID {
		return storage.ErrChallengeMismatch
	}
	c, err := b.Store.AuthChallengeForPrompt(ctx, chatID, promptID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_ = b.Sender.DeleteMessage(ctx, chatID, incomingID)
		}
		return err
	}
	deleteReply := func() { _ = b.Sender.DeleteMessage(ctx, chatID, incomingID) }
	if c.Status != "PENDING" {
		deleteReply()
		if c.Status != "CONSUMING" {
			b.deletePrompt(ctx, c)
		}
		return storage.ErrChallengeConsumed
	}
	if err := b.Store.ValidateAuthChallenge(ctx, c, time.Now()); err != nil {
		if !finiteFence(err) {
			return err
		}
		status := "INVALIDATED"
		if errors.Is(err, storage.ErrChallengeExpired) {
			status = "EXPIRED"
		}
		if finishErr := b.invalidate(ctx, c, status); finishErr != nil {
			return finishErr
		}
		deleteReply()
		return err
	}
	value := strings.TrimSpace(text)
	if !ValidResponse(c.Kind, value) {
		// Format rejection is a finite disposition; it never submits to ACB.
		deleteReply()
		b.mu.Lock()
		if b.reminded == nil {
			b.reminded = make(map[string]bool)
		}
		first := !b.reminded[c.ID]
		b.reminded[c.ID] = true
		b.mu.Unlock()
		if first {
			_, _ = b.Sender.SendText(ctx, chatID, "Định dạng chưa hợp lệ. Hãy trả lời đúng tin yêu cầu bằng mã trong ảnh hoặc OTP đăng nhập.", nil)
		}
		return ErrInvalidResponse
	}
	observation, err := b.Browser.Observe(ctx, c.AttemptID)
	if err != nil {
		if err := b.invalidate(ctx, c, "INVALIDATED"); err != nil {
			return err
		}
		deleteReply()
		return ErrOutcomeUnknown
	}
	if !observationMatches(c, observation) || !observation.ExpiresAt.IsZero() && !time.Now().Before(observation.ExpiresAt) {
		if err := b.invalidate(ctx, c, "INVALIDATED"); err != nil {
			return err
		}
		deleteReply()
		return storage.ErrChallengeMismatch
	}
	consumed, err := b.Store.ConsumeAuthChallenge(ctx, c.ID, c.Generation, c.BrowserRevision, chatID, promptID, time.Now())
	if err != nil {
		if errors.Is(err, storage.ErrChallengeConsumed) {
			deleteReply()
			return err
		}
		if finiteFence(err) || errors.Is(err, storage.ErrRecoveryBudgetExhausted) {
			if finishErr := b.invalidate(ctx, c, "INVALIDATED"); finishErr != nil {
				return finishErr
			}
			deleteReply()
		}
		return err
	}
	_, submitErr := b.Submitter.SubmitChallenge(ctx, consumed, value)
	value, text = "", ""
	// CONSUMING is already durable: even a failed terminal write must ACK the
	// update, not replay plaintext or call the bank again after restart.
	deleteReply()
	terminal := "CONSUMED"
	if submitErr != nil {
		terminal = "INVALIDATED"
	}
	finishErr := b.Store.FinishAuthChallenge(ctx, c.ID, terminal)
	b.deletePrompt(ctx, c)
	if submitErr != nil || finishErr != nil {
		return ErrOutcomeUnknown
	}
	return nil
}

// RequestCaptchaImage queues IDs only; action admission and binding belong to
// storage/Telegram. Viewing an image neither creates nor consumes a challenge.
func (b *Broker) RequestCaptchaImage(_ context.Context, a storage.TelegramAuthAction) error {
	if a.Action != "CAPTCHA_IMAGE" || a.Status != "CONSUMED" || a.ChatID != b.Config.ChatID || a.AttemptID == "" || a.BrowserRevision == "" {
		return storage.ErrChallengeMismatch
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, pending := range b.images {
		if pending.ID == a.ID {
			return nil
		}
	}
	b.images = append(b.images, a)
	return nil
}

func (b *Broker) deliverCaptchaImages(ctx context.Context) error {
	for i := 0; i < len(b.imageMessages); {
		m := b.imageMessages[i]
		if m.promptID != 0 {
			prompt, err := b.Store.AuthChallengeForPrompt(ctx, b.Config.ChatID, m.promptID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err != nil || prompt.Status != "PENDING" {
				if err := b.Sender.DeleteMessage(ctx, b.Config.ChatID, m.id); err != nil {
					i++
					continue
				}
				b.imageMessages = append(b.imageMessages[:i], b.imageMessages[i+1:]...)
				continue
			}
		}
		err := b.Store.ValidateAuthChallenge(ctx, m.challenge, time.Now())
		if err != nil && !finiteFence(err) {
			return err
		}
		if err == nil {
			o, observeErr := b.Browser.Observe(ctx, m.challenge.AttemptID)
			if observeErr != nil {
				return observeErr
			}
			if observationMatches(m.challenge, o) {
				i++
				continue
			}
		}
		if err := b.Sender.DeleteMessage(ctx, b.Config.ChatID, m.id); err != nil {
			i++
			continue
		}
		b.imageMessages = append(b.imageMessages[:i], b.imageMessages[i+1:]...)
	}
	b.mu.Lock()
	if time.Now().Before(b.nextDelivery) || len(b.images) == 0 {
		b.mu.Unlock()
		return nil
	}
	a := b.images[0]
	b.images = b.images[1:]
	b.mu.Unlock()
	e, err := b.Store.AuthRecoveryEpisode(ctx, a.EpisodeID)
	if err != nil {
		return err
	}
	if e.Generation != a.ExpectedGeneration || e.AttemptID != a.AttemptID || e.ConfigRevision != a.ExpectedConfigRevision || e.FinishedAt != "" {
		_, err = b.Sender.SendText(ctx, b.Config.ChatID, "Captcha đã được xử lý", nil)
		return err
	}
	attempt, err := b.Store.AuthAttemptForOwner(ctx, a.AttemptID, storage.RecoveryOwner)
	if err != nil {
		return err
	}
	c := storage.AuthChallenge{EpisodeID: e.ID, ConnectionID: e.ConnectionID, Generation: e.Generation, AttemptID: e.AttemptID, BrowserRevision: a.BrowserRevision, Kind: "CAPTCHA_TEXT", ChatID: b.Config.ChatID, ExpiresAt: attempt.ExpiresAt}
	if err := b.Store.ValidateAuthChallenge(ctx, c, time.Now()); err != nil {
		if !finiteFence(err) {
			return err
		}
		_, err = b.Sender.SendText(ctx, b.Config.ChatID, "Captcha đã được xử lý", nil)
		return err
	}
	o, err := b.Browser.Observe(ctx, c.AttemptID)
	if err != nil {
		return err
	}
	if !observationMatches(c, o) {
		_, err = b.Sender.SendText(ctx, b.Config.ChatID, "Captcha đã được xử lý", nil)
		return err
	}
	if expiry := time.Now().UTC().Add(180 * time.Second); expiry.Before(mustImageExpiry(c.ExpiresAt)) {
		c.ExpiresAt = expiry.Format(time.RFC3339Nano)
	}
	if !o.ExpiresAt.IsZero() && o.ExpiresAt.Before(mustImageExpiry(c.ExpiresAt)) {
		c.ExpiresAt = o.ExpiresAt.Format(time.RFC3339Nano)
	}
	var promptID int64
	if active, activeErr := b.Store.ActiveAuthChallenge(ctx, c.AttemptID); activeErr == nil {
		if active.Status == "CONSUMING" {
			_, err = b.Sender.SendText(ctx, b.Config.ChatID, "Captcha đã được xử lý", nil)
			return err
		}
		if active.Kind == c.Kind && active.BrowserRevision == c.BrowserRevision {
			if mustImageExpiry(active.ExpiresAt).Before(mustImageExpiry(c.ExpiresAt)) {
				c.ExpiresAt = active.ExpiresAt
			}
			promptID = active.PromptMessageID
		}
	} else if !errors.Is(activeErr, sql.ErrNoRows) {
		return activeErr
	}
	image, err := b.Browser.CaptureCaptcha(ctx, c.AttemptID, c.BrowserRevision)
	if err != nil || !validPNG(image) {
		return errors.New("UNSAFE_CAPTCHA_CROP")
	}
	if fenceErr := b.Store.ValidateAuthChallenge(ctx, c, time.Now()); fenceErr != nil {
		if !finiteFence(fenceErr) {
			return fenceErr
		}
		_, err = b.Sender.SendText(ctx, b.Config.ChatID, "Captcha đã được xử lý", nil)
		return err
	}
	o, err = b.Browser.Observe(ctx, c.AttemptID)
	if err != nil {
		return err
	}
	if !observationMatches(c, o) {
		_, err = b.Sender.SendText(ctx, b.Config.ChatID, "Captcha đã được xử lý", nil)
		return err
	}
	sender, ok := b.Sender.(interface {
		SendCaptchaImage(context.Context, int64, []byte) (int64, error)
	})
	if !ok {
		return errors.New("captcha image delivery unavailable")
	}
	id, err := sender.SendCaptchaImage(ctx, b.Config.ChatID, image)
	image = nil
	if err != nil {
		b.deferDelivery(err)
		if id > 0 {
			_ = b.Sender.DeleteMessage(ctx, b.Config.ChatID, id)
		}
		return err
	}
	if fenceErr := b.Store.ValidateAuthChallenge(ctx, c, time.Now()); fenceErr != nil {
		_ = b.Sender.DeleteMessage(ctx, b.Config.ChatID, id)
		if !finiteFence(fenceErr) {
			return fenceErr
		}
		_, err = b.Sender.SendText(ctx, b.Config.ChatID, "Captcha đã được xử lý", nil)
		return err
	}
	b.imageMessages = append(b.imageMessages, captchaImageMessage{id: id, challenge: c, promptID: promptID})
	return nil
}

func mustImageExpiry(value string) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, value)
	return at
}
