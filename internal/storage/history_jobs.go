package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// HistoryJobStatus defines typed state machine statuses for durable history jobs.
type HistoryJobStatus string

const (
	HistoryJobStatusQueued    HistoryJobStatus = "QUEUED"
	HistoryJobStatusRunning   HistoryJobStatus = "RUNNING"
	HistoryJobStatusCompleted HistoryJobStatus = "COMPLETED"
	HistoryJobStatusFailed    HistoryJobStatus = "FAILED"
	HistoryJobStatusCanceled  HistoryJobStatus = "CANCELED"
)

const (
	StatusQueued    = HistoryJobStatusQueued
	StatusRunning   = HistoryJobStatusRunning
	StatusCompleted = HistoryJobStatusCompleted
	StatusFailed    = HistoryJobStatusFailed
	StatusCanceled  = HistoryJobStatusCanceled
)

var (
	ErrJobNotFound            = errors.New("history sync job not found")
	ErrJobCanceled            = errors.New("history sync job is canceled")
	ErrJobTerminal            = errors.New("history sync job is already in terminal state")
	ErrInvalidStateTransition = errors.New("invalid history sync job state transition")
)

const historyJobCols = `id, connection_id, generation, range_from, range_to, status,
	current_day, pages_done, rows_seen, attempts, next_attempt_at,
	error_code, error_message, created_at, started_at, heartbeat_at,
	finished_at, updated_at`

// HistorySyncJob represents a durable asynchronous historical sync task.
// Request parameters (connectionId, generation, rangeFrom, rangeTo) are immutable once created.
type HistorySyncJob struct {
	ID            string           `json:"id"`
	ConnectionID  string           `json:"connectionId"`
	Generation    int64            `json:"generation"`
	RangeFrom     string           `json:"rangeFrom"`
	RangeTo       string           `json:"rangeTo"`
	Status        HistoryJobStatus `json:"status"`
	CurrentDay    string           `json:"currentDay,omitempty"`
	PagesDone     int              `json:"pagesDone"`
	RowsSeen      int              `json:"rowsSeen"`
	Attempts      int              `json:"attempts"`
	NextAttemptAt string           `json:"nextAttemptAt,omitempty"`
	ErrorCode     string           `json:"errorCode,omitempty"`
	ErrorMessage  string           `json:"errorMessage,omitempty"`
	CreatedAt     string           `json:"createdAt"`
	StartedAt     string           `json:"startedAt,omitempty"`
	HeartbeatAt   string           `json:"heartbeatAt,omitempty"`
	FinishedAt    string           `json:"finishedAt,omitempty"`
	UpdatedAt     string           `json:"updatedAt"`
}

var sensitiveErrorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(token|bearer|password|passwd|pwd|secret|cookie|authorization|auth)[=:\s]+[^\s,;]+`),
}

func sanitizeJobErrorMessage(msg string) string {
	if msg == "" {
		return ""
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<body") {
		msg = "upstream returned HTML error response"
	}
	for _, p := range sensitiveErrorPatterns {
		msg = p.ReplaceAllString(msg, "$1=[REDACTED]")
	}
	const maxLen = 1000
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "..."
	}
	return msg
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanHistoryJob(s rowScanner) (HistorySyncJob, error) {
	var j HistorySyncJob
	var currentDay, nextAttempt, errCode, errMsg, startedAt, heartbeatAt, finishedAt sql.NullString
	err := s.Scan(
		&j.ID, &j.ConnectionID, &j.Generation, &j.RangeFrom, &j.RangeTo, &j.Status,
		&currentDay, &j.PagesDone, &j.RowsSeen, &j.Attempts, &nextAttempt,
		&errCode, &errMsg, &j.CreatedAt, &startedAt,
		&heartbeatAt, &finishedAt, &j.UpdatedAt,
	)
	if err != nil {
		return HistorySyncJob{}, err
	}
	j.CurrentDay = currentDay.String
	j.NextAttemptAt = nextAttempt.String
	j.ErrorCode = errCode.String
	j.ErrorMessage = errMsg.String
	j.StartedAt = startedAt.String
	j.HeartbeatAt = heartbeatAt.String
	j.FinishedAt = finishedAt.String
	return j, nil
}

func (s *Store) checkAndFenceJob(ctx context.Context, tx *sql.Tx, jobID string, requiredStatus HistoryJobStatus) (string, error) {
	var connID string
	var cGen, jGen int64
	var status string
	err := tx.QueryRowContext(ctx, `
		SELECT j.connection_id, c.generation, j.generation, j.status
		FROM history_sync_jobs j
		JOIN connections c ON c.id = j.connection_id
		WHERE j.id = ?
	`, jobID).Scan(&connID, &cGen, &jGen, &status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrJobNotFound
		}
		return "", err
	}
	if status == string(HistoryJobStatusCanceled) {
		return "", ErrJobCanceled
	}
	if requiredStatus != "" && status != string(requiredStatus) {
		return "", fmt.Errorf("%w: job in status %s, expected %s", ErrInvalidStateTransition, status, requiredStatus)
	}
	if cGen != jGen {
		nowTs := now()
		_, _ = tx.ExecContext(ctx, `
			UPDATE history_sync_jobs
			SET status = 'CANCELED', error_code = 'STALE_GENERATION',
			    error_message = 'Connection generation bumped during execution',
			    finished_at = ?, updated_at = ?
			WHERE id = ?
		`, nowTs, nowTs, jobID)
		return "", fmt.Errorf("%w: job generation %d != connection generation %d", ErrGenerationFenceMismatch, jGen, cGen)
	}
	return connID, nil
}

// CreateOrGetHistorySyncJob atomically creates a new queued job or returns an active (QUEUED or RUNNING) job
// for the given connection, generation, and date range.
func (s *Store) CreateOrGetHistorySyncJob(ctx context.Context, connectionID string, generation int64, fromDay, toDay string) (HistorySyncJob, bool, error) {
	if connectionID == "" {
		return HistorySyncJob{}, false, errors.New("connection ID is required")
	}
	fromT, err := time.Parse("2006-01-02", fromDay)
	if err != nil {
		return HistorySyncJob{}, false, fmt.Errorf("invalid fromDay: %w", err)
	}
	toT, err := time.Parse("2006-01-02", toDay)
	if err != nil {
		return HistorySyncJob{}, false, fmt.Errorf("invalid toDay: %w", err)
	}
	if fromT.After(toT) {
		return HistorySyncJob{}, false, errors.New("from date must not be after to date")
	}

	var outJob HistorySyncJob
	var created bool

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var currentGen int64
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT generation, state FROM connections WHERE id = ?`, connectionID).Scan(&currentGen, &state); err != nil {
			return fmt.Errorf("verify connection: %w", err)
		}
		if currentGen != generation {
			return fmt.Errorf("%w: expected generation %d, current is %d", ErrGenerationFenceMismatch, generation, currentGen)
		}

		row := tx.QueryRowContext(ctx, `
			SELECT `+historyJobCols+` FROM history_sync_jobs
			WHERE connection_id = ? AND generation = ? AND range_from = ? AND range_to = ? AND status IN ('QUEUED', 'RUNNING')
			ORDER BY created_at DESC LIMIT 1
		`, connectionID, generation, fromDay, toDay)
		if existing, err := scanHistoryJob(row); err == nil {
			outJob = existing
			created = false
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		newID := id("syncjob")
		nowStr := now()
		_, err := tx.ExecContext(ctx, `
			INSERT INTO history_sync_jobs (
				id, connection_id, generation, range_from, range_to,
				status, current_day, pages_done, rows_seen, attempts,
				created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, 'QUEUED', ?, 0, 0, 0, ?, ?)
		`, newID, connectionID, generation, fromDay, toDay, fromDay, nowStr, nowStr)
		if err != nil {
			row := tx.QueryRowContext(ctx, `
				SELECT `+historyJobCols+` FROM history_sync_jobs
				WHERE connection_id = ? AND generation = ? AND range_from = ? AND range_to = ? AND status IN ('QUEUED', 'RUNNING')
				LIMIT 1
			`, connectionID, generation, fromDay, toDay)
			if raceExisting, qErr := scanHistoryJob(row); qErr == nil {
				outJob = raceExisting
				created = false
				return nil
			}
			return err
		}

		outJob = HistorySyncJob{
			ID:           newID,
			ConnectionID: connectionID,
			Generation:   generation,
			RangeFrom:    fromDay,
			RangeTo:      toDay,
			Status:       HistoryJobStatusQueued,
			CurrentDay:   fromDay,
			CreatedAt:    nowStr,
			UpdatedAt:    nowStr,
		}
		created = true
		return nil
	})

	return outJob, created, err
}

