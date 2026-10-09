package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
)

var (
	ErrProviderConfigInvalid     = errors.New("invalid provider configuration")
	ErrProviderChannelLocked     = errors.New("provider channel is locked by issued orders")
	ErrProviderConfigBusy        = errors.New("provider configuration has active operations")
	ErrProviderRevisionConflict  = errors.New("provider configuration revision changed")
	ErrProviderConfigUnavailable = errors.New("provider configuration unavailable")
)

// PaymentProviderCredentials is internal only, and must never be marshalled by an API.
type PaymentProviderCredentials struct {
	ClientID    string `json:"clientId"`
	APIKey      string `json:"apiKey"`
	ChecksumKey string `json:"checksumKey"`
}

type PaymentProviderConfig struct {
	Credentials      PaymentProviderCredentials `json:"-"`
	Revision         int64                      `json:"-"`
	Enabled          bool                       `json:"enabled"`
	WebhookConfirmed bool                       `json:"webhookConfirmed"`
}

type PaymentProviderActor struct{ Subject, Role, RequestID string }

type providerQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type paymentProviderOperationContextKey struct{}

// WithPaymentProviderOperation carries admission to the same transaction gate
// used by order, receipt, inbox and journal writes. Expired work cannot resume
// after a process pause and commit under a replaced configuration.
func WithPaymentProviderOperation(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, paymentProviderOperationContextKey{}, token)
}

func checkPaymentProviderOperation(ctx context.Context, q providerQuery) error {
	token, _ := ctx.Value(paymentProviderOperationContextKey{}).(string)
	if token == "" {
		return nil
	}
	var exists int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM payment_provider_operations WHERE token=? AND lease_until>?`, token, now()).Scan(&exists); err != nil {
		return ErrProviderConfigUnavailable
	}
	if exists != 1 {
		return ErrProviderConfigUnavailable
	}
	return nil
}

func (s *Store) providerConfig(ctx context.Context, q providerQuery) (PaymentProviderConfig, error) {
	var cfg PaymentProviderConfig
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT revision, credentials_envelope, enabled, webhook_confirmed FROM payment_provider_config WHERE id=1`).Scan(&cfg.Revision, &raw, &cfg.Enabled, &cfg.WebhookConfirmed); err != nil {
		return cfg, ErrProviderConfigUnavailable
	}
	if raw == "" {
		return cfg, nil
	}
	var err error
	cfg.Credentials, err = s.decryptProviderCredentials(raw)
	if err != nil {
		return PaymentProviderConfig{}, err
	}
	return cfg, nil
}

func (s *Store) PaymentProviderConfig(ctx context.Context) (PaymentProviderConfig, error) {
	return s.providerConfig(ctx, s.db)
}

func (s *Store) decryptProviderCredentials(raw string) (PaymentProviderCredentials, error) {
	var keys PaymentProviderCredentials
	if s.keyring == nil {
		return keys, ErrProviderConfigUnavailable
	}
	var envelope security.Envelope
	if json.Unmarshal([]byte(raw), &envelope) != nil {
		return keys, ErrProviderConfigUnavailable
	}
	plain, err := s.keyring.Decrypt(envelope, []byte("payment-provider:PAYOS:credentials"))
	if err != nil {
		return keys, ErrProviderConfigUnavailable
	}
	defer clear(plain)
	if json.Unmarshal(plain, &keys) != nil {
		return PaymentProviderCredentials{}, ErrProviderConfigUnavailable
	}
	return keys, nil
}

