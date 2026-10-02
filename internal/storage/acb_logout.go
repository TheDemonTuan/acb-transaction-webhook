package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrACBLogoutFence = errors.New("ACB_LOGOUT_FENCE_CONFLICT")

// SessionEnvelope is a revoke-only snapshot, never a monitoring restore source.
type ACBLogoutJob struct {
	ID, ConnectionID, State, ReasonCode            string
	OldGeneration, FencedGeneration                int64
	AttemptID                                      string
	SessionGeneration                              int64  `json:"-"`
	SessionEnvelope                                []byte `json:"-"`
	SessionKeyID                                   string `json:"-"`
	LocalClearedAt, BankStatus, BankReasonCode     string
	StatusMessageID                                int64
	NoticeStatus, CreatedAt, UpdatedAt, FinishedAt string
}

const logoutColumns = `id,connection_id,old_generation,fenced_generation,COALESCE(attempt_id,''),COALESCE(session_generation,0),session_envelope,COALESCE(session_key_id,''),state,reason_code,COALESCE(local_cleared_at,''),bank_status,bank_reason_code,COALESCE(status_message_id,0),notice_status,created_at,updated_at,COALESCE(finished_at,'')`

func scanLogoutJob(row recoveryScanner) (ACBLogoutJob, error) {
	var j ACBLogoutJob
	err := row.Scan(&j.ID, &j.ConnectionID, &j.OldGeneration, &j.FencedGeneration, &j.AttemptID, &j.SessionGeneration, &j.SessionEnvelope, &j.SessionKeyID, &j.State, &j.ReasonCode, &j.LocalClearedAt, &j.BankStatus, &j.BankReasonCode, &j.StatusMessageID, &j.NoticeStatus, &j.CreatedAt, &j.UpdatedAt, &j.FinishedAt)
	return j, err
}

func checkNoACBLogoutTx(ctx context.Context, tx *sql.Tx, connectionID string) error {
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM acb_logout_jobs WHERE connection_id=? AND finished_at IS NULL)`, connectionID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return ErrACBSessionBusy
	}
	return nil
}

func createACBLogoutTx(ctx context.Context, tx *sql.Tx, botID int64, connectionID string, generation int64) error {
	if err := checkNoACBLogoutTx(ctx, tx, connectionID); err != nil {
		return err
	}
	j := ACBLogoutJob{ID: id("logout"), ConnectionID: connectionID, OldGeneration: generation, FencedGeneration: generation + 1}
	err := tx.QueryRowContext(ctx, `SELECT generation,envelope,key_id FROM sessions WHERE connection_id=?`, connectionID).Scan(&j.SessionGeneration, &j.SessionEnvelope, &j.SessionKeyID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	err = tx.QueryRowContext(ctx, `SELECT id FROM auth_attempts WHERE connection_id=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING') ORDER BY created_at DESC LIMIT 1`, connectionID).Scan(&j.AttemptID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	t := now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO acb_logout_jobs(id,connection_id,old_generation,fenced_generation,attempt_id,session_generation,session_envelope,session_key_id,state,created_at,updated_at) VALUES(?,?,?,?,NULLIF(?,''),?,?,NULLIF(?,''),'PENDING',?,?)`, j.ID, connectionID, generation, j.FencedGeneration, j.AttemptID, j.SessionGeneration, j.SessionEnvelope, j.SessionKeyID, t, t); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE connections SET generation=generation+1,state='AUTH_REQUIRED',updated_at=? WHERE id=? AND generation=?`, t, connectionID, generation)
	if err != nil {
		return err
	}
	if err := requireRecoveryRow(result, ErrACBLogoutFence); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE telegram_auth_state SET paused=1,updated_at=? WHERE bot_id=?`, t, botID); err != nil {
		return err
	}
	if err := supersedeACBInputsTx(ctx, tx, connectionID, "OPERATOR_LOGOUT"); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE acb_credential_grants SET status='REVOKED' WHERE connection_id=? AND status='PENDING'`, connectionID)
	return err
}

// CheckACBLogoutFence accepts only the current open logout job, including repeat
// invalidations for a job whose bank outcome is known but local clear is pending.
func (s *Store) CheckACBLogoutFence(ctx context.Context, connectionID string, generation int64) error {
	var valid bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connections c JOIN acb_logout_jobs j ON j.connection_id=c.id WHERE c.id=? AND c.generation=? AND c.state NOT IN ('MONITORING','AUTH_STARTING') AND j.fenced_generation=c.generation AND j.finished_at IS NULL)`, connectionID, generation).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrACBLogoutFence
	}
	return nil
}

func (s *Store) OpenACBLogoutJob(ctx context.Context) (ACBLogoutJob, error) {
	return scanLogoutJob(s.db.QueryRowContext(ctx, `SELECT `+logoutColumns+` FROM acb_logout_jobs WHERE finished_at IS NULL ORDER BY created_at LIMIT 1`))
}
func (s *Store) ACBLogoutJob(ctx context.Context, jobID string) (ACBLogoutJob, error) {
	return scanLogoutJob(s.db.QueryRowContext(ctx, `SELECT `+logoutColumns+` FROM acb_logout_jobs WHERE id=?`, jobID))
}

// aggregateLogoutTx keeps the bank result and local acknowledgement independent.
func aggregateLogoutTx(ctx context.Context, tx *sql.Tx, jobID string) error {
	j, err := scanLogoutJob(tx.QueryRowContext(ctx, `SELECT `+logoutColumns+` FROM acb_logout_jobs WHERE id=?`, jobID))
	if err != nil {
		return err
	}
	state := "CLEARING"
	var finished any
	if j.LocalClearedAt != "" {
		switch j.BankStatus {
		case "CONFIRMED", "ALREADY_EXPIRED":
			state = "COMPLETED"
			finished = now()
		case "UNCONFIRMED":
			state = "LOCAL_ONLY"
			finished = now()
		case "IN_FLIGHT":
			state = "REVOKING"
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE acb_logout_jobs SET state=?,reason_code=?,finished_at=?,updated_at=?,notice_status=CASE WHEN state<>? THEN 'PENDING' ELSE notice_status END WHERE id=?`, state, j.BankReasonCode, finished, now(), state, jobID)
	return err
}