// ClaimNextHistorySyncJob atomically leases the next runnable QUEUED history job.
// Cancels jobs whose generation is stale compared to the active connection.
func (s *Store) ClaimNextHistorySyncJob(ctx context.Context, nowTime time.Time) (HistorySyncJob, bool, error) {
	var claimedJob HistorySyncJob
	var found bool

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		nowStr := nowTime.UTC().Format(time.RFC3339Nano)
		rows, err := tx.QueryContext(ctx, `
			SELECT j.id, j.connection_id, j.generation, c.generation,
			       j.range_from, j.range_to, j.status, j.current_day,
			       j.pages_done, j.rows_seen, j.attempts, j.next_attempt_at,
			       j.error_code, j.error_message, j.created_at, j.started_at,
			       j.heartbeat_at, j.finished_at, j.updated_at
			FROM history_sync_jobs j
			JOIN connections c ON c.id = j.connection_id
			WHERE j.status = 'QUEUED'
			  AND (j.next_attempt_at IS NULL OR j.next_attempt_at <= ?)
			ORDER BY j.created_at ASC, j.id ASC
			LIMIT 10
		`, nowStr)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var j HistorySyncJob
			var cGen int64
			var currentDay, nextAttempt, errCode, errMsg, startedAt, heartbeatAt, finishedAt sql.NullString
			if err := rows.Scan(
				&j.ID, &j.ConnectionID, &j.Generation, &cGen,
				&j.RangeFrom, &j.RangeTo, &j.Status, &currentDay,
				&j.PagesDone, &j.RowsSeen, &j.Attempts, &nextAttempt,
				&errCode, &errMsg, &j.CreatedAt, &startedAt,
				&heartbeatAt, &finishedAt, &j.UpdatedAt,
			); err != nil {
				continue
			}
			j.CurrentDay = currentDay.String
			j.NextAttemptAt = nextAttempt.String
			j.ErrorCode = errCode.String
			j.ErrorMessage = errMsg.String
			j.StartedAt = startedAt.String
			j.HeartbeatAt = heartbeatAt.String
			j.FinishedAt = finishedAt.String

			if j.Generation != cGen {
				nowTs := now()
				_, _ = tx.ExecContext(ctx, `
					UPDATE history_sync_jobs
					SET status = 'CANCELED', error_code = 'STALE_GENERATION',
					    error_message = 'Connection generation bumped before claim',
					    finished_at = ?, updated_at = ?
					WHERE id = ? AND status = 'QUEUED'
				`, nowTs, nowTs, j.ID)
				continue
			}

			claimTime := now()
			res, err := tx.ExecContext(ctx, `
				UPDATE history_sync_jobs
				SET status = 'RUNNING', attempts = attempts + 1,
				    started_at = COALESCE(started_at, ?), heartbeat_at = ?, updated_at = ?
				WHERE id = ? AND status = 'QUEUED'
			`, claimTime, claimTime, claimTime, j.ID)
			if err != nil {
				return err
			}
			if ra, _ := res.RowsAffected(); ra == 1 {
				j.Status = HistoryJobStatusRunning
				j.Attempts++
				if j.StartedAt == "" {
					j.StartedAt = claimTime
				}
				j.HeartbeatAt = claimTime
				j.UpdatedAt = claimTime
				claimedJob = j
				found = true
				return nil
			}
		}
		return nil
	})

	return claimedJob, found, err
}

