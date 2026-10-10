package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

var (
	ErrSePayConfigUnavailable = errors.New("SePay configuration unavailable")
	ErrSePayRevisionConflict  = errors.New("SePay configuration revision changed")
)

// SePayManagedConfig is internal only. All fields, including receiver and token,
// are encrypted together so a reader observes one coherent configuration.
type SePayManagedConfig struct {
	Revision   int64           `json:"-"`
	ConfigJSON json.RawMessage `json:"-"`
	BotToken   string          `json:"-"`
}

type sepayManagedPayload struct {
	ConfigJSON json.RawMessage `json:"config"`
	BotToken   string          `json:"botToken"`
}

func (s *Store) SePayManagedConfig(ctx context.Context) (SePayManagedConfig, bool, error) {
	var result SePayManagedConfig
	var blob []byte
	err := s.db.QueryRowContext(ctx, `SELECT revision,config_envelope FROM sepay_managed_config WHERE id=1`).Scan(&result.Revision, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil || s.keyring == nil {
		return SePayManagedConfig{}, false, ErrSePayConfigUnavailable
	}
	plain, err := s.sepayDecrypt(blob, []byte("sepay-managed-config:v1"))
	if err != nil {
		return SePayManagedConfig{}, false, ErrSePayConfigUnavailable
	}
	defer clear(plain)
	var payload sepayManagedPayload
	if json.Unmarshal(plain, &payload) != nil {
		return SePayManagedConfig{}, false, ErrSePayConfigUnavailable
	}
	result.ConfigJSON, result.BotToken = payload.ConfigJSON, payload.BotToken
	return result, true, nil
}

// SaveSePayManagedConfig serializes with every ingest writer before checking CAS.
// Runtime readers never cache the managed record across requests/processes.
func (s *Store) SaveSePayManagedConfig(ctx context.Context, cfg SePayManagedConfig, storeKey, bank, account string, actor PaymentProviderActor) (int64, error) {
	if s.keyring == nil {
		return 0, ErrSePayConfigUnavailable
	}
	plain, err := json.Marshal(sepayManagedPayload{ConfigJSON: cfg.ConfigJSON, BotToken: cfg.BotToken})
	if err != nil {
		return 0, ErrSePayConfigUnavailable
	}
	defer clear(plain)
	blob, err := s.sepayEncrypt(plain, []byte("sepay-managed-config:v1"))
	if err != nil {
		return 0, ErrSePayConfigUnavailable
	}
	next := cfg.Revision + 1
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		// This lock also works before the singleton exists, and has no payOS effect.
		if _, err := tx.ExecContext(ctx, `UPDATE sepay_managed_config SET id=id WHERE id=1`); err != nil {
			return err
		}
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		if err := checkSePayRevisionTx(ctx, tx, cfg.Revision); err != nil {
			return err
		}
		if storeKey != "" {
			var existingBank string
			var envelope []byte
			err := tx.QueryRowContext(ctx, `SELECT bank_code,account_envelope FROM connections WHERE id=?`, "sepay-store:"+storeKey).Scan(&existingBank, &envelope)
			if err == nil {
				receiver, err := s.sepayDecrypt(envelope, []byte("sepay-store:"+storeKey))
				if err != nil {
					return ErrSePayConfigUnavailable
				}
				defer clear(receiver)
				if existingBank != bank || string(receiver) != account {
					return ErrSePayReceiverMismatch
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sepay_managed_config(id,revision,config_envelope,updated_at) VALUES(1,?,?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,config_envelope=excluded.config_envelope,updated_at=excluded.updated_at`, next, blob, now()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(id,actor_subject,actor_role,action,target,request_id,details_json,created_at) VALUES(?,?,?,?,?,?, '{}',?)`, id("audit"), actor.Subject, actor.Role, "sepay-store.config.save", "SEPAY", actor.RequestID, now())
		return err
	})
	return next, err
}

func checkSePayRevisionTx(ctx context.Context, tx *sql.Tx, expected int64) error {
	var actual int64
	err := tx.QueryRowContext(ctx, `SELECT revision FROM sepay_managed_config WHERE id=1`).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		actual = 0
	} else if err != nil {
		return ErrSePayConfigUnavailable
	}
	if expected != actual {
		return ErrSePayRevisionConflict
	}
	return nil
}
