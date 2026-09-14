package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
)

type EndpointWithSecret struct {
	Endpoint
	Secret string `json:"secret,omitempty"`
}

func (s *Store) CreateEndpointWithSecret(ctx context.Context, name, targetURL string) (EndpointWithSecret, error) {
	if err := s.CheckMutationAllowed(ctx); err != nil {
		return EndpointWithSecret{}, err
	}
	if name == "" || targetURL == "" {
		return EndpointWithSecret{}, errors.New("name and URL are required")
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return EndpointWithSecret{}, err
	}
	secretHex := hex.EncodeToString(secretBytes)
	epID := id("ep")
	createdAt := now()

	var secretBlob []byte
	if s.keyring != nil {
		env, err := s.keyring.Encrypt([]byte(secretHex), []byte("webhook-secret:"+epID+":k1"))
		if err != nil {
			return EndpointWithSecret{}, fmt.Errorf("encrypt webhook secret: %w", err)
		}
		envBytes, err := json.Marshal(env)
		if err != nil {
			return EndpointWithSecret{}, fmt.Errorf("marshal webhook secret envelope: %w", err)
		}
		secretBlob = envBytes
	} else {
		secretBlob = []byte(secretHex)
	}

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO webhook_endpoints(id,name,status,current_revision,created_at,updated_at) VALUES(?,?,?,1,?,?)`, epID, name, "DISABLED", createdAt, createdAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO endpoint_versions(endpoint_id,revision,url,filters_json,created_at) VALUES(?,1,?,'[]',?)`, epID, targetURL, createdAt); err != nil {
			return err
		}
		encodingVersion := "legacy-hex"
		if s.keyring != nil {
			encodingVersion = "aes-gcm-v1"
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO endpoint_secrets(endpoint_id,key_id,envelope,status,created_at,encoding_version) VALUES(?,'k1',?,'ACTIVE',?,?)`, epID, secretBlob, createdAt, encodingVersion)
		return err
	})
	if err != nil {
		return EndpointWithSecret{}, err
	}

	return EndpointWithSecret{
		Endpoint: Endpoint{
			ID:        epID,
			Name:      name,
			URL:       targetURL,
			Status:    "DISABLED",
			Revision:  1,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		},
		Secret: secretHex,
	}, nil
}

func (s *Store) EndpointDetails(ctx context.Context, endpointID string) (url string, secret []byte, err error) {
	var sec []byte
	var keyID, encodingVersion string
	err = s.db.QueryRowContext(ctx, `
		SELECT v.url, s.envelope, s.key_id, s.encoding_version
		FROM webhook_endpoints e
		JOIN endpoint_versions v ON v.endpoint_id = e.id AND v.revision = e.current_revision
		JOIN endpoint_secrets s ON s.endpoint_id = e.id AND s.status = 'ACTIVE'
		WHERE e.id = ?
		LIMIT 1
	`, endpointID).Scan(&url, &sec, &keyID, &encodingVersion)
	if err != nil {
		return "", nil, err
	}

	if encodingVersion == "aes-gcm-v1" || bytes.HasPrefix(sec, []byte(`{"Version":`)) {
		if s.keyring == nil {
			return "", nil, errors.New("master keyring required to decrypt webhook secret")
		}
		var env security.Envelope
		if err := json.Unmarshal(sec, &env); err != nil {
			return "", nil, fmt.Errorf("unmarshal webhook secret envelope: %w", err)
		}
		decrypted, err := s.keyring.Decrypt(env, []byte("webhook-secret:"+endpointID+":"+keyID))
		if err != nil {
			return "", nil, fmt.Errorf("decrypt webhook secret: %w", err)
		}
		return url, decrypted, nil
	}

	if encodingVersion != "legacy-hex" {
		return "", nil, fmt.Errorf("unsupported webhook secret encoding %q", encodingVersion)
	}
	return url, sec, nil
}

func (s *Store) EventPayload(ctx context.Context, eventID string) ([]byte, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM events WHERE id = ?`, eventID).Scan(&payload)
	return payload, err
}

// EmitTransactionEvent supports legacy callers and records the durable journal entry atomically.
func (s *Store) EmitTransactionEvent(ctx context.Context, transactionID, eventType, namespace, semanticKey string, data any) (string, error) {
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256([]byte(namespace + "\x00" + semanticKey + "\x00" + eventType))
	eventID := "bevt_" + hex.EncodeToString(sum[:16])
	payloadHash := hex.EncodeToString(sum[:])
	createdAt := now()

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO events(id, transaction_id, event_type, payload, payload_hash, created_at)
			VALUES(?, ?, ?, ?, ?, ?)
			ON CONFLICT(transaction_id, event_type) DO NOTHING
		`, eventID, transactionID, eventType, dataBytes, payloadHash, createdAt)
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted == 0 {
			return nil
		}

		// Queue delivery to all active endpoints
		rows, err := tx.QueryContext(ctx, `
			SELECT e.id, e.current_revision, COALESCE(e.provider, 'WEBHOOK'), COALESCE(s.key_id, 'k1')
			FROM webhook_endpoints e
			LEFT JOIN endpoint_secrets s ON s.endpoint_id = e.id AND s.status = 'ACTIVE'
			WHERE e.status = 'ACTIVE'
		`)
		if err != nil {
			return err
		}
		defer rows.Close()

		type epInfo struct {
			id       string
			revision int
			provider string
			keyID    string
		}
		var endpoints []epInfo
		for rows.Next() {
			var ep epInfo
			if err := rows.Scan(&ep.id, &ep.revision, &ep.provider, &ep.keyID); err != nil {
				return err
			}
			endpoints = append(endpoints, ep)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		for _, ep := range endpoints {
			deliveryID := id("del")
			_, err := tx.ExecContext(ctx, `
				INSERT INTO deliveries(id, event_id, endpoint_id, endpoint_revision, key_id, status, attempts, next_attempt_at, created_at, updated_at)
				VALUES(?, ?, ?, ?, ?, 'PENDING', 0, ?, ?, ?)
				ON CONFLICT(event_id, endpoint_id) DO NOTHING
			`, deliveryID, eventID, ep.id, ep.revision, ep.keyID, createdAt, createdAt, createdAt)
			if err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO event_journal(epoch,event_type,aggregate_id,payload_json,created_at) VALUES('ep1',?,?,?,?)`, eventType, transactionID, string(dataBytes), createdAt)
		return err
	})

	if err != nil {
		return "", fmt.Errorf("emit transaction event: %w", err)
	}
	return eventID, nil
}

func (s *Store) FailDelivery(ctx context.Context, deliveryID, claimToken, reason string) error {
	if deliveryID == "" || claimToken == "" {
		return errors.New("delivery claim is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE deliveries SET status='DEAD_LETTER',claim_token=NULL,lease_until=NULL,updated_at=? WHERE id=? AND status='IN_FLIGHT' AND claim_token=?`, now(), deliveryID, claimToken)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) RecordAttempt(ctx context.Context, deliveryID string, attemptNum, statusCode, latencyMs int, outcome, sanitizedError, provider, providerErrorCode string) error {
	if provider == "" {
		provider = "WEBHOOK"
	}
	attemptID := id("att")
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO delivery_attempts(id, delivery_id, attempt_number, status_code, latency_ms, outcome, sanitized_error, provider, provider_error_code, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, attemptID, deliveryID, attemptNum, statusCode, latencyMs, outcome, sanitizedError, provider, providerErrorCode, now())
	return err
}