// HeartbeatHistorySyncJob extends the job lease and records incremental progress.
// Fails closed if the connection generation has bumped or the job has been canceled.
func (s *Store) HeartbeatHistorySyncJob(ctx context.Context, jobID, currentDay string, pagesDone, rowsSeen int) error {
	if jobID == "" {
		return errors.New("job ID is required")
	}
	var fenceErr error
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.checkAndFenceJob(ctx, tx, jobID, HistoryJobStatusRunning); err != nil {
			fenceErr = err
			return nil
		}
		nowTs := now()
		_, err := tx.ExecContext(ctx, `
			UPDATE history_sync_jobs
			SET current_day = COALESCE(NULLIF(?, ''), current_day),
			    pages_done = ?, rows_seen = ?, heartbeat_at = ?, updated_at = ?
			WHERE id = ? AND status = 'RUNNING'
		`, currentDay, pagesDone, rowsSeen, nowTs, nowTs, jobID)
		return err
	})
	if err != nil {
		return err
	}
	return fenceErr
}

// RecordHistoryJobProgress updates job progress, extends heartbeat, and records history_coverage
// for the day within a single database transaction.
func (s *Store) RecordHistoryJobProgress(ctx context.Context, jobID string, currentDay string, pagesDone, rowsSeen int, dayCompleted bool, dayRows int) error {
	if jobID == "" {
		return errors.New("job ID is required")
	}
	var fenceErr error
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		connID, err := s.checkAndFenceJob(ctx, tx, jobID, HistoryJobStatusRunning)
		if err != nil {
			fenceErr = err
			return nil
		}
		nowTs := now()
		_, err = tx.ExecContext(ctx, `
			UPDATE history_sync_jobs
			SET current_day = COALESCE(NULLIF(?, ''), current_day),
			    pages_done = ?, rows_seen = ?, heartbeat_at = ?, updated_at = ?
			WHERE id = ? AND status = 'RUNNING'
		`, currentDay, pagesDone, rowsSeen, nowTs, nowTs, jobID)
		if err != nil {
			return err
		}

		if dayCompleted && currentDay != "" {
			covID := id("cov")
			_, err = tx.ExecContext(ctx, `
				INSERT INTO history_coverage(id, connection_id, day, status, last_sync_at, rows_seen)
				VALUES(?, ?, ?, 'COMPLETE', ?, ?)
				ON CONFLICT(connection_id, day) DO UPDATE SET
					status = 'COMPLETE', last_sync_at = excluded.last_sync_at, rows_seen = excluded.rows_seen
			`, covID, connID, currentDay, nowTs, dayRows)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return fenceErr
}

// RequeueHistorySyncJob transitions a RUNNING job back to QUEUED with backoff scheduling.
func (s *Store) RequeueHistorySyncJob(ctx context.Context, jobID, errorCode, errorMessage string, nextAttemptAt time.Time) error {
	if jobID == "" {
		return errors.New("job ID is required")
	}
	var fenceErr error
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.checkAndFenceJob(ctx, tx, jobID, HistoryJobStatusRunning); err != nil {
			fenceErr = err
			return nil
		}
		nowTs := now()
		sanitizedErr := sanitizeJobErrorMessage(errorMessage)
		nextAtStr := nextAttemptAt.UTC().Format(time.RFC3339Nano)
		_, err := tx.ExecContext(ctx, `
			UPDATE history_sync_jobs
			SET status = 'QUEUED', error_code = ?, error_message = ?, next_attempt_at = ?, updated_at = ?
			WHERE id = ? AND status = 'RUNNING'
		`, errorCode, sanitizedErr, nextAtStr, nowTs, jobID)
		return err
	})
	if err != nil {
		return err
	}
	return fenceErr
}

