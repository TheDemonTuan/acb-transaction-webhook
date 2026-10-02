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
	var count int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		count, err = expireStaleAuthAttemptsTx(ctx, tx)
		return err
	})
	return count, err
}

func expireStaleAuthAttemptsTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	nowTime := time.Now().UTC()
	rows, err := tx.QueryContext(ctx, `SELECT id,connection_id,generation,expires_at FROM auth_attempts WHERE status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING')`)
	if err != nil {
		return 0, err
	}
	var stale []AuthAttempt
	for rows.Next() {
		var a AuthAttempt
		if err := rows.Scan(&a.ID, &a.ConnectionID, &a.Generation, &a.ExpiresAt); err != nil {
			rows.Close()
			return 0, err
		}
		expires, err := time.Parse(time.RFC3339Nano, a.ExpiresAt)
		if err != nil {
			rows.Close()
			return 0, err
		}
		if !nowTime.Before(expires) {
			stale = append(stale, a)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, a := range stale {
		e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE attempt_id=? AND finished_at IS NULL`, a.ID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if err == nil {
			if err := checkRecoveryCurrentTx(ctx, tx, e); err != nil {
				if !errors.Is(err, ErrRecoverySuperseded) {
					return 0, err
				}
				if err := supersedeAuthRecoveryTx(ctx, tx, e); err != nil {
					return 0, err
				}
			} else if err := finishRecoveryAuthAttemptTx(ctx, tx, e, "EXPIRED", "WAIT_OPERATOR", "ATTEMPT_EXPIRED", time.Time{}); err != nil {
				return 0, err
			}
		} else {
			// Stale generations are terminalized without touching the new connection.
			if _, err := tx.ExecContext(ctx, `UPDATE connections SET state='AUTH_REQUIRED',generation=generation+1,updated_at=? WHERE id=? AND generation=? AND state='AUTH_STARTING'`, now(), a.ConnectionID, a.Generation); err != nil {
				return 0, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE auth_attempts SET status='EXPIRED',finished_at=? WHERE id=?`, now(), a.ID); err != nil {
			return 0, err
		}
	}
	return int64(len(stale)), nil
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
	var attempt AuthAttempt
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := expireStaleAuthAttemptsTx(ctx, tx); err != nil {
			return err
		}
		var connectionID string
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT id,generation FROM connections ORDER BY created_at LIMIT 1`).Scan(&connectionID, &generation); err != nil {
			return err
		}
		var err error
		attempt, err = s.startAuthAttemptTx(ctx, tx, connectionID, generation, owner, ttl)
		if err != nil {
			return err
		}
		e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE connection_id=? AND finished_at IS NULL`, connectionID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return supersedeAuthRecoveryTx(ctx, tx, e)
	})
	return attempt, err
}

