package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrAuthAttemptActive = errors.New("an auth attempt is already active")

type AuthAttempt struct {
	ID           string `json:"id"`
	ConnectionID string `json:"connectionId"`
	Generation   int64  `json:"generation"`
	Status       string `json:"status"`
	ExpiresAt    string `json:"expiresAt"`
	CreatedAt    string `json:"createdAt"`
	OwnerSubject string `json:"ownerSubject,omitempty"`
}

// ExpireStaleAuthAttempts marks any active auth attempts whose TTL has elapsed as EXPIRED,
// and resets connection state to AUTH_REQUIRED if it was still in AUTH_STARTING for that generation.
func (s *Store) ExpireStaleAuthAttempts(ctx context.Context) (int64, error) {
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	var expiredCount int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, connection_id, generation FROM auth_attempts
			WHERE status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING') AND expires_at <= ?
		`, nowStr)
		if err != nil {
			return err
		}
		defer rows.Close()

		type staleAttempt struct {
			id           string
			connectionID string
			generation   int64
		}
		var stale []staleAttempt
		for rows.Next() {
			var it staleAttempt
			if err := rows.Scan(&it.id, &it.connectionID, &it.generation); err != nil {
				return err
			}
			stale = append(stale, it)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		for _, it := range stale {
			_, err = tx.ExecContext(ctx, `UPDATE auth_attempts SET status='EXPIRED', finished_at=? WHERE id=?`, now(), it.id)
			if err != nil {
				return err
			}
			_, _ = tx.ExecContext(ctx, `
				UPDATE connections SET state='AUTH_REQUIRED', generation=generation+1, updated_at=?
				WHERE id=? AND generation=? AND state='AUTH_STARTING'
			`, now(), it.connectionID, it.generation)
			expiredCount++
		}
		return nil
	})
	return expiredCount, err
}

// ActiveAuthAttemptForOwner returns the current non-expired active auth attempt for the connection, if any.
func (s *Store) ActiveAuthAttemptForOwner(ctx context.Context, owner string) (AuthAttempt, bool, error) {
	connection, err := s.Connection(ctx)
	if err != nil {
		return AuthAttempt{}, false, err
	}
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	var attempt AuthAttempt
	err = s.db.QueryRowContext(ctx, `
		SELECT id, connection_id, generation, COALESCE(owner_subject, ''), status, expires_at, created_at
		FROM auth_attempts
		WHERE connection_id=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING')
		  AND owner_subject=? AND expires_at>?
		ORDER BY created_at DESC LIMIT 1
	`, connection.ID, owner, nowStr).Scan(
		&attempt.ID, &attempt.ConnectionID, &attempt.Generation,
		&attempt.OwnerSubject, &attempt.Status, &attempt.ExpiresAt, &attempt.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AuthAttempt{}, false, nil
		}
		return AuthAttempt{}, false, err
	}
	return attempt, true, nil
}

func (s *Store) StartAuthAttempt(ctx context.Context, owner string, ttl time.Duration) (AuthAttempt, error) {
	if ttl <= 0 {
		return AuthAttempt{}, errors.New("auth attempt TTL must be positive")
	}
	// Expire stale attempts first to prevent stale locks
	_, _ = s.ExpireStaleAuthAttempts(ctx)

	connection, err := s.Connection(ctx)
	if err != nil {
		return AuthAttempt{}, err
	}

	// Check if an active attempt already exists
	hasActive, err := s.HasActiveAuthAttempt(ctx, connection.ID)
	if err == nil && hasActive {
		return AuthAttempt{}, ErrAuthAttemptActive
	}

	attempt := AuthAttempt{
		ID:           id("auth"),
		ConnectionID: connection.ID,
		Generation:   connection.Generation + 1,
		Status:       "STARTING",
		ExpiresAt:    time.Now().UTC().Add(ttl).Format(time.RFC3339Nano),
		CreatedAt:    now(),
		OwnerSubject: owner,
	}

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE connections SET state='AUTH_STARTING',generation=?,updated_at=? WHERE id=? AND generation=?`, attempt.Generation, now(), connection.ID, connection.Generation)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrAuthAttemptActive
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO auth_attempts(id,connection_id,generation,owner_subject,status,expires_at,created_at) VALUES(?,?,?,?,?,?,?)`, attempt.ID, attempt.ConnectionID, attempt.Generation, owner, attempt.Status, attempt.ExpiresAt, attempt.CreatedAt)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "one_active_auth_attempt") || strings.Contains(err.Error(), "database is locked") {
				return ErrAuthAttemptActive
			}
			return err
		}
		return nil
	})
	return attempt, err
}

func (s *Store) MarkAuthAttemptInProgress(ctx context.Context, attemptID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE auth_attempts SET status='IN_PROGRESS'
		WHERE id=? AND status='STARTING' AND EXISTS (
			SELECT 1 FROM connections c WHERE c.id=auth_attempts.connection_id
			AND c.generation=auth_attempts.generation AND c.state='AUTH_STARTING'
		)`, attemptID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) FinishAuthAttempt(ctx context.Context, attemptID, status string) error {
	if status != "CANCELLED" && status != "EXPIRED" && status != "FAILED" {
		return errors.New("invalid auth attempt finish status")
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var connectionID string
		var generation int64
		err := tx.QueryRowContext(ctx, `SELECT connection_id,generation FROM auth_attempts WHERE id=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING')`, attemptID).Scan(&connectionID, &generation)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE connections SET state='AUTH_REQUIRED',generation=generation+1,updated_at=? WHERE id=? AND generation=?`, now(), connectionID, generation)
		if err != nil {
			return err
		}
		if _, err := result.RowsAffected(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE auth_attempts SET status=?,finished_at=? WHERE id=?`, status, now(), attemptID)
		return err
	})
}