// CompleteHistorySyncJob transitions a RUNNING job to COMPLETED.
func (s *Store) CompleteHistorySyncJob(ctx context.Context, jobID string, pagesDone, rowsSeen int) error {
	if jobID == "" {
		return errors.New("job ID is required")
	}
	var fenceErr error
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM history_sync_jobs WHERE id = ?`, jobID).Scan(&status)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrJobNotFound
			}
			return err
		}
		if status == string(HistoryJobStatusCompleted) {
			return nil
		}

		if _, err := s.checkAndFenceJob(ctx, tx, jobID, HistoryJobStatusRunning); err != nil {
			fenceErr = err
			return nil
		}
		nowTs := now()
		_, err = tx.ExecContext(ctx, `
			UPDATE history_sync_jobs
			SET status = 'COMPLETED', pages_done = ?, rows_seen = ?,
			    error_code = NULL, error_message = NULL, finished_at = ?, updated_at = ?
			WHERE id = ? AND status = 'RUNNING'
		`, pagesDone, rowsSeen, nowTs, nowTs, jobID)
		return err
	})
	if err != nil {
		return err
	}
	return fenceErr
}

// FailHistorySyncJob transitions a QUEUED or RUNNING job to FAILED.
func (s *Store) FailHistorySyncJob(ctx context.Context, jobID, errorCode, errorMessage string) error {
	if jobID == "" {
		return errors.New("job ID is required")
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM history_sync_jobs WHERE id = ?`, jobID).Scan(&status)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrJobNotFound
			}
			return err
		}

		if status == string(HistoryJobStatusFailed) {
			return nil
		}
		if status == string(HistoryJobStatusCompleted) || status == string(HistoryJobStatusCanceled) {
			return fmt.Errorf("%w: cannot fail job in status %s", ErrJobTerminal, status)
		}

		nowTs := now()
		sanitizedErr := sanitizeJobErrorMessage(errorMessage)
		_, err = tx.ExecContext(ctx, `
			UPDATE history_sync_jobs
			SET status = 'FAILED', error_code = ?, error_message = ?, finished_at = ?, updated_at = ?
			WHERE id = ? AND status IN ('RUNNING', 'QUEUED')
		`, errorCode, sanitizedErr, nowTs, nowTs, jobID)
		return err
	})
}