// Historical credentials are verification-only; never use these to create,
// cancel or reconcile a link. A channel change before issuance is filtered out.
func (s *Store) PaymentProviderVerificationKeys(ctx context.Context, channelID string, revision int64) ([]PaymentProviderCredentials, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT credentials_envelope FROM payment_provider_credential_versions WHERE revision < ? ORDER BY revision DESC`, revision)
	if err != nil {
		return nil, ErrProviderConfigUnavailable
	}
	defer rows.Close()
	var result []PaymentProviderCredentials
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, ErrProviderConfigUnavailable
		}
		keys, err := s.decryptProviderCredentials(raw)
		if err != nil {
			return nil, err
		}
		if keys.ClientID == channelID {
			result = append(result, keys)
		}
	}
	if rows.Err() != nil {
		return nil, ErrProviderConfigUnavailable
	}
	return result, nil
}

// Taking the runtime write lock before reading makes revision checks and operation
// admission atomic across independently opened gateway/worker database handles.
func providerWriteLock(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE payment_provider_runtime SET id=id WHERE id=1`)
	return err
}

func (s *Store) providerAudit(ctx context.Context, tx *sql.Tx, actor PaymentProviderActor, action string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(id,actor_subject,actor_role,action,target,request_id,details_json,created_at) VALUES(?,?,?,?,?,?, '{}',?)`, id("audit"), actor.Subject, actor.Role, action, "PAYOS", actor.RequestID, now())
	return err
}

func (s *Store) SavePaymentProviderConfig(ctx context.Context, keys PaymentProviderCredentials, enabled bool, actor PaymentProviderActor) (PaymentProviderConfig, error) {
	keys.ClientID, keys.APIKey, keys.ChecksumKey = strings.TrimSpace(keys.ClientID), strings.TrimSpace(keys.APIKey), strings.TrimSpace(keys.ChecksumKey)
	if keys.ClientID == "" || len(keys.ClientID) > 512 || len(keys.APIKey) > 4096 || len(keys.ChecksumKey) > 4096 || strings.ContainsAny(keys.ClientID+keys.APIKey+keys.ChecksumKey, "\r\n\x00") {
		return PaymentProviderConfig{}, ErrProviderConfigInvalid
	}
	var result PaymentProviderConfig
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := providerWriteLock(ctx, tx); err != nil {
			return err
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		old, err := s.providerConfig(ctx, tx)
		if err != nil {
			return err
		}
		if keys.ClientID != old.Credentials.ClientID {
			var count int
			// An initial managed save may adopt existing orders only when every issued
			// order already belongs to this exact channel; it never rebinds an order.
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM payment_orders WHERE channel_id<>? OR ?<>''`, keys.ClientID, old.Credentials.ClientID).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return ErrProviderChannelLocked
			}
		} else {
			if keys.APIKey == "" {
				keys.APIKey = old.Credentials.APIKey
			}
			if keys.ChecksumKey == "" {
				keys.ChecksumKey = old.Credentials.ChecksumKey
			}
		}
		if keys.APIKey == "" || keys.ChecksumKey == "" {
			return ErrProviderConfigInvalid
		}
		changed := keys != old.Credentials
		if changed {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM payment_provider_operations WHERE lease_until > ?`, now()).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return ErrProviderConfigBusy
			}
		}
		if s.keyring == nil {
			return ErrProviderConfigUnavailable
		}
		plain, err := json.Marshal(keys)
		if err != nil {
			return ErrProviderConfigUnavailable
		}
		defer clear(plain)
		envelope, err := s.keyring.Encrypt(plain, []byte("payment-provider:PAYOS:credentials"))
		if err != nil {
			return ErrProviderConfigUnavailable
		}
		raw, err := json.Marshal(envelope)
		if err != nil {
			return ErrProviderConfigUnavailable
		}
		result = PaymentProviderConfig{Credentials: keys, Revision: old.Revision + 1, Enabled: enabled, WebhookConfirmed: old.WebhookConfirmed && !changed}
		if changed {
			if _, err := tx.ExecContext(ctx, `INSERT INTO payment_provider_credential_versions(revision,credentials_envelope) VALUES(?,?)`, result.Revision, string(raw)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE payment_provider_config SET revision=?,credentials_envelope=?,enabled=?,webhook_confirmed=?,updated_at=? WHERE id=1`, result.Revision, string(raw), enabled, result.WebhookConfirmed, now()); err != nil {
			return err
		}
		return s.providerAudit(ctx, tx, actor, "payment-provider.config.save")
	})
	return result, err
}