// HasActiveAuthAttempt checks whether an interactive browser authentication attempt
// is currently pending, in progress, or exporting.
func (s *Store) HasActiveAuthAttempt(ctx context.Context, connectionID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM auth_attempts
		WHERE connection_id = ? AND status IN ('STARTING', 'IN_PROGRESS', 'EXPORTING', 'VERIFYING') AND expires_at > ?
	`, connectionID, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&count)
	return count > 0, err
}

type ActiveAuthReport struct {
	ActiveCount int           `json:"activeCount"`
	Count       int           `json:"count"`
	Attempts    []AuthAttempt `json:"attempts"`
}

// ActiveAuthAttempts returns all active non-expired auth attempts across all connections.
func (s *Store) ActiveAuthAttempts(ctx context.Context) (ActiveAuthReport, error) {
	rep := ActiveAuthReport{
		Attempts: []AuthAttempt{},
	}
	var tableExists int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='auth_attempts'`).Scan(&tableExists)
	if err != nil {
		return rep, fmt.Errorf("check auth_attempts table: %w", err)
	}
	if tableExists == 0 {
		return rep, nil
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, connection_id, generation, COALESCE(owner_subject, ''), status, expires_at, created_at
		FROM auth_attempts
		WHERE status IN ('STARTING', 'IN_PROGRESS', 'EXPORTING', 'VERIFYING') AND expires_at > ?
		ORDER BY created_at ASC
	`, nowStr)
	if err != nil {
		return rep, fmt.Errorf("query active auth attempts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var it AuthAttempt
		if err := rows.Scan(
			&it.ID, &it.ConnectionID, &it.Generation,
			&it.OwnerSubject, &it.Status, &it.ExpiresAt, &it.CreatedAt,
		); err != nil {
			return rep, fmt.Errorf("scan auth attempt: %w", err)
		}
		rep.Attempts = append(rep.Attempts, it)
	}
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("iterate auth attempts: %w", err)
	}
	rep.ActiveCount = len(rep.Attempts)
	rep.Count = len(rep.Attempts)
	return rep, nil
}
