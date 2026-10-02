package challenge

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"strings"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type Broker struct {
	Store     *storage.Store
	Browser   Browser
	Sender    Sender
	Submitter Submitter
	Config    Config
	mu        sync.Mutex
	reminded  map[string]bool
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
func (b *Broker) Prompt(ctx context.Context, e storage.AuthRecoveryEpisode, observation authbrowser.AuthObservation, kind string, image []byte) (storage.AuthChallenge, error) {
	if b.Store == nil || b.Browser == nil || b.Sender == nil || b.Config.ChatID == 0 {
		return storage.AuthChallenge{}, storage.ErrChallengeMismatch
	}
	if kind == "CAPTCHA_TEXT" && !validPNG(image) {
		return storage.AuthChallenge{}, storage.ErrChallengeMismatch
	}
	if kind == "OTP" && len(image) > 0 {
		return storage.AuthChallenge{}, storage.ErrChallengeMismatch
	}
	if kind == "OTP" && observation.State != authbrowser.AuthPageState("OTP_REQUIRED") {
		return storage.AuthChallenge{}, storage.ErrChallengeMismatch
	}
	if kind == "CAPTCHA_TEXT" && observation.State != authbrowser.AuthPageState("CAPTCHA_REQUIRED") && !(observation.State == authbrowser.AuthPageState("LOGIN_FORM") && observation.CaptchaRequired) {
		return storage.AuthChallenge{}, storage.ErrChallengeMismatch
	}
	current, err := b.Store.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		return storage.AuthChallenge{}, err
	}
	if current.Generation != e.Generation || current.AttemptID != e.AttemptID || current.FinishedAt != "" {
		return storage.AuthChallenge{}, storage.ErrRecoverySuperseded
	}
	attempt, err := b.Store.AuthAttemptForOwner(ctx, e.AttemptID, storage.RecoveryOwner)
	if err != nil {
		return storage.AuthChallenge{}, err
	}
	sessionExpiry, err := time.Parse(time.RFC3339Nano, attempt.ExpiresAt)
	if err != nil {
		return storage.AuthChallenge{}, storage.ErrChallengeExpired
	}
	ttl := b.Config.CaptchaTTL
	capTTL := 180 * time.Second
	if kind == "OTP" {
		ttl = b.Config.OTPTTL
		capTTL = 120 * time.Second
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
	challenge, err := b.Store.CreateAuthChallenge(ctx, storage.AuthChallenge{EpisodeID: e.ID, ConnectionID: e.ConnectionID, Generation: e.Generation, AttemptID: e.AttemptID, BrowserRevision: observation.Revision, Kind: kind, ChatID: b.Config.ChatID, ExpiresAt: expires.Format(time.RFC3339Nano)})
	if err != nil {
		return challenge, err
	}
	messageID, err := b.Sender.SendChallenge(ctx, challenge, image)
	if err != nil {
		_ = b.Store.FinishAuthChallenge(ctx, challenge.ID, "INVALIDATED")
		return challenge, errors.New("challenge delivery unavailable")
	}
	if err := b.Store.DeliverAuthChallenge(ctx, challenge.ID, b.Config.ChatID, messageID); err != nil {
		_ = b.Store.FinishAuthChallenge(ctx, challenge.ID, "INVALIDATED")
		_ = b.Sender.DeleteMessage(ctx, b.Config.ChatID, messageID)
		return challenge, err
	}
	challenge.Status = "PENDING"
	challenge.PromptMessageID = messageID
	return challenge, nil
}
func (b *Broker) HandleReply(ctx context.Context, chatID, promptID, incomingID int64, text string) error {
	if b.Store == nil || b.Browser == nil || b.Sender == nil || b.Submitter == nil || chatID != b.Config.ChatID {
		return storage.ErrChallengeMismatch
	}
	c, err := b.Store.AuthChallengeForPrompt(ctx, chatID, promptID)
	if err != nil {
		return err
	}
	if c.Status != "PENDING" {
		return storage.ErrChallengeConsumed
	}
	value := strings.TrimSpace(text)
	if !ValidResponse(c.Kind, value) {
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
		return errors.New("browser observation unavailable")
	}
	if observation.Revision != c.BrowserRevision || c.Kind == "OTP" && observation.State != authbrowser.AuthPageState("OTP_REQUIRED") || c.Kind == "CAPTCHA_TEXT" && observation.State != authbrowser.AuthPageState("CAPTCHA_REQUIRED") && !(observation.State == authbrowser.AuthPageState("LOGIN_FORM") && observation.CaptchaRequired) {
		_ = b.Store.FinishAuthChallenge(ctx, c.ID, "INVALIDATED")
		return storage.ErrChallengeMismatch
	}
	consumed, err := b.Store.ConsumeAuthChallenge(ctx, c.ID, c.Generation, c.BrowserRevision, chatID, promptID, time.Now())
	if err != nil {
		return err
	}
	_, submitErr := b.Submitter.SubmitChallenge(ctx, consumed, value)
	value = ""
	text = ""
	terminal := "CONSUMED"
	if submitErr != nil {
		terminal = "INVALIDATED"
	}
	if err := b.Store.FinishAuthChallenge(ctx, c.ID, terminal); err != nil {
		return err
	}
	_ = b.Sender.DeleteMessage(ctx, chatID, incomingID)
	_ = b.Sender.DeleteMessage(ctx, chatID, promptID)
	b.mu.Lock()
	delete(b.reminded, c.ID)
	b.mu.Unlock()
	if submitErr != nil {
		return ErrOutcomeUnknown
	}
	return nil
}
