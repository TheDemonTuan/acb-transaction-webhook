package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	payos "github.com/payOSHQ/payos-lib-golang/v2"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type providerCache struct {
	sync.Mutex
	revision         int64
	provider         Provider
	channelNamespace string
	initialized      bool
}

// NewManagedService deliberately ignores legacy payOS environment credentials
// and flags. The encrypted database row is the sole runtime authority.
func NewManagedService(cfg config.Config, store *storage.Store, onCommit func(storage.EventNotification)) *Service {
	cfg.PayOSClientID, cfg.PayOSAPIKey, cfg.PayOSChecksumKey = "", "", ""
	cfg.PaymentsEnabled, cfg.PayOSWebhookConfirmed = false, false
	s := NewService(cfg, store, nil, onCommit)
	s.managed = true
	s.providerFactory = func(keys storage.PaymentProviderCredentials) (Provider, error) {
		return NewPayOS(keys.ClientID, keys.APIKey, keys.ChecksumKey)
	}
	s.providerCache = &providerCache{}
	return s
}

func (s *Service) snapshot(saved storage.PaymentProviderConfig) (*Service, error) {
	cfg := s.cfg
	cfg.PayOSClientID, cfg.PayOSAPIKey, cfg.PayOSChecksumKey = saved.Credentials.ClientID, saved.Credentials.APIKey, saved.Credentials.ChecksumKey
	cfg.PaymentsEnabled, cfg.PayOSWebhookConfirmed = saved.Enabled, saved.WebhookConfirmed
	var provider Provider
	var channelNamespace string
	if saved.Credentials.ClientID != "" {
		s.providerCache.Lock()
		if !s.providerCache.initialized || s.providerCache.revision != saved.Revision {
			next, err := s.providerFactory(saved.Credentials)
			if err != nil {
				s.providerCache.Unlock()
				return nil, ErrPaymentUnavailable
			}
			s.providerCache.provider, s.providerCache.revision, s.providerCache.initialized = next, saved.Revision, true
			sum := sha256.Sum256([]byte(saved.Credentials.ClientID))
			s.providerCache.channelNamespace = hex.EncodeToString(sum[:16])
		}
		provider = s.providerCache.provider
		channelNamespace = s.providerCache.channelNamespace
		s.providerCache.Unlock()
	}
	return &Service{cfg: cfg, store: s.store, provider: provider, onCommit: s.onCommit, channelNamespace: channelNamespace, reconcile: s.reconcile, reconcileWake: s.reconcileWake, managedSnapshot: true, providerRevision: saved.Revision, providerFactory: s.providerFactory}, nil
}

func (s *Service) resolve(ctx context.Context) (*Service, error) {
	if s.store == nil {
		return nil, ErrPaymentUnavailable
	}
	saved, err := s.store.PaymentProviderConfig(ctx)
	if err != nil {
		return nil, ErrPaymentUnavailable
	}
	return s.snapshot(saved)
}

// pin retains the same credentials, provider and channel through every network
// call and financial commit, without mutating the shared service. A database
// admission lease prevents key rotation in every process until work has drained.
func (s *Service) pin(ctx context.Context) (*Service, context.Context, func(), error) {
	if s.store == nil {
		return nil, ctx, nil, ErrPaymentUnavailable
	}
	saved, token, err := s.store.BeginPaymentProviderOperation(ctx)
	if err != nil {
		return nil, ctx, nil, ErrPaymentUnavailable
	}
	release := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.store.EndPaymentProviderOperation(cleanup, token)
	}
	operation, err := s.snapshot(saved)
	if err != nil {
		release()
		return nil, ctx, nil, err
	}
	operationCtx, cancel := context.WithCancel(ctx)
	operationCtx = storage.WithPaymentProviderOperation(operationCtx, token)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-operationCtx.Done():
				return
			case <-ticker.C:
				renewal, done := context.WithTimeout(operationCtx, 5*time.Second)
				err := s.store.RenewPaymentProviderOperation(renewal, token)
				done()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return operation, operationCtx, func() { cancel(); <-stopped; release() }, nil
}

type ProviderConfig struct {
	Configured            bool   `json:"configured"`
	ClientID              string `json:"clientId,omitempty"`
	APIKeyConfigured      bool   `json:"apiKeyConfigured"`
	ChecksumKeyConfigured bool   `json:"checksumKeyConfigured"`
	WebhookConfirmed      bool   `json:"webhookConfirmed"`
	Enabled               bool   `json:"enabled"`
	StaticURL             string `json:"staticUrl"`
	WebhookURL            string `json:"webhookUrl"`
}

