package payments

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// ExistingOrderIntent validates a retry before HTTP applies new-intent quota.
func (s *Service) ExistingOrderIntent(ctx context.Context, amount int64, origin, key string) (storage.PaymentOrder, bool, error) {
	if amount < 1 || amount > s.cfg.PaymentMaxAmountVND || amount > maxSafeInteger {
		return storage.PaymentOrder{}, false, ErrInvalidAmount
	}
	if origin != "STATIC_URL" && origin != "OPERATOR_DYNAMIC" {
		return storage.PaymentOrder{}, false, ErrInvalidOrigin
	}
	if !canonicalUUID(key) {
		return storage.PaymentOrder{}, false, ErrInvalidIdempotencyKey
	}
	if s.store == nil {
		return storage.PaymentOrder{}, false, ErrPaymentUnavailable
	}
	order, err := s.store.PaymentOrderByKey(ctx, s.cfg.PayOSClientID, key)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.PaymentOrder{}, false, nil
	}
	if err != nil {
		return storage.PaymentOrder{}, false, ErrPaymentUnavailable
	}
	if order.RequestHash != paymentRequestHash(amount, origin) {
		return storage.PaymentOrder{}, false, ErrIdempotencyConflict
	}
	return order, true, nil
}

func (s *Service) Order(ctx context.Context, id string) (storage.PaymentOrder, error) {
	if s.store == nil {
		return storage.PaymentOrder{}, ErrPaymentUnavailable
	}
	order, err := s.store.PaymentOrder(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && order.ChannelID != s.cfg.PayOSClientID) {
		return storage.PaymentOrder{}, ErrPaymentNotFound
	}
	if err != nil {
		return storage.PaymentOrder{}, ErrPaymentUnavailable
	}
	return order, nil
}

func (s *Service) Orders(ctx context.Context, status, cursor string, limit int) (storage.Page[storage.PaymentOrder], error) {
	if s.store == nil {
		return storage.Page[storage.PaymentOrder]{}, ErrPaymentUnavailable
	}
	return s.store.ListPaymentOrders(ctx, storage.PaymentOrderFilter{ChannelID: s.cfg.PayOSClientID, Status: status, Cursor: cursor, Limit: limit})
}

// ConfirmWebhook never takes a browser-supplied URL and does not change deployment flags.
func (s *Service) ConfirmWebhook(ctx context.Context) error {
	provider, ok := s.provider.(interface {
		Confirm(context.Context, string) (string, error)
	})
	if !ok || s.store == nil || s.cfg.PayOSClientID == "" || s.cfg.PayOSAPIKey == "" || s.cfg.PayOSChecksumKey == "" {
		return ErrPaymentUnavailable
	}
	if err := s.beginPaymentRequest(ctx); err != nil {
		return ErrPaymentUnavailable
	}
	defer s.endPaymentRequest()
	requestCtx, cancel := context.WithTimeout(ctx, providerRequestTimeout)
	defer cancel()
	confirmedURL, err := provider.Confirm(requestCtx, s.cfg.PaymentPublicOrigin+"/api/integrations/payos/webhook")
	if err != nil || confirmedURL != s.cfg.PaymentPublicOrigin+"/api/integrations/payos/webhook" {
		return ErrPaymentUnavailable
	}
	return nil
}

// Status contains only operational metadata, never provider credentials or accounts.
type Status struct {
	Provider         string  `json:"provider"`
	Bank             string  `json:"bank"`
	Configured       bool    `json:"configured"`
	Status           string  `json:"status"`
	WebhookConfirmed bool    `json:"webhookConfirmed"`
	LastWebhookAt    *string `json:"lastWebhookAt"`
	LastReconciledAt *string `json:"lastReconciledAt"`
	PendingOrders    int64   `json:"pendingOrders"`
	ReviewCount      int64   `json:"reviewCount"`
}

func (s *Service) Status(ctx context.Context) (Status, error) {
	result := Status{Provider: "PAYOS", Bank: "KienlongBank", Configured: s.cfg.PayOSClientID != "" && s.cfg.PayOSAPIKey != "" && s.cfg.PayOSChecksumKey != "", Status: s.Config().Status, WebhookConfirmed: s.cfg.PayOSWebhookConfirmed}
	if s.store == nil {
		result.Status = "UNAVAILABLE"
		return result, ErrPaymentUnavailable
	}
	activity, err := s.store.PaymentActivity(ctx, s.cfg.PayOSClientID)
	if err != nil {
		return result, ErrPaymentUnavailable
	}
	result.LastWebhookAt, result.LastReconciledAt = activity.LastWebhookAt, activity.LastReconciledAt
	result.PendingOrders, result.ReviewCount = activity.PendingOrders, activity.ReviewCount
	if err := s.store.CheckMutationAllowed(ctx); err != nil {
		result.Status = "UNAVAILABLE"
	}
	s.reconcile.mu.Lock()
	quiesced := s.reconcile.quiesced
	s.reconcile.mu.Unlock()
	if quiesced {
		result.Status = "UNAVAILABLE"
	}
	return result, nil
}

func (s *Service) recordActivity(ctx context.Context, kind string) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = s.store.RecordPaymentActivity(persistCtx, s.cfg.PayOSClientID, kind)
}
