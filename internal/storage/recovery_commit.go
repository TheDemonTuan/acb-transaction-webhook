package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RecoveryDayCommit atomically commits one complete recovery day and its durable cursor.
type RecoveryDayCommit struct {
	RunID        string
	ConnectionID string
	Generation   int64
	Day          string
	NextDay      string
	RowsSeen     int
	ScanID       string
	CoverageFrom string
}

// RecoveryDayAdvance advances a covered day without refreshing its coverage TTL.
type RecoveryDayAdvance struct {
	RunID        string
	ConnectionID string
	Generation   int64
	Day          string
	NextDay      string
	ScanID       string
	CoverageFrom string
}

func validateRecoveryDayCommit(c RecoveryDayCommit) error {
	if c.RunID == "" || c.ConnectionID == "" || c.Generation <= 0 {
		return errors.New("recovery day commit identity is required")
	}
	for _, date := range []string{c.Day, c.NextDay, c.CoverageFrom} {
		if date == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", date); err != nil {
			return fmt.Errorf("invalid recovery day date: %w", err)
		}
	}
	if c.Day == "" || c.NextDay == "" {
		return errors.New("recovery day and next day are required")
	}
	return nil
}

func fenceRecoveryWrite(ctx context.Context, tx *sql.Tx, connectionID string, generation int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE connections SET updated_at=updated_at WHERE id=? AND generation=?`, connectionID, generation)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrGenerationFenceMismatch
	}
	return nil
}

func loadRecoveryRunTx(ctx context.Context, tx *sql.Tx, runID, connectionID string, generation int64) (RecoveryRun, error) {
	return scanRecoveryRun(tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE id=? AND connection_id=? AND generation=?`, runID, connectionID, generation))
}

func commitRecoveryCheckpointTx(ctx context.Context, tx *sql.Tx, c RecoveryDayCommit) error {
	return upsertRecoveryCheckpointTx(ctx, tx, c.ConnectionID, c.ScanID, c.CoverageFrom, c.Day)
}

func upsertRecoveryCheckpointTx(ctx context.Context, tx *sql.Tx, connectionID, scanID, coverageFrom, coverageTo string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO checkpoints(connection_id, scan_id, coverage_from, coverage_to, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(connection_id) DO UPDATE SET
			scan_id=excluded.scan_id,
			coverage_from=excluded.coverage_from,
			coverage_to=excluded.coverage_to,
			updated_at=excluded.updated_at
		WHERE checkpoints.coverage_to='' OR checkpoints.coverage_to <= excluded.coverage_to
	`, connectionID, scanID, coverageFrom, coverageTo, now())
	return err
}

func updateRecoveryNextDayTx(ctx context.Context, tx *sql.Tx, c RecoveryDayCommit) error {
	_, err := tx.ExecContext(ctx, `UPDATE recovery_runs SET next_day=?, updated_at=? WHERE id=? AND connection_id=? AND generation=? AND status IN ('PENDING','RUNNING')`, c.NextDay, now(), c.RunID, c.ConnectionID, c.Generation)
	return err
}

func validateRecoveryRunDay(run RecoveryRun, day string) error {
	if run.Status != RecoveryRunStatusPending && run.Status != RecoveryRunStatusRunning {
		return ErrRecoveryRunTerminal
	}
	if run.NextDay != "" && day != run.NextDay {
		return ErrRecoveryPlanConflict
	}
	return nil
}

// CommitRecoveryDay atomically writes coverage, monotonic checkpoint, and recovery next_day.
func (s *Store) CommitRecoveryDay(ctx context.Context, c RecoveryDayCommit) (RecoveryRun, error) {
	if err := validateRecoveryDayCommit(c); err != nil {
		return RecoveryRun{}, err
	}
	var run RecoveryRun
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := fenceRecoveryWrite(ctx, tx, c.ConnectionID, c.Generation); err != nil {
			return err
		}
		var err error
		run, err = loadRecoveryRunTx(ctx, tx, c.RunID, c.ConnectionID, c.Generation)
		if err != nil {
			return err
		}
		if err := validateRecoveryRunDay(run, c.Day); err != nil {
			return err
		}
		stamp := now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO history_coverage(id, connection_id, day, status, last_sync_at, rows_seen)
			VALUES(?, ?, ?, 'COMPLETE', ?, ?)
			ON CONFLICT(connection_id, day) DO UPDATE SET status='COMPLETE', last_sync_at=excluded.last_sync_at, rows_seen=excluded.rows_seen
		`, id("cov"), c.ConnectionID, c.Day, stamp, c.RowsSeen); err != nil {
			return err
		}
		if err := commitRecoveryCheckpointTx(ctx, tx, c); err != nil {
			return err
		}
		if err := updateRecoveryNextDayTx(ctx, tx, c); err != nil {
			return err
		}
		run, err = loadRecoveryRunTx(ctx, tx, c.RunID, c.ConnectionID, c.Generation)
		return err
	})
	return run, err
}

// AdvanceRecoveryDay moves the durable cursor for a day already covered by a prior run.
func (s *Store) AdvanceRecoveryDay(ctx context.Context, c RecoveryDayAdvance) (RecoveryRun, error) {
	commit := RecoveryDayCommit{RunID: c.RunID, ConnectionID: c.ConnectionID, Generation: c.Generation, Day: c.Day, NextDay: c.NextDay, ScanID: c.ScanID, CoverageFrom: c.CoverageFrom}
	if err := validateRecoveryDayCommit(commit); err != nil {
		return RecoveryRun{}, err
	}
	var run RecoveryRun
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := fenceRecoveryWrite(ctx, tx, c.ConnectionID, c.Generation); err != nil {
			return err
		}
		var err error
		run, err = loadRecoveryRunTx(ctx, tx, c.RunID, c.ConnectionID, c.Generation)
		if err != nil {
			return err
		}
		if err := validateRecoveryRunDay(run, c.Day); err != nil {
			return err
		}
		if err := upsertRecoveryCheckpointTx(ctx, tx, c.ConnectionID, c.ScanID, c.CoverageFrom, c.Day); err != nil {
			return err
		}
		if err := updateRecoveryNextDayTx(ctx, tx, commit); err != nil {
			return err
		}
		run, err = loadRecoveryRunTx(ctx, tx, c.RunID, c.ConnectionID, c.Generation)
		return err
	})
	return run, err
}