// CancelHistorySyncJob transitions a QUEUED or RUNNING job to CANCELED.
func (s *Store) CancelHistorySyncJob(ctx context.Context, jobID string) error {
	if jobID == "" {
		return errors.New("job ID is required")
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM history_sync_jobs WHERE id = ?`, jobID).Scan(&status)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrJobNotFound
			}
			return err
		}

		if status == string(HistoryJobStatusCanceled) {
			return nil
		}
		if status == string(HistoryJobStatusCompleted) || status == string(HistoryJobStatusFailed) {
			return fmt.Errorf("%w: cannot cancel job in status %s", ErrJobTerminal, status)
		}

		nowTs := now()
		_, err = tx.ExecContext(ctx, `
			UPDATE history_sync_jobs
			SET status = 'CANCELED', finished_at = ?, updated_at = ?
			WHERE id = ? AND status IN ('QUEUED', 'RUNNING')
		`, nowTs, nowTs, jobID)
		return err
	})
}

// RequeueStaleHistorySyncJobs recovers jobs stuck in RUNNING state past their heartbeat deadline.
func (s *Store) RequeueStaleHistorySyncJobs(ctx context.Context, staleBefore time.Time) (int, error) {
	var count int

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		staleStr := staleBefore.UTC().Format(time.RFC3339Nano)
		type staleTarget struct {
			id         string
			isStaleGen bool
		}

		rows, err := tx.QueryContext(ctx, `
			SELECT j.id, j.generation, c.generation
			FROM history_sync_jobs j
			JOIN connections c ON c.id = j.connection_id
			WHERE j.status = 'RUNNING'
			  AND (
			    (j.heartbeat_at IS NOT NULL AND j.heartbeat_at < ?)
			    OR (j.heartbeat_at IS NULL AND j.started_at IS NOT NULL AND j.started_at < ?)
			    OR (j.heartbeat_at IS NULL AND j.started_at IS NULL AND j.updated_at < ?)
			  )
		`, staleStr, staleStr, staleStr)
		if err != nil {
			return err
		}
		defer rows.Close()

		var targets []staleTarget
		for rows.Next() {
			var jID string
			var jGen, cGen int64
			if err := rows.Scan(&jID, &jGen, &cGen); err == nil {
				targets = append(targets, staleTarget{id: jID, isStaleGen: jGen != cGen})
			}
		}
		_ = rows.Close()

		nowTs := now()
		requeuedCount := 0
		for _, t := range targets {
			if t.isStaleGen {
				_, _ = tx.ExecContext(ctx, `
					UPDATE history_sync_jobs
					SET status = 'CANCELED', error_code = 'STALE_GENERATION',
					    error_message = 'Connection generation bumped while job was running',
					    finished_at = ?, updated_at = ?
					WHERE id = ? AND status = 'RUNNING'
				`, nowTs, nowTs, t.id)
			} else {
				res, err := tx.ExecContext(ctx, `
					UPDATE history_sync_jobs
					SET status = 'QUEUED', error_code = 'STALE_HEARTBEAT_RECOVERED',
					    error_message = 'Requeued after stale heartbeat threshold exceeded',
					    next_attempt_at = NULL, updated_at = ?
					WHERE id = ? AND status = 'RUNNING'
				`, nowTs, t.id)
				if err == nil {
					if ra, _ := res.RowsAffected(); ra > 0 {
						requeuedCount += int(ra)
					}
				}
			}
		}
		count = requeuedCount
		return nil
	})

	return count, err
}

// RequeueRunningHistorySyncJobs recovers all RUNNING jobs back to QUEUED upon graceful worker shutdown.
func (s *Store) RequeueRunningHistorySyncJobs(ctx context.Context, reason string) (int, error) {
	var count int
	if reason == "" {
		reason = "Graceful worker shutdown"
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		type runningTarget struct {
			id         string
			isStaleGen bool
		}

		rows, err := tx.QueryContext(ctx, `
			SELECT j.id, j.generation, c.generation
			FROM history_sync_jobs j
			JOIN connections c ON c.id = j.connection_id
			WHERE j.status = 'RUNNING'
		`)
		if err != nil {
			return err
		}
		defer rows.Close()

		var targets []runningTarget
		for rows.Next() {
			var jID string
			var jGen, cGen int64
			if err := rows.Scan(&jID, &jGen, &cGen); err == nil {
				targets = append(targets, runningTarget{id: jID, isStaleGen: jGen != cGen})
			}
		}
		_ = rows.Close()

		nowTs := now()
		sanitizedReason := sanitizeJobErrorMessage(reason)
		requeuedCount := 0
		for _, t := range targets {
			if t.isStaleGen {
				_, _ = tx.ExecContext(ctx, `
					UPDATE history_sync_jobs
					SET status = 'CANCELED', error_code = 'STALE_GENERATION',
					    error_message = 'Connection generation bumped while job was running',
					    finished_at = ?, updated_at = ?
					WHERE id = ? AND status = 'RUNNING'
				`, nowTs, nowTs, t.id)
			} else {
				res, err := tx.ExecContext(ctx, `
					UPDATE history_sync_jobs
					SET status = 'QUEUED', error_code = 'WORKER_SHUTDOWN',
					    error_message = ?,
					    next_attempt_at = NULL, updated_at = ?
					WHERE id = ? AND status = 'RUNNING'
				`, sanitizedReason, nowTs, t.id)
				if err == nil {
					if ra, _ := res.RowsAffected(); ra > 0 {
						requeuedCount += int(ra)
					}
				}
			}
		}
		count = requeuedCount
		return nil
	})

	return count, err
}

// GetHistorySyncJob retrieves a job by ID.
func (s *Store) GetHistorySyncJob(ctx context.Context, jobID string) (HistorySyncJob, error) {
	if jobID == "" {
		return HistorySyncJob{}, errors.New("job ID is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+historyJobCols+` FROM history_sync_jobs WHERE id = ?`, jobID)
	j, err := scanHistoryJob(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HistorySyncJob{}, ErrJobNotFound
		}
		return HistorySyncJob{}, err
	}
	return j, nil
}

// CreateHistorySyncJob provides backwards compatibility for existing callers.
func (s *Store) CreateHistorySyncJob(ctx context.Context, connectionID, rangeFrom, rangeTo string) (string, error) {
	var gen int64
	err := s.db.QueryRowContext(ctx, `SELECT generation FROM connections WHERE id = ?`, connectionID).Scan(&gen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	job, _, err := s.CreateOrGetHistorySyncJob(ctx, connectionID, gen, rangeFrom, rangeTo)
	if err != nil {
		return "", err
	}
	return job.ID, nil
}

// GetLatestHistorySyncJob returns the most recent history sync job for a connection.
func (s *Store) GetLatestHistorySyncJob(ctx context.Context, connectionID string) (HistorySyncJob, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+historyJobCols+` FROM history_sync_jobs
		WHERE connection_id = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1
	`, connectionID)
	job, err := scanHistoryJob(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HistorySyncJob{}, false, nil
		}
		return HistorySyncJob{}, false, err
	}
	return job, true, nil
}
