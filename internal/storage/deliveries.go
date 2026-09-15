package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Delivery struct {
	ID                     string `json:"id"`
	EventID                string `json:"eventId"`
	EndpointID             string `json:"endpointId"`
	EndpointRevision       int    `json:"endpointRevision"`
	KeyID                  string `json:"keyId"`
	Provider               string `json:"provider"`
	Status                 string `json:"status"`
	Attempts               int    `json:"attempts"`
	RetryCycleStartAttempt int    `json:"retryCycleStartAttempt"`
	ClaimToken             string `json:"-"`
	LeaseUntil             string `json:"leaseUntil,omitempty"`
	NextAttempt            string `json:"nextAttemptAt"`
}

func (s *Store) NextDeliveryDue(ctx context.Context, nowAt time.Time) (time.Time, error) {
	nowString := nowAt.UTC().Format(time.RFC3339Nano)
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT min(CASE WHEN d.status='IN_FLIGHT' THEN d.lease_until ELSE d.next_attempt_at END)
		FROM deliveries d
		JOIN webhook_endpoints e ON e.id=d.endpoint_id
		WHERE e.status='ACTIVE'
		  AND d.status IN ('PENDING','IN_FLIGHT')
		  AND (d.status!='IN_FLIGHT' OR d.lease_until IS NOT NULL)
		  AND NOT EXISTS (
			SELECT 1 FROM deliveries active
			WHERE active.endpoint_id=d.endpoint_id
			  AND active.status='IN_FLIGHT'
			  AND active.lease_until>=?
			  AND active.id<>d.id
		  )
	`, nowString).Scan(&raw)
	if err != nil {
		return time.Time{}, err
	}
	if !raw.Valid || raw.String == "" {
		return time.Time{}, sql.ErrNoRows
	}
	due, err := time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		return time.Time{}, err
	}
	return due, nil
}

func (s *Store) ClaimDelivery(ctx context.Context, nowAt time.Time, lease time.Duration) (Delivery, error) {
	if lease <= 0 {
		return Delivery{}, errors.New("delivery lease must be positive")
	}
	var delivery Delivery
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		nowString := nowAt.Format(time.RFC3339Nano)
		err := tx.QueryRowContext(ctx, `
			SELECT d.id, d.event_id, d.endpoint_id, COALESCE(d.endpoint_revision, 1), COALESCE(d.key_id, 'k1'),
			       COALESCE(e.provider, 'WEBHOOK'), d.status, d.attempts, COALESCE(d.retry_cycle_start_attempt, 0), d.next_attempt_at
			FROM deliveries d
			JOIN webhook_endpoints e ON e.id = d.endpoint_id
			WHERE e.status = 'ACTIVE'
			  AND ((d.status='PENDING' AND d.next_attempt_at<=?) OR (d.status='IN_FLIGHT' AND d.lease_until<?))
			  AND NOT EXISTS (
				SELECT 1 FROM deliveries active
				WHERE active.endpoint_id=d.endpoint_id AND active.status='IN_FLIGHT'
				  AND active.lease_until>=? AND active.id<>d.id
			  )
			ORDER BY d.next_attempt_at, d.id LIMIT 1
		`, nowString, nowString, nowString).Scan(
			&delivery.ID, &delivery.EventID, &delivery.EndpointID, &delivery.EndpointRevision, &delivery.KeyID,
			&delivery.Provider, &delivery.Status, &delivery.Attempts, &delivery.RetryCycleStartAttempt, &delivery.NextAttempt,
		)
		if err != nil {
			return err
		}
		delivery.ClaimToken = id("claim")
		delivery.LeaseUntil = nowAt.Add(lease).Format(time.RFC3339Nano)
		result, err := tx.ExecContext(ctx, `UPDATE deliveries SET status='IN_FLIGHT',attempts=attempts+1,claim_token=?,lease_until=?,updated_at=? WHERE id=? AND ((status='PENDING' AND next_attempt_at<=?) OR (status='IN_FLIGHT' AND lease_until<?))`, delivery.ClaimToken, delivery.LeaseUntil, now(), delivery.ID, nowAt.Format(time.RFC3339Nano), nowAt.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return sql.ErrNoRows
		}
		delivery.Status = "IN_FLIGHT"
		delivery.Attempts++
		return nil
	})
	return delivery, err
}

func (s *Store) CompleteDelivery(ctx context.Context, delivery Delivery, delivered bool, retryAt time.Time) error {
	if delivery.ID == "" || delivery.ClaimToken == "" {
		return errors.New("delivery claim is required")
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		status := "PENDING"
		if delivered {
			status = "DELIVERED"
		}
		result, err := tx.ExecContext(ctx, `UPDATE deliveries SET status=?,next_attempt_at=?,claim_token=NULL,lease_until=NULL,updated_at=? WHERE id=? AND status='IN_FLIGHT' AND claim_token=?`, status, retryAt.UTC().Format(time.RFC3339Nano), now(), delivery.ID, delivery.ClaimToken)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
}
