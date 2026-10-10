package sepay

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

var ErrUnavailable = errors.New("sepay service unavailable")

// Service authenticates the configured source and delegates the complete
// inbox/receipt/money/event transaction to storage. It never settles payOS.
type Service struct {
	cfg      Config
	store    *storage.Store
	onCommit func(storage.EventNotification)
	revision int64
	pinned   bool
	token    string
	fields   *AdminFields
	telegram *TelegramClient
}

func NewService(cfg Config, store *storage.Store, onCommit func(storage.EventNotification)) *Service {
	return &Service{cfg: cfg, store: store, onCommit: onCommit}
}

// Config returns a value copy; callers must project an explicit public DTO.
func (s *Service) Config() Config {
	if s == nil {
		return Config{Mode: ModeDisabled}
	}
	return s.cfg
}

func (s *Service) Enabled() bool {
	return s != nil && (s.cfg.Mode == ModeObserve || s.cfg.Mode == ModeActive)
}

// Authenticate runs before the HTTP layer reads or decodes a request body.
func (s *Service) Authenticate(secret string) bool {
	if !s.Enabled() || s.cfg.WebhookSecret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(s.cfg.WebhookSecret)) == 1
}

// HandleUpdate acknowledges unsupported/untrusted updates without persisting
// them. Trusted accepted, review and duplicate messages return only after the
// durable ingest transaction completes. Commit hints are never sent on failure.
func (s *Service) HandleUpdate(ctx context.Context, update TelegramUpdate) error {
	if s != nil && !s.pinned && s.store != nil {
		snapshot, err := s.Snapshot(ctx)
		if err != nil {
			return err
		}
		return snapshot.HandleUpdate(ctx, update)
	}
	if !s.Enabled() || s.store == nil {
		return ErrUnavailable
	}
	message, edited := update.trustedMessage(s.cfg)
	if message == nil {
		return nil
	}
	in := storage.SePayNotificationInput{
		StoreKey: s.cfg.StoreKey, BankCode: s.cfg.BankCode,
		AccountNumber: s.cfg.AccountNumber, Mode: s.cfg.Mode,
		BotID: s.cfg.BotID, UpdateID: update.UpdateID,
		ChatID: message.Chat.ID, MessageID: message.MessageID,
		ActivationAt: s.cfg.ActivationAt, MessageAt: time.Unix(message.Date, 0).UTC(),
		RawPayload:     update.RawPayload,
		ConfigRevision: s.revision,
	}
	if edited {
		in.ReviewReason = ReasonEditedMessage
	} else {
		notification, err := ParseNotification(message.Text)
		switch {
		case err != nil:
			in.ReviewReason = notificationReviewReason(err)
		case notification.AccountNumber != s.cfg.AccountNumber || notification.BankName != s.cfg.BankName:
			in.ReviewReason = ReasonAccountMismatch
		default:
			in.Credit = &storage.SePayCredit{
				AmountVND: notification.AmountVND, Reference: notification.Reference,
				TransactionAt: notification.TransactionAt,
			}
		}
	}
	result, err := s.store.IngestSePayNotification(ctx, in)
	if err != nil {
		return err
	}
	if result.Event != nil && s.onCommit != nil {
		s.onCommit(*result.Event)
	}
	return nil
}