func (s *Store) MarkACBLogoutLocalCleared(ctx context.Context, jobID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE acb_logout_jobs SET local_cleared_at=?,updated_at=? WHERE id=? AND finished_at IS NULL AND local_cleared_at IS NULL AND EXISTS(SELECT 1 FROM connections c WHERE c.id=acb_logout_jobs.connection_id AND c.generation=acb_logout_jobs.fenced_generation AND c.state NOT IN ('MONITORING','AUTH_STARTING'))`, now(), now(), jobID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrACBLogoutFence
		}
		return aggregateLogoutTx(ctx, tx, jobID)
	})
}

func (s *Store) BeginACBLogoutRevocation(ctx context.Context, jobID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE acb_logout_jobs SET bank_status='IN_FLIGHT',state=CASE WHEN local_cleared_at IS NULL THEN 'CLEARING' ELSE 'REVOKING' END,updated_at=? WHERE id=? AND finished_at IS NULL AND bank_status='PENDING' AND EXISTS(SELECT 1 FROM connections c WHERE c.id=acb_logout_jobs.connection_id AND c.generation=acb_logout_jobs.fenced_generation AND c.state NOT IN ('MONITORING','AUTH_STARTING'))`, now(), jobID)
		if err != nil {
			return err
		}
		return requireRecoveryRow(result, ErrACBLogoutFence)
	})
}

func (s *Store) FinishACBLogoutRevocation(ctx context.Context, jobID, status, reason string) error {
	if status != "CONFIRMED" && status != "ALREADY_EXPIRED" && status != "UNCONFIRMED" {
		return ErrACBLogoutFence
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE acb_logout_jobs SET bank_status=?,bank_reason_code=?,session_envelope=NULL,session_key_id=NULL,session_generation=NULL,notice_status='PENDING',updated_at=? WHERE id=? AND finished_at IS NULL AND bank_status IN ('PENDING','IN_FLIGHT')`, status, reason, now(), jobID)
		if err != nil {
			return err
		}
		if err := requireRecoveryRow(result, ErrACBLogoutFence); err != nil {
			return err
		}
		return aggregateLogoutTx(ctx, tx, jobID)
	})
}

// RecoverACBLogoutRevocation never replays an uncertain bank click. The five
// minute window also bounds snapshot retention when invalidation is offline.
func (s *Store) RecoverACBLogoutRevocation(ctx context.Context, jobID string, at time.Time) error {
	j, err := s.ACBLogoutJob(ctx, jobID)
	if err != nil {
		return err
	}
	if j.FinishedAt != "" {
		return nil
	}
	if j.BankStatus == "IN_FLIGHT" {
		return s.FinishACBLogoutRevocation(ctx, jobID, "UNCONFIRMED", "LOGOUT_OUTCOME_UNKNOWN")
	}
	created, err := time.Parse(time.RFC3339Nano, j.CreatedAt)
	if err != nil || !at.Before(created.Add(5*time.Minute)) {
		if j.BankStatus == "PENDING" {
			return s.FinishACBLogoutRevocation(ctx, jobID, "UNCONFIRMED", "REVOKE_WINDOW_EXPIRED")
		}
	}
	return nil
}

func (s *Store) PendingACBLogoutNotices(ctx context.Context) ([]ACBLogoutJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+logoutColumns+` FROM acb_logout_jobs WHERE notice_status='PENDING' AND bank_status IN ('CONFIRMED','ALREADY_EXPIRED','UNCONFIRMED') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []ACBLogoutJob
	for rows.Next() {
		j, err := scanLogoutJob(rows)
		if err != nil {
			return nil, err
		}
		j.SessionEnvelope = nil
		j.SessionKeyID = ""
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// Delivery acknowledgement cannot hide a newer local or bank result.
func (s *Store) FinishACBLogoutNotice(ctx context.Context, jobID string, messageID int64, observedUpdatedAt string) error {
	if messageID <= 0 || observedUpdatedAt == "" {
		return ErrChallengeMismatch
	}
	query := `UPDATE acb_logout_jobs SET notice_status='SENT',status_message_id=? WHERE id=? AND notice_status='PENDING' AND updated_at=?`
	args := []any{messageID, jobID, observedUpdatedAt}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	return requireRecoveryRow(result, ErrACBLogoutFence)
}