func (s *Service) safeProviderConfig(saved storage.PaymentProviderConfig) ProviderConfig {
	return ProviderConfig{Configured: saved.Credentials.ClientID != "" && saved.Credentials.APIKey != "" && saved.Credentials.ChecksumKey != "", ClientID: saved.Credentials.ClientID, APIKeyConfigured: saved.Credentials.APIKey != "", ChecksumKeyConfigured: saved.Credentials.ChecksumKey != "", WebhookConfirmed: saved.WebhookConfirmed, Enabled: saved.Enabled, StaticURL: s.cfg.PaymentPublicOrigin + "/pay", WebhookURL: s.cfg.PaymentPublicOrigin + "/api/integrations/payos/webhook"}
}
func (s *Service) ProviderConfig(ctx context.Context) (ProviderConfig, error) {
	saved, err := s.store.PaymentProviderConfig(ctx)
	if err != nil {
		return ProviderConfig{}, ErrPaymentUnavailable
	}
	return s.safeProviderConfig(saved), nil
}
func providerConfigError(err error) error {
	switch {
	case errors.Is(err, storage.ErrProviderConfigInvalid):
		return &ServiceError{Code: "INVALID_PROVIDER_CONFIG", HTTPStatus: http.StatusBadRequest}
	case errors.Is(err, storage.ErrProviderChannelLocked):
		return &ServiceError{Code: "PROVIDER_CHANNEL_LOCKED", HTTPStatus: http.StatusConflict}
	case errors.Is(err, storage.ErrProviderConfigBusy):
		return &ServiceError{Code: "PROVIDER_CONFIG_BUSY", HTTPStatus: http.StatusConflict}
	case errors.Is(err, storage.ErrProviderRevisionConflict):
		return &ServiceError{Code: "PROVIDER_CONFIG_CHANGED", HTTPStatus: http.StatusConflict}
	default:
		return ErrPaymentUnavailable
	}
}
func (s *Service) SaveProviderConfig(ctx context.Context, keys storage.PaymentProviderCredentials, enabled bool, actor storage.PaymentProviderActor) (ProviderConfig, error) {
	saved, err := s.store.SavePaymentProviderConfig(ctx, keys, enabled, actor)
	if err != nil {
		return ProviderConfig{}, providerConfigError(err)
	}
	s.wakeReconciler(ctx)
	return s.safeProviderConfig(saved), nil
}

// ConfirmProviderWebhook pins the current revision; a concurrent flags-only save
// may proceed but cannot let this old response confirm a newer configuration.
func (s *Service) ConfirmProviderWebhook(ctx context.Context, actor storage.PaymentProviderActor) error {
	if !s.managed {
		return s.ConfirmWebhook(ctx)
	}
	operation, operationCtx, done, err := s.pin(ctx)
	if err != nil {
		return err
	}
	defer done()
	if err := operation.ConfirmWebhook(operationCtx); err != nil {
		return err
	}
	return providerConfigErrorOrNil(s.store.ConfirmPaymentProviderWebhook(operationCtx, operation.providerRevision, actor))
}
func providerConfigErrorOrNil(err error) error {
	if err == nil {
		return nil
	}
	return providerConfigError(err)
}

func (s *Service) verifyWebhook(ctx context.Context, body map[string]any) (*payos.WebhookData, error) {
	data, err := s.provider.Verify(ctx, body)
	if !s.managedSnapshot || !errors.Is(err, ErrInvalidSignature) {
		return data, err
	}
	previous, loadErr := s.store.PaymentProviderVerificationKeys(ctx, s.cfg.PayOSClientID, s.providerRevision)
	if loadErr != nil {
		return nil, ErrWebhookUnavailable
	}
	for _, keys := range previous {
		if keys.ChecksumKey == s.cfg.PayOSChecksumKey {
			continue
		}
		provider, createErr := s.providerFactory(keys)
		if createErr != nil {
			return nil, ErrWebhookUnavailable
		}
		verified, verifyErr := provider.Verify(ctx, body)
		if verifyErr == nil {
			return verified, nil
		}
		if !errors.Is(verifyErr, ErrInvalidSignature) {
			return nil, verifyErr
		}
	}
	return nil, err
}
