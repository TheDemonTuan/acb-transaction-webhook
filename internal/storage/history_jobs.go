package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type HistorySyncJob struct {
	ID           string `json:"id"`
	ConnectionID string `json:"connectionId"`
	RangeFrom    string `json:"rangeFrom"`
	RangeTo      string `json:"rangeTo"`
	Status       string `json:"status"` // RUNNING, COMPLETED, FAILED
	RowsSeen     int    `json:"rowsSeen"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
}

func (s *Store) CreateHistorySyncJob(ctx context.Context, connectionID, rangeFrom, rangeTo string) (string, error) {
	jobID := id("syncjob")
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO history_sync_jobs (id, connection_id, range_from, range_to, status, rows_seen, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'RUNNING', 0, ?, ?)
	`, jobID, connectionID, rangeFrom, rangeTo, nowStr, nowStr)
	if err != nil {
		return "", err
	}
	return jobID, nil
}

func (s *Store) CompleteHistorySyncJob(ctx context.Context, jobID string, rowsSeen int, syncErr error) error {
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	status := "COMPLETED"
	var errMsg *string
	if syncErr != nil {
		status = "FAILED"
		msg := syncErr.Error()
		errMsg = &msg
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE history_sync_jobs
		SET status = ?, rows_seen = ?, error_message = ?, updated_at = ?
		WHERE id = ?
	`, status, rowsSeen, errMsg, nowStr, jobID)
	return err
}

func (s *Store) GetLatestHistorySyncJob(ctx context.Context, connectionID string) (HistorySyncJob, bool, error) {
	var job HistorySyncJob
	var errMsg sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, connection_id, range_from, range_to, status, rows_seen, error_message, created_at, updated_at
		FROM history_sync_jobs
		WHERE connection_id = ?
		ORDER BY created_at DESC, rowid DESC
		LIMIT 1
	`, connectionID).Scan(&job.ID, &job.ConnectionID, &job.RangeFrom, &job.RangeTo, &job.Status, &job.RowsSeen, &errMsg, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HistorySyncJob{}, false, nil
		}
		return HistorySyncJob{}, false, err
	}
	if errMsg.Valid {
		job.ErrorMessage = errMsg.String
	}
	return job, true, nil
}
