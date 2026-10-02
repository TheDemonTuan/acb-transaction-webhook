package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var recoveryLocation = time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)
var errInvalidAutomaticCheckpoint = errors.New("INVALID_CHECKPOINT")

func (s *Store) IsAutomaticRecoveryRun(ctx context.Context, runID string) (bool, error) {
	var automatic bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recovery_runs r JOIN auth_recovery_episodes e ON e.connection_id=r.connection_id JOIN auth_attempts a ON a.id=e.attempt_id WHERE r.id=? AND r.generation=e.generation AND a.owner_subject=? AND (r.id=e.recovery_run_id OR r.event_key=e.attempt_id OR r.event_key LIKE e.attempt_id||':gap-tail:%') AND EXISTS(SELECT 1 FROM connections c WHERE c.id=r.connection_id AND c.generation=r.generation))`, runID, RecoveryOwner).Scan(&automatic)
	return automatic, err
}
func autoRecoveryPlanTx(ctx context.Context, tx *sql.Tx, e AuthRecoveryEpisode, at time.Time) (RecoveryRunPlan, error) {
	today := at.In(recoveryLocation).Format("2006-01-02")
	day, err := time.Parse("2006-01-02", today)
	if err != nil {
		return RecoveryRunPlan{}, err
	}
	from := day.AddDate(0, 0, -6)
	created, err := time.Parse(time.RFC3339Nano, e.CreatedAt)
	if err != nil || created.After(at) {
		return RecoveryRunPlan{}, errInvalidAutomaticCheckpoint
	}
	createdDay, err := time.Parse("2006-01-02", created.In(recoveryLocation).Format("2006-01-02"))
	if err != nil {
		return RecoveryRunPlan{}, errInvalidAutomaticCheckpoint
	}
	if createdDay.Before(from) {
		from = createdDay
	}
	var cpFrom, cpTo string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(coverage_from,''),COALESCE(coverage_to,'') FROM checkpoints WHERE connection_id=?`, e.ConnectionID).Scan(&cpFrom, &cpTo)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return RecoveryRunPlan{}, err
	}
	if err == nil {
		if cpTo == "" && cpFrom != "" {
			return RecoveryRunPlan{}, errInvalidAutomaticCheckpoint
		}
		if cpTo != "" {
			end, err := time.Parse("2006-01-02", cpTo)
			if err != nil || end.After(day) {
				return RecoveryRunPlan{}, errInvalidAutomaticCheckpoint
			}
			if cpFrom != "" {
				start, err := time.Parse("2006-01-02", cpFrom)
				if err != nil || start.After(end) {
					return RecoveryRunPlan{}, errInvalidAutomaticCheckpoint
				}
			}
			if end.Before(from) {
				from = end
			}
		}
	}
	if e.RequiredFrom != "" {
		required, err := time.Parse("2006-01-02", e.RequiredFrom)
		if err != nil || required.After(day) {
			return RecoveryRunPlan{}, errInvalidAutomaticCheckpoint
		}
		if required.Before(from) {
			from = required
		}
	}
	if e.RequiredTo != "" {
		required, err := time.Parse("2006-01-02", e.RequiredTo)
		if err != nil || required.After(day) {
			return RecoveryRunPlan{}, errInvalidAutomaticCheckpoint
		}
	}
	return RecoveryRunPlan{Reason: RecoveryReasonReauth, RangeFrom: from.Format("2006-01-02"), RangeTo: today, NextDay: from.Format("2006-01-02")}, nil
}
func completeAutomaticSessionTx(ctx context.Context, tx *sql.Tx, attemptID, connectionID string, generation int64, at time.Time) error {
	e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE attempt_id=? AND connection_id=? AND generation=? AND finished_at IS NULL`, attemptID, connectionID, generation))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkRecoveryCurrentTx(ctx, tx, e); err != nil {
		return err
	}
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(owner_subject,'') FROM auth_attempts WHERE id=?`, attemptID).Scan(&owner); err != nil {
		return err
	}
	if owner != RecoveryOwner {
		return ErrRecoverySuperseded
	}
	plan, planErr := autoRecoveryPlanTx(ctx, tx, e, at)
	if planErr != nil && !errors.Is(planErr, errInvalidAutomaticCheckpoint) {
		return planErr
	}
	run, err := scanRecoveryRun(tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE connection_id=? AND generation=? AND event_key=?`, connectionID, generation, attemptID))
	if err != nil {
		return err
	}
	state, reason := "CATCHING_UP", ""
	if planErr != nil {
		state, reason = "MANUAL_REQUIRED", "INVALID_CHECKPOINT"
		_, err = tx.ExecContext(ctx, `UPDATE recovery_runs SET status='FAILED',error_code='INVALID_CHECKPOINT',error_message='',finished_at=?,updated_at=? WHERE id=?`, now(), now(), run.ID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE recovery_runs SET reason=?,range_from=?,range_to=?,next_day=? WHERE id=?`, plan.Reason, plan.RangeFrom, plan.RangeTo, plan.NextDay, run.ID)
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET state=?,reason_code=?,recovery_run_id=?,required_from=NULLIF(?,''),required_to=NULLIF(?,''),updated_at=? WHERE id=? AND generation=? AND finished_at IS NULL`, state, reason, run.ID, plan.RangeFrom, plan.RangeTo, now(), e.ID, generation)
	if err != nil {
		return err
	}
	if err := invalidateRecoveryInputsTx(ctx, tx, e.ID); err != nil {
		return err
	}
	return enqueueRecoveryNoticeTx(ctx, tx, e, state)
}

// completeAutomaticRecoveryRunTx is worker-owned. A COMPLETED run alone never
// releases automatic recovery; every frozen gap day must have durable coverage.
func completeAutomaticRecoveryRunTx(ctx context.Context, tx *sql.Tx, run RecoveryRun, at time.Time) error {
	e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE connection_id=? AND generation=? AND recovery_run_id=? AND finished_at IS NULL`, run.ConnectionID, run.Generation, run.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkRecoveryCurrentTx(ctx, tx, e); err != nil {
		return err
	}
	from, err := time.Parse("2006-01-02", e.RequiredFrom)
	if err != nil {
		return ErrRecoveryPlanConflict
	}
	to, err := time.Parse("2006-01-02", e.RequiredTo)
	if err != nil || from.After(to) {
		return ErrRecoveryPlanConflict
	}
	runTo, err := time.Parse("2006-01-02", run.RangeTo)
	if err != nil || runTo.After(to) {
		return ErrRecoveryPlanConflict
	}
	if run.NextDay == "" || run.NextDay <= run.RangeTo {
		return ErrRecoveryPlanConflict
	}
	var expected, count int
	expected = int(to.Sub(from)/(24*time.Hour)) + 1
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM history_coverage WHERE connection_id=? AND day>=? AND day<=? AND status='COMPLETE'`, e.ConnectionID, e.RequiredFrom, e.RequiredTo).Scan(&count); err != nil {
		return err
	}
	if count != expected {
		return ErrRecoveryPlanConflict
	}
	today := at.In(recoveryLocation).Format("2006-01-02")
	current, err := time.Parse("2006-01-02", today)
	if err != nil || current.Before(to) {
		return ErrRecoveryPlanConflict
	}
	if current.After(to) {
		next := to.AddDate(0, 0, 1).Format("2006-01-02")
		eventKey := e.AttemptID + ":gap-tail:" + today
		extension, _, err := insertRecoveryRunTx(ctx, tx, e.ConnectionID, e.Generation, eventKey, RecoveryRunStatusPending, now(), RecoveryRunPlan{Reason: RecoveryReasonReauth, RangeFrom: next, RangeTo: today, NextDay: next})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET state='CATCHING_UP',required_to=?,recovery_run_id=?,updated_at=? WHERE id=? AND generation=? AND finished_at IS NULL`, today, extension.ID, now(), e.ID, e.Generation)
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET state='COMPLETED',reason_code='',finished_at=?,updated_at=? WHERE id=? AND generation=? AND finished_at IS NULL`, now(), now(), e.ID, e.Generation)
	if err != nil {
		return err
	}
	if err := invalidateRecoveryInputsTx(ctx, tx, e.ID); err != nil {
		return err
	}
	return enqueueRecoveryNoticeTx(ctx, tx, e, "COMPLETED")
}
