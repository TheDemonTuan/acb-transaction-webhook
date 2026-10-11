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

func (s *Service) TelegramEnabled() bool {
	return s.Enabled() && s.cfg.BotID > 0 && s.cfg.ChatID < 0 && s.cfg.SenderBotID > 0 && s.cfg.WebhookSecret != ""
}

func (s *Service) NativeEnabled() bool {
	return s.Enabled() && (s.cfg.NativeMode == ModeObserve || s.cfg.NativeMode == ModeActive) && len(s.cfg.NativeWebhookSecret) >= 32
}

// Authenticate runs before the HTTP layer reads or decodes a request body.
func (s *Service) Authenticate(secret string) bool {
	if !s.TelegramEnabled() {
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
	if !s.TelegramEnabled() || s.store == nil {
		return ErrUnavailable
	}
	message, edited := update.trustedMessage(s.cfg)
	if message == nil {
		return nil
	}
	mode := s.cfg.Mode
	if s.cfg.ActiveTransport == "native" { mode = ModeObserve }
	in := storage.SePayNotificationInput{
		StoreKey: s.cfg.StoreKey, BankCode: s.cfg.BankCode,
		AccountNumber: s.cfg.AccountNumber, Mode: mode,
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
		case notification.AccountNumber != s.cfg.effectiveNotificationAccountNumber() || notification.BankName != s.cfg.BankName:
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

func (s *Service) HandleNativeWebhook(ctx context.Context, hook NativeWebhook) error {
	if s != nil && !s.pinned && s.store != nil {
		snapshot, err := s.Snapshot(ctx)
		if err != nil {
			return err
		}
		return snapshot.HandleNativeWebhook(ctx, hook)
	}
	if !s.NativeEnabled() || s.store == nil {
		return ErrUnavailable
	}
	mode := s.cfg.NativeMode
	if s.cfg.Mode == ModeObserve || s.cfg.ActiveTransport != "native" {
		mode = ModeObserve
	}
	if mode == "" || mode == ModeDisabled {
		return ErrUnavailable
	}
	in := storage.SePayWebhookInput{
		StoreKey:                   s.cfg.StoreKey,
		BankCode:                   s.cfg.BankCode,
		AccountNumber:              s.cfg.AccountNumber,
		Mode:                       mode,
		Gateway:                    hook.Gateway,
		NotificationAccountNumber: hook.AccountNumber,
		SubAccount:                 hook.SubAccount,
		Reference:                  hook.ReferenceCode,
		TransferType:               hook.TransferType,
		ProviderID:                 hook.ID,
		AmountVND:                  hook.TransferAmount,
		ConfigRevision:             s.revision,
		ActivationAt:               s.cfg.ActivationAt,
		TransactionAt:              hook.TransactionAt,
		RawPayload:                 hook.RawPayload,
		NativeReferenceVerified:    s.cfg.NativeReferenceVerified,
	}
	if hook.TransferType != "in" {
		in.ReviewReason = "OUTGOING"
	} else if hook.Gateway != s.cfg.BankName || hook.AccountNumber != s.cfg.effectiveNotificationAccountNumber() || hook.SubAccount != s.cfg.NativeSubAccount {
		in.ReviewReason = ReasonAccountMismatch
	}
	result, err := s.store.IngestSePayWebhook(ctx, in)
	if err != nil {
		return err
	}
	if result.Event != nil && s.onCommit != nil {
		s.onCommit(*result.Event)
	}
	return nil
}