func (s *Store) startAuthAttemptTx(ctx context.Context, tx *sql.Tx, connectionID string, generation int64, owner string, ttl time.Duration) (AuthAttempt, error) {
	if ttl <= 0 {
		return AuthAttempt{}, errors.New("auth attempt TTL must be positive")
	}
	if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
		return AuthAttempt{}, err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_attempts WHERE connection_id=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING'))`, connectionID).Scan(&active); err != nil {
		return AuthAttempt{}, err
	}
	if active {
		return AuthAttempt{}, ErrAuthAttemptActive
	}
	attempt := AuthAttempt{ID: id("auth"), ConnectionID: connectionID, Generation: generation + 1, OwnerSubject: owner, Status: "STARTING", ExpiresAt: time.Now().UTC().Add(ttl).Format(time.RFC3339Nano), CreatedAt: now()}
	result, err := tx.ExecContext(ctx, `UPDATE connections SET state='AUTH_STARTING',generation=?,updated_at=? WHERE id=? AND generation=?`, attempt.Generation, now(), connectionID, generation)
	if err != nil {
		return AuthAttempt{}, err
	}
	if err := requireRecoveryRow(result, ErrAuthAttemptActive); err != nil {
		return AuthAttempt{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_attempts(id,connection_id,generation,owner_subject,status,expires_at,created_at) VALUES(?,?,?,?,?,?,?)`, attempt.ID, connectionID, attempt.Generation, owner, attempt.Status, attempt.ExpiresAt, attempt.CreatedAt)
	if err != nil && (strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "one_active_auth_attempt")) {
		err = ErrAuthAttemptActive
	}
	return attempt, err
}

func (s *Store) MarkAuthAttemptInProgress(ctx context.Context, attemptID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE auth_attempts SET status='IN_PROGRESS'
		WHERE id=? AND status='STARTING' AND julianday(expires_at)>julianday(?) AND EXISTS (
			SELECT 1 FROM connections c WHERE c.id=auth_attempts.connection_id
			AND c.generation=auth_attempts.generation AND c.state='AUTH_STARTING'
		)`, attemptID, now())
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
	return s.withTx(ctx, func(tx *sql.Tx) error {
		e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE attempt_id=? AND finished_at IS NULL`, attemptID))
		if err == nil {
			state := "RETRY_WAIT"
			if status == "CANCELLED" {
				state = "CANCELLED"
			}
			if status == "EXPIRED" {
				state = "WAIT_OPERATOR"
			}
			return finishRecoveryAuthAttemptTx(ctx, tx, e, status, state, "ATTEMPT_"+status, time.Now().UTC().Add(30*time.Second))
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return finishAuthAttemptTx(ctx, tx, attemptID, status)
	})
}

func finishAuthAttemptTx(ctx context.Context, tx *sql.Tx, attemptID, status string) error {
	if status != "CANCELLED" && status != "EXPIRED" && status != "FAILED" {
		return errors.New("invalid auth attempt finish status")
	}
	var connectionID string
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT connection_id,generation FROM auth_attempts WHERE id=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING')`, attemptID).Scan(&connectionID, &generation); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE connections SET state='AUTH_REQUIRED',generation=generation+1,updated_at=? WHERE id=? AND generation=? AND state='AUTH_STARTING'`, now(), connectionID, generation)
	if err != nil {
		return err
	}
	if err := requireRecoveryRow(result, ErrGenerationFenceMismatch); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE auth_attempts SET status=?,finished_at=? WHERE id=?`, status, now(), attemptID)
	return err
}

// HasActiveAuthAttempt checks whether an interactive browser authentication attempt
// is currently pending, in progress, or exporting.
func (s *Store) HasActiveAuthAttempt(ctx context.Context, connectionID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM auth_attempts a
		WHERE a.connection_id = ? AND a.status IN ('STARTING', 'IN_PROGRESS', 'EXPORTING', 'VERIFYING') AND a.expires_at > ?
		AND EXISTS (SELECT 1 FROM connections c WHERE c.id=a.connection_id AND c.generation=a.generation)
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

type AuthLifecycleSummary struct {
	HasActiveAttempt        bool           `json:"hasActiveAttempt"`
	ActiveAttemptAgeSeconds float64        `json:"activeAttemptAgeSeconds"`
	ActiveAttemptStuck      bool           `json:"activeAttemptStuck"`
	SessionState            string         `json:"sessionState"`
	RecentAttemptsCount     int            `json:"recentAttemptsCount"`
	AttemptsByStatus        map[string]int `json:"attemptsByStatus"`
}

func (s *Store) AuthLifecycleSummary(ctx context.Context, stuckThreshold time.Duration) (AuthLifecycleSummary, error) {
	summary := AuthLifecycleSummary{
		AttemptsByStatus: make(map[string]int),
		SessionState:     "UNKNOWN",
	}
	if s.db == nil {
		return summary, errors.New("database not open")
	}

	if conn, err := s.Connection(ctx); err == nil {
		summary.SessionState = conn.State
	}

	if stuckThreshold <= 0 {
		stuckThreshold = 10 * time.Minute
	}
	activeReport, err := s.ActiveAuthAttempts(ctx)
	if err == nil && len(activeReport.Attempts) > 0 {
		summary.HasActiveAttempt = true
		oldest := activeReport.Attempts[0]
		if t, err := time.Parse(time.RFC3339Nano, oldest.CreatedAt); err == nil {
			summary.ActiveAttemptAgeSeconds = time.Since(t).Seconds()
		} else if t, err := time.Parse(time.RFC3339, oldest.CreatedAt); err == nil {
			summary.ActiveAttemptAgeSeconds = time.Since(t).Seconds()
		}
		if summary.ActiveAttemptAgeSeconds < 0 {
			summary.ActiveAttemptAgeSeconds = 0
		}
		if summary.ActiveAttemptAgeSeconds > stuckThreshold.Seconds() {
			summary.ActiveAttemptStuck = true
		}
	}

	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM auth_attempts GROUP BY status`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var st string
			var cnt int
			if err := rows.Scan(&st, &cnt); err == nil {
				summary.AttemptsByStatus[st] = cnt
				summary.RecentAttemptsCount += cnt
			}
		}
	}

	return summary, nil
}
