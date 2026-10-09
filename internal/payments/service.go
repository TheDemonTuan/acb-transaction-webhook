package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// Service owns payment orchestration; financial commits belong to storage.
// Provider calls must never execute inside a SQLite transaction.
type Service struct {
	cfg              config.Config
	store            *storage.Store
	provider         Provider
	onCommit         func(storage.EventNotification)
	channelNamespace string
	reconcile        *reconcileState
	managed          bool
	managedSnapshot  bool
	providerRevision int64
	providerFactory  func(storage.PaymentProviderCredentials) (Provider, error)
	providerCache    *providerCache
	reconcileWake    func(context.Context) error
}

func NewService(cfg config.Config, store *storage.Store, provider Provider, onCommit func(storage.EventNotification)) *Service {
	sum := sha256.Sum256([]byte(cfg.PayOSClientID))
	return &Service{cfg: cfg, store: store, provider: provider, onCommit: onCommit, channelNamespace: hex.EncodeToString(sum[:16]), reconcile: &reconcileState{}}
}

// Config reports operational readiness without exposing provider credentials.
type Config struct {
	Provider     string `json:"provider"`
	Bank         string `json:"bank"`
	StaticURL    string `json:"staticUrl"`
	MinAmountVND int64  `json:"minAmountVnd"`
	MaxAmountVND int64  `json:"maxAmountVnd"`
	Ready        bool   `json:"ready"`
	Status       string `json:"status"`
}

func (s *Service) Config() Config {
	if s.managed {
		snapshot, err := s.resolve(context.Background())
		if err == nil {
			return snapshot.Config()
		}
		return Config{Provider: "PAYOS", Bank: "KienlongBank", StaticURL: s.cfg.PaymentPublicOrigin + "/pay", MinAmountVND: 1, MaxAmountVND: s.cfg.PaymentMaxAmountVND, Status: "UNAVAILABLE"}
	}
	status := "READY"
	switch {
	case s.cfg.PayOSClientID == "" || s.cfg.PayOSAPIKey == "" || s.cfg.PayOSChecksumKey == "":
		status = "UNCONFIGURED"
	case !s.cfg.PaymentsEnabled:
		status = "DISABLED"
	case !s.cfg.PayOSWebhookConfirmed:
		status = "WEBHOOK_UNCONFIRMED"
	case s.provider == nil:
		status = "UNAVAILABLE"
	}
	return Config{Provider: "PAYOS", Bank: "KienlongBank", StaticURL: s.cfg.PaymentPublicOrigin + "/pay", MinAmountVND: 1, MaxAmountVND: s.cfg.PaymentMaxAmountVND, Ready: status == "READY", Status: status}
}