// BeginPaymentProviderOperation pins a credential revision until the complete
// network-and-commit operation ends. Key changes cannot race an admitted writer.
func (s *Store) BeginPaymentProviderOperation(ctx context.Context) (PaymentProviderConfig, string, error) {
	var cfg PaymentProviderConfig
	token, err := randomPaymentToken()
	if err != nil {
		return cfg, "", err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := providerWriteLock(ctx, tx); err != nil {
			return err
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		var paused bool
		if err := tx.QueryRowContext(ctx, `SELECT quiesced FROM payment_provider_runtime WHERE id=1`).Scan(&paused); err != nil {
			return err
		}
		if paused {
			return ErrProviderConfigUnavailable
		}
		var err error
		cfg, err = s.providerConfig(ctx, tx)
		if err != nil {
			return err
		}
		if cfg.Credentials.ClientID == "" {
			return ErrProviderConfigUnavailable
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM payment_provider_operations WHERE lease_until <= ?`, now()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO payment_provider_operations(token,revision,lease_until) VALUES(?,?,?)`, token, cfg.Revision, time.Now().UTC().Add(5*time.Minute).Format(time.RFC3339Nano))
		return err
	})
	return cfg, token, err
}

func (s *Store) RenewPaymentProviderOperation(ctx context.Context, token string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE payment_provider_operations SET lease_until=? WHERE token=? AND lease_until>?`, time.Now().UTC().Add(5*time.Minute).Format(time.RFC3339Nano), token, now())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrProviderConfigUnavailable
	}
	return nil
}
func (s *Store) EndPaymentProviderOperation(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM payment_provider_operations WHERE token=?`, token)
	return err
}

func (s *Store) ConfirmPaymentProviderWebhook(ctx context.Context, revision int64, actor PaymentProviderActor) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := providerWriteLock(ctx, tx); err != nil {
			return err
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE payment_provider_config SET webhook_confirmed=1, updated_at=? WHERE id=1 AND revision=? AND credentials_envelope<>''`, now(), revision)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrProviderRevisionConflict
		}
		return s.providerAudit(ctx, tx, actor, "payment-provider.webhook.confirm")
	})
}

func (s *Store) SetPaymentProviderQuiesced(ctx context.Context, paused bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE payment_provider_runtime SET quiesced=? WHERE id=1`, paused)
	return err
}
func (s *Store) PaymentProviderQuiesced(ctx context.Context) (bool, error) {
	var paused bool
	err := s.db.QueryRowContext(ctx, `SELECT quiesced FROM payment_provider_runtime WHERE id=1`).Scan(&paused)
	return paused, err
}
func (s *Store) PaymentProviderActiveRequests(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM payment_provider_operations WHERE lease_until>?`, now()).Scan(&n)
	return n, err
}

func (s *Store) PaymentProviderRequestSlot(ctx context.Context) (time.Time, error) {
	var at time.Time
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := providerWriteLock(ctx, tx); err != nil {
			return err
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		var raw string
		var paused bool
		if err := tx.QueryRowContext(ctx, `SELECT next_request_at,quiesced FROM payment_provider_runtime WHERE id=1`).Scan(&raw, &paused); err != nil {
			return err
		}
		if paused {
			return ErrProviderConfigUnavailable
		}
		at, _ = time.Parse(time.RFC3339Nano, raw)
		if n := time.Now().UTC(); at.Before(n) {
			at = n
		}
		_, err := tx.ExecContext(ctx, `UPDATE payment_provider_runtime SET next_request_at=? WHERE id=1`, at.Add(time.Second).Format(time.RFC3339Nano))
		return err
	})
	return at, err
}
