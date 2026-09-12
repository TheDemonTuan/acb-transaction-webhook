package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
)

var (
	ErrDeliveryNotDeadLetter = errors.New("delivery is not in dead-letter state")
	ErrEndpointNotActive     = errors.New("notification channel is not active")
	ErrRevisionConflict      = errors.New("channel revision conflict")
	ErrInvalidBarkConfig     = errors.New("invalid Bark configuration")
)

type BarkConfig struct {
	Group              string `json:"group,omitempty"`
	Level              string `json:"level,omitempty"`
	Sound              string `json:"sound,omitempty"`
	Icon               string `json:"icon,omitempty"`
	IncludeBalance     bool   `json:"includeBalance"`
	IncludeDescription bool   `json:"includeDescription"`
	DashboardLink      bool   `json:"dashboardLink"`
}

type NotificationChannel struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Provider     string      `json:"provider"`
	Status       string      `json:"status"`
	Revision     int         `json:"revision"`
	URL          string      `json:"url,omitempty"`
	BarkConfig   *BarkConfig `json:"barkConfig,omitempty"`
	HasDeviceKey bool        `json:"hasDeviceKey,omitempty"`
	Secret       string      `json:"secret,omitempty"`
	CreatedAt    string      `json:"createdAt"`
	UpdatedAt    string      `json:"updatedAt"`
}

type DeliveryTarget struct {
	DeliveryID       string
	EndpointID       string
	Provider         string
	URL              string
	BarkConfig       *BarkConfig
	Secret           []byte
	EndpointRevision int
	KeyID            string
}

func DefaultBarkConfig() BarkConfig {
	return BarkConfig{
		Group:              "ACB",
		Level:              "timeSensitive",
		Sound:              "shake",
		Icon:               "https://api.vietqr.io/img/ACB.png",
		IncludeBalance:     false,
		IncludeDescription: true,
		DashboardLink:      true,
	}
}

func ValidateAndNormalizeBarkConfig(cfg *BarkConfig) (BarkConfig, error) {
	res := DefaultBarkConfig()
	if cfg != nil {
		if g := strings.TrimSpace(cfg.Group); g != "" {
			if len(g) > 64 {
				return res, fmt.Errorf("group name too long (max 64): %s", g)
			}
			res.Group = g
		}
		if l := strings.TrimSpace(cfg.Level); l != "" {
			switch l {
			case "passive", "active", "timeSensitive":
				res.Level = l
			default:
				return res, fmt.Errorf("invalid level %q: must be passive, active, or timeSensitive", l)
			}
		}
		if s := strings.TrimSpace(cfg.Sound); s != "" {
			if len(s) > 64 {
				return res, fmt.Errorf("sound name too long (max 64): %s", s)
			}
			res.Sound = s
		}
		if ic := strings.TrimSpace(cfg.Icon); ic != "" {
			if len(ic) > 500 {
				return res, fmt.Errorf("icon URL too long (max 500): %s", ic)
			}
			res.Icon = ic
		}
		res.IncludeBalance = cfg.IncludeBalance
		res.IncludeDescription = cfg.IncludeDescription
		res.DashboardLink = cfg.DashboardLink
	}
	return res, nil
}

