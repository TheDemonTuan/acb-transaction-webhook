package storage

import (
	"context"
	"database/sql"
	"errors"
)

type PollRun struct {
	ID            string `json:"id"`
	ConnectionID  string `json:"connectionId"`
	Generation    int64  `json:"generation"`
	Status        string `json:"status"`
	Classifier    string `json:"classifier,omitempty"`
	HTTPStatus    int    `json:"httpStatus,omitempty"`
	Pages         int    `json:"pages"`
	RowsSeen      int    `json:"rowsSeen"`
	Error         string `json:"error,omitempty"`
	StartedAt     string `json:"startedAt"`
	FinishedAt    string `json:"finishedAt,omitempty"`
	AuthConfirmed bool   `json:"-"`
}

func (s *Store) LastSuccessfulPoll(ctx context.Context, connectionID string) (PollRun, error) {
	var poll PollRun
	err := s.db.QueryRowContext(ctx, `
		SELECT id,connection_id,generation,status,classifier,http_status,pages,rows_seen,
		       COALESCE(sanitized_error,''),started_at,COALESCE(finished_at,'')
		FROM poll_runs
		WHERE connection_id=? AND status='SUCCEEDED'
		ORDER BY finished_at DESC
		LIMIT 1
	`, connectionID).Scan(
		&poll.ID, &poll.ConnectionID, &poll.Generation, &poll.Status, &poll.Classifier,
		&poll.HTTPStatus, &poll.Pages, &poll.RowsSeen, &poll.Error, &poll.StartedAt, &poll.FinishedAt,
	)
	return poll, err
}

func (s *Store) StartPoll(ctx context.Context) (PollRun, error) {
	connection, err := s.Connection(ctx)
	if err != nil {
		return PollRun{}, err
	}
	if connection.State != "MONITORING" {
		return PollRun{}, errors.New("connection is not monitoring")
	}
	poll := PollRun{ID: id("poll"), ConnectionID: connection.ID, Generation: connection.Generation, Status: "RUNNING", StartedAt: now()}
	_, err = s.db.ExecContext(ctx, `INSERT INTO poll_runs(id,connection_id,generation,status,started_at) VALUES(?,?,?,?,?)`, poll.ID, poll.ConnectionID, poll.Generation, poll.Status, poll.StartedAt)
	return poll, err
}

func (s *Store) FinishPoll(ctx context.Context, poll PollRun) error {
	if poll.Status != "SUCCEEDED" && poll.Status != "FAILED" && poll.Status != "AUTH_REQUIRED" && poll.Status != "PROTOCOL_CHANGED" && poll.Status != "PARTIAL" {
		return errors.New("invalid poll status")
	}
	if poll.Status == "AUTH_REQUIRED" && !poll.AuthConfirmed {
		return errors.New("cannot finish poll as AUTH_REQUIRED without confirmed auth proof")
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE poll_runs SET status=?,classifier=?,http_status=?,pages=?,rows_seen=?,sanitized_error=?,finished_at=? WHERE id=? AND status='RUNNING'`, poll.Status, poll.Classifier, poll.HTTPStatus, poll.Pages, poll.RowsSeen, poll.Error, now(), poll.ID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return sql.ErrNoRows
		}
		if poll.Status == "SUCCEEDED" {
			result, err = tx.ExecContext(ctx, `UPDATE connections SET updated_at=? WHERE id=? AND generation=? AND state='MONITORING'`, now(), poll.ConnectionID, poll.Generation)
			if err != nil {
				return err
			}
			changed, err = result.RowsAffected()
			if err != nil || changed != 1 {
				return ErrGenerationFenceMismatch
			}
			return nil
		}
		if poll.Status == "AUTH_REQUIRED" {
			result, err = tx.ExecContext(ctx, `UPDATE connections SET state=?,generation=generation+1,updated_at=? WHERE id=? AND generation=? AND state='MONITORING'`, poll.Status, now(), poll.ConnectionID, poll.Generation)
			if err != nil {
				return err
			}
			changed, err = result.RowsAffected()
			if err != nil || changed != 1 {
				return ErrGenerationFenceMismatch
			}
			return nil
		}
		// FAILED or PROTOCOL_CHANGED do not invalidate the authenticated session; connection remains MONITORING
		return nil
	})
}