func (s *Store) CreateBarkChannel(ctx context.Context, name, deviceKey string, cfg *BarkConfig) (NotificationChannel, error) {
	name = strings.TrimSpace(name)
	deviceKey = strings.TrimSpace(deviceKey)
	if name == "" {
		return NotificationChannel{}, errors.New("channel name is required")
	}
	if deviceKey == "" {
		return NotificationChannel{}, errors.New("bark device key is required")
	}
	if len(deviceKey) > 500 {
		return NotificationChannel{}, errors.New("bark device key exceeds max length of 500 characters")
	}
	for _, r := range deviceKey {
		if r < 32 || r == 127 {
			return NotificationChannel{}, errors.New("bark device key contains invalid control characters")
		}
	}

	normCfg, err := ValidateAndNormalizeBarkConfig(cfg)
	if err != nil {
		return NotificationChannel{}, err
	}
	cfgJSON, err := json.Marshal(normCfg)
	if err != nil {
		return NotificationChannel{}, fmt.Errorf("marshal bark config: %w", err)
	}

	if s.keyring == nil {
		return NotificationChannel{}, errors.New("master keyring required to store Bark device key")
	}

	chID := id("ch_bark")
	createdAt := now()

	env, err := s.keyring.Encrypt([]byte(deviceKey), []byte("notification-secret:BARK:"+chID+":k1"))
	if err != nil {
		return NotificationChannel{}, fmt.Errorf("encrypt bark device key: %w", err)
	}
	secretBlob, err := json.Marshal(env)
	if err != nil {
		return NotificationChannel{}, fmt.Errorf("marshal secret envelope: %w", err)
	}

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO webhook_endpoints(id, name, provider, status, current_revision, created_at, updated_at)
			VALUES(?, ?, 'BARK', 'DISABLED', 1, ?, ?)
		`, chID, name, createdAt, createdAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO endpoint_versions(endpoint_id, revision, url, filters_json, provider_config_json, created_at)
			VALUES(?, 1, '', '[]', ?, ?)
		`, chID, string(cfgJSON), createdAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO endpoint_secrets(endpoint_id, key_id, secret_kind, envelope, status, created_at, encoding_version)
			VALUES(?, 'k1', 'BARK_DEVICE_KEY', ?, 'ACTIVE', ?, 'aes-gcm-v1')
		`, chID, secretBlob, createdAt)
		return err
	})
	if err != nil {
		return NotificationChannel{}, err
	}

	return NotificationChannel{
		ID:           chID,
		Name:         name,
		Provider:     "BARK",
		Status:       "DISABLED",
		Revision:     1,
		BarkConfig:   &normCfg,
		HasDeviceKey: true,
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}, nil
}

func (s *Store) NotificationChannels(ctx context.Context) ([]NotificationChannel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.name, COALESCE(e.provider, 'WEBHOOK'), e.status, e.current_revision,
		       COALESCE(v.url, ''), COALESCE(v.provider_config_json, '{}'),
		       EXISTS(SELECT 1 FROM endpoint_secrets s WHERE s.endpoint_id = e.id AND s.status = 'ACTIVE'),
		       e.created_at, e.updated_at
		FROM webhook_endpoints e
		JOIN endpoint_versions v ON v.endpoint_id = e.id AND v.revision = e.current_revision
		ORDER BY e.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channels []NotificationChannel
	for rows.Next() {
		var ch NotificationChannel
		var cfgJSON string
		if err := rows.Scan(&ch.ID, &ch.Name, &ch.Provider, &ch.Status, &ch.Revision, &ch.URL, &cfgJSON, &ch.HasDeviceKey, &ch.CreatedAt, &ch.UpdatedAt); err != nil {
			return nil, err
		}
		if ch.Provider == "BARK" && cfgJSON != "" && cfgJSON != "{}" {
			var bCfg BarkConfig
			if err := json.Unmarshal([]byte(cfgJSON), &bCfg); err == nil {
				ch.BarkConfig = &bCfg
			}
		}
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

func (s *Store) NotificationChannelByID(ctx context.Context, id string) (NotificationChannel, error) {
	var ch NotificationChannel
	var cfgJSON string
	err := s.db.QueryRowContext(ctx, `
		SELECT e.id, e.name, COALESCE(e.provider, 'WEBHOOK'), e.status, e.current_revision,
		       COALESCE(v.url, ''), COALESCE(v.provider_config_json, '{}'),
		       EXISTS(SELECT 1 FROM endpoint_secrets s WHERE s.endpoint_id = e.id AND s.status = 'ACTIVE'),
		       e.created_at, e.updated_at
		FROM webhook_endpoints e
		JOIN endpoint_versions v ON v.endpoint_id = e.id AND v.revision = e.current_revision
		WHERE e.id = ?
		LIMIT 1
	`, id).Scan(&ch.ID, &ch.Name, &ch.Provider, &ch.Status, &ch.Revision, &ch.URL, &cfgJSON, &ch.HasDeviceKey, &ch.CreatedAt, &ch.UpdatedAt)
	if err != nil {
		return NotificationChannel{}, err
	}
	if ch.Provider == "BARK" && cfgJSON != "" && cfgJSON != "{}" {
		var bCfg BarkConfig
		if err := json.Unmarshal([]byte(cfgJSON), &bCfg); err == nil {
			ch.BarkConfig = &bCfg
		}
	}
	return ch, nil
}

func (s *Store) UpdateChannel(ctx context.Context, id string, expectedRevision int, name, targetURL string, barkCfg *BarkConfig) (NotificationChannel, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return NotificationChannel{}, errors.New("channel name is required")
	}

	var updated NotificationChannel
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var provider string
		var curRevision int
		var status string
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(provider, 'WEBHOOK'), current_revision, status FROM webhook_endpoints WHERE id = ?`, id).Scan(&provider, &curRevision, &status)
		if err != nil {
			return err
		}
		if curRevision != expectedRevision {
			return fmt.Errorf("%w: expected %d, got %d", ErrRevisionConflict, expectedRevision, curRevision)
		}

		newRev := curRevision + 1
		t := now()
		var urlVal string
		var cfgJSON string

		if provider == "WEBHOOK" {
			targetURL = strings.TrimSpace(targetURL)
			if targetURL == "" {
				return errors.New("target URL is required for webhook channel")
			}
			if _, err := security.ValidateWebhookURL(targetURL); err != nil {
				return fmt.Errorf("invalid webhook URL: %w", err)
			}
			urlVal = targetURL
			cfgJSON = "{}"
		} else if provider == "BARK" {
			norm, err := ValidateAndNormalizeBarkConfig(barkCfg)
			if err != nil {
				return err
			}
			b, _ := json.Marshal(norm)
			cfgJSON = string(b)
			urlVal = ""
		} else {
			return fmt.Errorf("unknown provider %q", provider)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO endpoint_versions(endpoint_id, revision, url, filters_json, provider_config_json, created_at)
			VALUES(?, ?, ?, '[]', ?, ?)
		`, id, newRev, urlVal, cfgJSON, t); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE webhook_endpoints SET name = ?, current_revision = ?, updated_at = ? WHERE id = ?
		`, name, newRev, t, id); err != nil {
			return err
		}

		updated = NotificationChannel{
			ID:        id,
			Name:      name,
			Provider:  provider,
			Status:    status,
			Revision:  newRev,
			URL:       urlVal,
			CreatedAt: t,
			UpdatedAt: t,
		}
		if provider == "BARK" && barkCfg != nil {
			norm, _ := ValidateAndNormalizeBarkConfig(barkCfg)
			updated.BarkConfig = &norm
		}
		return nil
	})
	if err != nil {
		return NotificationChannel{}, err
	}
	return s.NotificationChannelByID(ctx, id)
}

func (s *Store) RotateSecret(ctx context.Context, chID string, newBarkDeviceKey string) (newSecret string, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var provider string
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(provider, 'WEBHOOK') FROM webhook_endpoints WHERE id = ?`, chID).Scan(&provider)
		if err != nil {
			return err
		}

		t := now()
		keyID := id("key")

		if provider == "WEBHOOK" {
			secretBytes := make([]byte, 32)
			if _, err := rand.Read(secretBytes); err != nil {
				return err
			}
			secretHex := hex.EncodeToString(secretBytes)
			newSecret = secretHex

			var secretBlob []byte
			encodingVersion := "legacy-hex"
			if s.keyring != nil {
				env, err := s.keyring.Encrypt([]byte(secretHex), []byte("webhook-secret:"+chID+":"+keyID))
				if err != nil {
					return fmt.Errorf("encrypt webhook secret: %w", err)
				}
				envBytes, err := json.Marshal(env)
				if err != nil {
					return fmt.Errorf("marshal envelope: %w", err)
				}
				secretBlob = envBytes
				encodingVersion = "aes-gcm-v1"
			} else {
				secretBlob = []byte(secretHex)
			}

			// Retire old active secret
			_, _ = tx.ExecContext(ctx, `UPDATE endpoint_secrets SET status = 'RETIRED', retired_at = ? WHERE endpoint_id = ? AND status = 'ACTIVE'`, t, chID)
			// Insert new active secret
			_, err = tx.ExecContext(ctx, `
				INSERT INTO endpoint_secrets(endpoint_id, key_id, secret_kind, envelope, status, created_at, encoding_version)
				VALUES(?, ?, 'WEBHOOK_HMAC', ?, 'ACTIVE', ?, ?)
			`, chID, keyID, secretBlob, t, encodingVersion)
			return err

		} else if provider == "BARK" {
			newBarkDeviceKey = strings.TrimSpace(newBarkDeviceKey)
			if newBarkDeviceKey == "" {
				return errors.New("bark device key is required")
			}
			if len(newBarkDeviceKey) > 500 {
				return errors.New("bark device key exceeds max length of 500 characters")
			}
			for _, r := range newBarkDeviceKey {
				if r < 32 || r == 127 {
					return errors.New("bark device key contains invalid control characters")
				}
			}
			if s.keyring == nil {
				return errors.New("master keyring required to rotate Bark device key")
			}

			env, err := s.keyring.Encrypt([]byte(newBarkDeviceKey), []byte("notification-secret:BARK:"+chID+":"+keyID))
			if err != nil {
				return fmt.Errorf("encrypt bark device key: %w", err)
			}
			secretBlob, err := json.Marshal(env)
			if err != nil {
				return fmt.Errorf("marshal secret envelope: %w", err)
			}

			// Retire old active secret
			_, _ = tx.ExecContext(ctx, `UPDATE endpoint_secrets SET status = 'RETIRED', retired_at = ? WHERE endpoint_id = ? AND status = 'ACTIVE'`, t, chID)
			// Insert new active secret
			_, err = tx.ExecContext(ctx, `
				INSERT INTO endpoint_secrets(endpoint_id, key_id, secret_kind, envelope, status, created_at, encoding_version)
				VALUES(?, ?, 'BARK_DEVICE_KEY', ?, 'ACTIVE', ?, 'aes-gcm-v1')
			`, chID, keyID, secretBlob, t)
			return err
		}

		return fmt.Errorf("unknown provider %q", provider)
	})
	return newSecret, err
}

func (s *Store) DeliveryTargetForDelivery(ctx context.Context, delivery Delivery) (DeliveryTarget, error) {
	var target DeliveryTarget
	target.DeliveryID = delivery.ID
	target.EndpointID = delivery.EndpointID
	target.EndpointRevision = delivery.EndpointRevision
	target.KeyID = delivery.KeyID

	var provider, urlVal, cfgJSON, keyID, encodingVersion, secretKind string
	var secretBlob []byte

	// Select matching version and secret using delivery snapshot if available
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(e.provider, 'WEBHOOK'), COALESCE(v.url, ''), COALESCE(v.provider_config_json, '{}'),
		       s.envelope, s.key_id, COALESCE(s.encoding_version, 'legacy-hex'), COALESCE(s.secret_kind, 'WEBHOOK_HMAC')
		FROM webhook_endpoints e
		JOIN endpoint_versions v ON v.endpoint_id = e.id AND v.revision = CASE WHEN ? > 0 THEN ? ELSE e.current_revision END
		JOIN endpoint_secrets s ON s.endpoint_id = e.id AND s.key_id = CASE WHEN ? != '' THEN ? ELSE (SELECT s2.key_id FROM endpoint_secrets s2 WHERE s2.endpoint_id = e.id AND s2.status = 'ACTIVE' LIMIT 1) END
		WHERE e.id = ?
		LIMIT 1
	`, delivery.EndpointRevision, delivery.EndpointRevision, delivery.KeyID, delivery.KeyID, delivery.EndpointID).Scan(
		&provider, &urlVal, &cfgJSON, &secretBlob, &keyID, &encodingVersion, &secretKind,
	)
	if err != nil {
		return target, fmt.Errorf("fetch delivery target for endpoint %s: %w", delivery.EndpointID, err)
	}

	target.Provider = provider
	target.URL = urlVal
	target.KeyID = keyID

	if provider == "BARK" {
		if s.keyring == nil {
			return target, errors.New("master keyring required to decrypt Bark device key")
		}
		var env security.Envelope
		if err := json.Unmarshal(secretBlob, &env); err != nil {
			return target, fmt.Errorf("unmarshal bark secret envelope: %w", err)
		}
		decrypted, err := s.keyring.Decrypt(env, []byte("notification-secret:BARK:"+delivery.EndpointID+":"+keyID))
		if err != nil {
			return target, fmt.Errorf("decrypt bark device key: %w", err)
		}
		target.Secret = decrypted

		if cfgJSON != "" && cfgJSON != "{}" {
			var bCfg BarkConfig
			if err := json.Unmarshal([]byte(cfgJSON), &bCfg); err == nil {
				target.BarkConfig = &bCfg
			}
		}
		if target.BarkConfig == nil {
			def := DefaultBarkConfig()
			target.BarkConfig = &def
		}
		return target, nil
	}

	// WEBHOOK provider
	if encodingVersion == "aes-gcm-v1" || bytes.HasPrefix(secretBlob, []byte(`{"Version":`)) {
		if s.keyring == nil {
			return target, errors.New("master keyring required to decrypt webhook secret")
		}
		var env security.Envelope
		if err := json.Unmarshal(secretBlob, &env); err != nil {
			return target, fmt.Errorf("unmarshal webhook secret envelope: %w", err)
		}
		decrypted, err := s.keyring.Decrypt(env, []byte("webhook-secret:"+delivery.EndpointID+":"+keyID))
		if err != nil {
			return target, fmt.Errorf("decrypt webhook secret: %w", err)
		}
		target.Secret = decrypted
		return target, nil
	}

	target.Secret = secretBlob
	return target, nil
}

func (s *Store) ReplayDelivery(ctx context.Context, deliveryID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var status, endpointStatus string
		var attempts int
		err := tx.QueryRowContext(ctx, `
			SELECT d.status, d.attempts, e.status
			FROM deliveries d
			JOIN webhook_endpoints e ON e.id = d.endpoint_id
			WHERE d.id = ?
		`, deliveryID).Scan(&status, &attempts, &endpointStatus)
		if err != nil {
			return err
		}
		if status != "DEAD_LETTER" {
			return ErrDeliveryNotDeadLetter
		}
		if endpointStatus != "ACTIVE" {
			return ErrEndpointNotActive
		}

		t := now()
		res, err := tx.ExecContext(ctx, `
			UPDATE deliveries
			SET status = 'PENDING',
			    next_attempt_at = ?,
			    retry_cycle_start_attempt = attempts,
			    updated_at = ?
			WHERE id = ? AND status = 'DEAD_LETTER'
		`, t, t, deliveryID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
}
