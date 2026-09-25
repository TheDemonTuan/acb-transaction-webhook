package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RecoveryRunStatus is the durable state of a recovery intent.
type RecoveryRunStatus string

const (
	RecoveryRunStatusPending   RecoveryRunStatus = "PENDING"
	RecoveryRunStatusRunning   RecoveryRunStatus = "RUNNING"
	RecoveryRunStatusCompleted RecoveryRunStatus = "COMPLETED"
	RecoveryRunStatusSucceeded RecoveryRunStatus = RecoveryRunStatusCompleted // compatibility alias
	RecoveryRunStatusFailed    RecoveryRunStatus = "FAILED"
	RecoveryRunStatusCanceled  RecoveryRunStatus = "CANCELED"
	RecoveryRunStatusCancelled RecoveryRunStatus = RecoveryRunStatusCanceled // compatibility alias
)

const (
	RecoveryReasonInitialAuth = "INITIAL_AUTH_BOOTSTRAP"
	RecoveryReasonReauth      = "SESSION_REAUTH_CATCHUP"
	RecoveryReasonStartup     = "WORKER_STARTUP"
	RecoveryReasonAuthLegacy  = "SESSION_AUTHENTICATED" // compatibility alias
)

var (
	ErrRecoveryRunTerminal     = errors.New("recovery run is already terminal")
	ErrRecoveryRunInvalidState = errors.New("invalid recovery run state")
	ErrRecoveryPlanConflict    = errors.New("recovery plan conflicts with existing run")
)

// RecoveryRunPlan is the immutable, non-sensitive recovery window and resume cursor.
type RecoveryRunPlan struct {
	Reason    string `json:"reason,omitempty"`
	RangeFrom string `json:"rangeFrom,omitempty"`
	RangeTo   string `json:"rangeTo,omitempty"`
	NextDay   string `json:"nextDay,omitempty"`
}

// RecoveryRun stores recovery intent and non-sensitive progress metadata.
type RecoveryRun struct {
	ID           string            `json:"id"`
	ConnectionID string            `json:"connectionId"`
	Generation   int64             `json:"generation"`
	EventKey     string            `json:"eventKey"`
	Reason       string            `json:"reason,omitempty"`
	RangeFrom    string            `json:"rangeFrom,omitempty"`
	RangeTo      string            `json:"rangeTo,omitempty"`
	NextDay      string            `json:"nextDay,omitempty"`
	Status       RecoveryRunStatus `json:"status"`
	ProgressJSON string            `json:"progressJson"`
	ErrorCode    string            `json:"errorCode,omitempty"`
	ErrorMessage string            `json:"errorMessage,omitempty"`
	ClaimedAt    string            `json:"claimedAt,omitempty"`
	CreatedAt    string            `json:"createdAt"`
	UpdatedAt    string            `json:"updatedAt"`
	FinishedAt   string            `json:"finishedAt,omitempty"`
}

const recoveryRunColumns = `id, connection_id, generation, event_key, reason, range_from, range_to, next_day, status,
	progress_json, error_code, error_message, claimed_at, created_at, updated_at, finished_at`

type recoveryRunScanner interface {
	Scan(dest ...any) error
}

func scanRecoveryRun(row recoveryRunScanner) (RecoveryRun, error) {
	var run RecoveryRun
	var status string
	var errorCode, errorMessage, claimedAt, finishedAt sql.NullString
	if err := row.Scan(
		&run.ID, &run.ConnectionID, &run.Generation, &run.EventKey,
		&run.Reason, &run.RangeFrom, &run.RangeTo, &run.NextDay, &status,
		&run.ProgressJSON, &errorCode, &errorMessage, &claimedAt,
		&run.CreatedAt, &run.UpdatedAt, &finishedAt,
	); err != nil {
		return RecoveryRun{}, err
	}
	run.Status = RecoveryRunStatus(status)
	run.ErrorCode = errorCode.String
	run.ErrorMessage = errorMessage.String
	run.ClaimedAt = claimedAt.String
	run.FinishedAt = finishedAt.String
	return run, nil
}

func validateRecoveryRunKey(connectionID, eventKey string, generation int64) error {
	if connectionID == "" {
		return errors.New("connection ID is required")
	}
	if eventKey == "" {
		return errors.New("recovery event key is required")
	}
	if generation < 0 {
		return errors.New("recovery generation must not be negative")
	}
	return nil
}

func validRecoveryRunStatus(status RecoveryRunStatus) bool {
	switch status {
	case RecoveryRunStatusPending, RecoveryRunStatusRunning,
		RecoveryRunStatusCompleted, RecoveryRunStatusFailed, RecoveryRunStatusCanceled:
		return true
	default:
		return false
	}
}

func ensureCurrentGeneration(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, connectionID string, generation int64) error {
	var currentGeneration int64
	if err := q.QueryRowContext(ctx, `SELECT generation FROM connections WHERE id=?`, connectionID).Scan(&currentGeneration); err != nil {
		return err
	}
	if currentGeneration != generation {
		return fmt.Errorf("%w: expected generation %d, current is %d", ErrGenerationFenceMismatch, generation, currentGeneration)
	}
	return nil
}

func insertRecoveryRunTx(ctx context.Context, tx *sql.Tx, connectionID string, generation int64, eventKey string, status RecoveryRunStatus, createdAt string, plan RecoveryRunPlan) (RecoveryRun, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE connection_id=? AND generation=? AND event_key=?`, connectionID, generation, eventKey)
	if existing, err := scanRecoveryRun(row); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return RecoveryRun{}, false, err
	}
	run := RecoveryRun{
		ID:           id("recovery"),
		ConnectionID: connectionID,
		Generation:   generation,
		EventKey:     eventKey,
		Reason:       plan.Reason,
		RangeFrom:    plan.RangeFrom,
		RangeTo:      plan.RangeTo,
		NextDay:      plan.NextDay,
		Status:       status,
		ProgressJSON: "{}",
		CreatedAt:    createdAt,
		UpdatedAt:    createdAt,
	}
	if _, err := tx.ExecContext(ctx, `
				INSERT INTO recovery_runs(id, connection_id, generation, event_key, reason, range_from, range_to, next_day, status, progress_json, created_at, updated_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
			`, run.ID, run.ConnectionID, run.Generation, run.EventKey, run.Reason, run.RangeFrom, run.RangeTo, run.NextDay, run.Status, run.ProgressJSON, run.CreatedAt, run.UpdatedAt); err != nil {

		row := tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE connection_id=? AND generation=? AND event_key=?`, connectionID, generation, eventKey)
		if existing, scanErr := scanRecoveryRun(row); scanErr == nil {
			return existing, false, nil
		}
		return RecoveryRun{}, false, err
	}
	return run, true, nil
}

// EnsureRecoveryRun creates one PENDING intent for a connection generation and event key.
// Repeated calls return the existing intent without changing its progress or status.
func (s *Store) EnsureRecoveryRun(ctx context.Context, connectionID string, generation int64, eventKey string) (RecoveryRun, bool, error) {
	return s.EnsureRecoveryRunWithPlan(ctx, connectionID, generation, eventKey, RecoveryRunPlan{})
}

// EnsureRecoveryRunWithPlan creates or hydrates one durable recovery plan.
func (s *Store) EnsureRecoveryRunWithPlan(ctx context.Context, connectionID string, generation int64, eventKey string, plan RecoveryRunPlan) (RecoveryRun, bool, error) {
	if err := validateRecoveryRunKey(connectionID, eventKey, generation); err != nil {
		return RecoveryRun{}, false, err
	}
	if err := validateRecoveryPlanDates(plan); err != nil {
		return RecoveryRun{}, false, err
	}
	var run RecoveryRun
	var created bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := ensureCurrentGeneration(ctx, tx, connectionID, generation); err != nil {
			return err
		}
		var err error
		run, created, err = insertRecoveryRunTx(ctx, tx, connectionID, generation, eventKey, RecoveryRunStatusPending, now(), plan)
		if err != nil || created {
			return err
		}
		if err := validateRecoveryPlanCompatibility(run, plan); err != nil {
			return err
		}
		if recoveryPlanNeedsHydration(run, plan) {
			run, err = updateRecoveryPlanTx(ctx, tx, run.ID, connectionID, generation, plan)
		}
		return err
	})
	return run, created, err
}

func validateRecoveryPlanCompatibility(run RecoveryRun, plan RecoveryRunPlan) error {
	// next_day is mutable progress; only the reason and window are immutable.
	for _, pair := range [][2]string{{run.Reason, plan.Reason}, {run.RangeFrom, plan.RangeFrom}, {run.RangeTo, plan.RangeTo}} {
		if pair[0] != "" && pair[1] != "" && pair[0] != pair[1] {
			return ErrRecoveryPlanConflict
		}
	}
	return nil
}

func recoveryPlanNeedsHydration(run RecoveryRun, plan RecoveryRunPlan) bool {
	return (run.Reason == "" && plan.Reason != "") || (run.RangeFrom == "" && plan.RangeFrom != "") || (run.RangeTo == "" && plan.RangeTo != "") || (run.NextDay == "" && plan.NextDay != "")
}

func updateRecoveryPlanTx(ctx context.Context, tx *sql.Tx, runID, connectionID string, generation int64, plan RecoveryRunPlan) (RecoveryRun, error) {
	_, err := tx.ExecContext(ctx, `UPDATE recovery_runs SET reason=CASE WHEN reason='' THEN ? ELSE reason END, range_from=CASE WHEN range_from='' THEN ? ELSE range_from END, range_to=CASE WHEN range_to='' THEN ? ELSE range_to END, next_day=CASE WHEN next_day='' THEN ? ELSE next_day END, updated_at=? WHERE id=? AND connection_id=? AND generation=? AND status IN ('PENDING','RUNNING')`, plan.Reason, plan.RangeFrom, plan.RangeTo, plan.NextDay, now(), runID, connectionID, generation)
	if err != nil {
		return RecoveryRun{}, err
	}
	return scanRecoveryRun(tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE id=?`, runID))
}

func validateRecoveryPlanDates(plan RecoveryRunPlan) error {
	for _, value := range []string{plan.RangeFrom, plan.RangeTo, plan.NextDay} {
		if value == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", value); err != nil {
			return fmt.Errorf("invalid recovery plan date: %w", err)
		}
	}
	if plan.RangeFrom != "" && plan.RangeTo != "" && plan.RangeFrom > plan.RangeTo {
		return errors.New("recovery plan range is reversed")
	}
	return nil
}

// SetRecoveryRunPlan hydrates a pending legacy intent before its first upstream request.
func (s *Store) SetRecoveryRunPlan(ctx context.Context, runID, connectionID string, generation int64, plan RecoveryRunPlan) (RecoveryRun, error) {
	if runID == "" {
		return RecoveryRun{}, errors.New("recovery run ID is required")
	}
	if err := validateRecoveryRunKey(connectionID, "event", generation); err != nil {
		return RecoveryRun{}, err
	}
	if err := validateRecoveryPlanDates(plan); err != nil {
		return RecoveryRun{}, err
	}
	var run RecoveryRun
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := ensureCurrentGeneration(ctx, tx, connectionID, generation); err != nil {
			return err
		}
		current, err := scanRecoveryRun(tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE id=? AND connection_id=? AND generation=?`, runID, connectionID, generation))
		if err != nil {
			return err
		}
		if current.Status != RecoveryRunStatusPending && current.Status != RecoveryRunStatusRunning {
			return ErrRecoveryRunTerminal
		}
		if err := validateRecoveryPlanCompatibility(current, plan); err != nil {
			return err
		}
		run, err = updateRecoveryPlanTx(ctx, tx, runID, connectionID, generation, plan)
		return err
	})
	return run, err
}

// AdvanceRecoveryRunDay durably moves the next unprocessed day.
func (s *Store) AdvanceRecoveryRunDay(ctx context.Context, runID, connectionID string, generation int64, nextDay string) (RecoveryRun, error) {
	if runID == "" {
		return RecoveryRun{}, errors.New("recovery run ID is required")
	}
	if err := validateRecoveryRunKey(connectionID, "event", generation); err != nil {
		return RecoveryRun{}, err
	}
	if _, err := time.Parse("2006-01-02", nextDay); err != nil {
		return RecoveryRun{}, err
	}
	var run RecoveryRun
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := ensureCurrentGeneration(ctx, tx, connectionID, generation); err != nil {
			return err
		}
		current, err := scanRecoveryRun(tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE id=? AND connection_id=? AND generation=?`, runID, connectionID, generation))
		if err != nil {
			return err
		}
		if current.Status != RecoveryRunStatusPending && current.Status != RecoveryRunStatusRunning {
			return ErrRecoveryRunTerminal
		}
		if current.NextDay != "" && nextDay < current.NextDay {
			return ErrRecoveryPlanConflict
		}
		result, err := tx.ExecContext(ctx, `UPDATE recovery_runs SET next_day=?, updated_at=? WHERE id=? AND connection_id=? AND generation=? AND status IN ('PENDING','RUNNING')`, nextDay, now(), runID, connectionID, generation)
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
		run, err = scanRecoveryRun(tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE id=?`, runID))
		return err
	})
	return run, err
}

// ClaimRecoveryRun atomically claims a PENDING intent. Repeating a claim for a RUNNING
// intent is idempotent; all mutations are fenced to the supplied connection generation.
func (s *Store) ClaimRecoveryRun(ctx context.Context, runID, connectionID string, generation int64) (RecoveryRun, error) {
	if runID == "" {
		return RecoveryRun{}, errors.New("recovery run ID is required")
	}
	if err := validateRecoveryRunKey(connectionID, "event", generation); err != nil {
		return RecoveryRun{}, err
	}
	var run RecoveryRun
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := ensureCurrentGeneration(ctx, tx, connectionID, generation); err != nil {
			return err
		}
		var err error
		run, err = scanRecoveryRun(tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE id=? AND connection_id=? AND generation=?`, runID, connectionID, generation))
		if err != nil {
			return err
		}
		if run.Status == RecoveryRunStatusRunning {
			return nil
		}
		if run.Status != RecoveryRunStatusPending {
			return ErrRecoveryRunTerminal
		}
		claimedAt := now()
		result, err := tx.ExecContext(ctx, `
				UPDATE recovery_runs SET status='RUNNING', claimed_at=?, updated_at=?
				WHERE id=? AND connection_id=? AND generation=? AND status='PENDING'
				AND EXISTS (SELECT 1 FROM connections WHERE id=? AND generation=?)
			`, claimedAt, claimedAt, runID, connectionID, generation, connectionID, generation)
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
		run, err = scanRecoveryRun(tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE id=?`, runID))
		return err
	})
	return run, err
}

// ClaimRecoveryRunByEvent atomically claims the event-keyed PENDING intent.
// Repeating a claim for a RUNNING intent is idempotent.
func (s *Store) ClaimRecoveryRunByEvent(ctx context.Context, connectionID string, generation int64, eventKey string) (RecoveryRun, error) {
	run, err := s.GetRecoveryRunByEvent(ctx, connectionID, generation, eventKey)
	if err != nil {
		return RecoveryRun{}, err
	}
	return s.ClaimRecoveryRun(ctx, run.ID, connectionID, generation)
}

// ListPendingRecoveryRuns lists PENDING intents for the current connection generation.
func (s *Store) ListPendingRecoveryRuns(ctx context.Context, connectionID string, generation int64) ([]RecoveryRun, error) {
	return s.listRecoveryRuns(ctx, connectionID, generation, "PENDING")
}

// ListOpenRecoveryRuns lists PENDING and RUNNING intents for the current connection generation.
func (s *Store) ListOpenRecoveryRuns(ctx context.Context, connectionID string, generation int64) ([]RecoveryRun, error) {
	return s.listRecoveryRuns(ctx, connectionID, generation, "PENDING','RUNNING")
}

func (s *Store) listRecoveryRuns(ctx context.Context, connectionID string, generation int64, statuses string) ([]RecoveryRun, error) {
	if err := validateRecoveryRunKey(connectionID, "event", generation); err != nil {
		return nil, err
	}
	if err := ensureCurrentGeneration(ctx, s.db, connectionID, generation); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE connection_id=? AND generation=? AND status IN ('`+statuses+`') ORDER BY created_at ASC, id ASC`, connectionID, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := make([]RecoveryRun, 0)
	for rows.Next() {
		run, err := scanRecoveryRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// GetRecoveryRunByEvent gets an intent by connection, generation, and event key.
func (s *Store) GetRecoveryRunByEvent(ctx context.Context, connectionID string, generation int64, eventKey string) (RecoveryRun, error) {
	if err := validateRecoveryRunKey(connectionID, eventKey, generation); err != nil {
		return RecoveryRun{}, err
	}
	if err := ensureCurrentGeneration(ctx, s.db, connectionID, generation); err != nil {
		return RecoveryRun{}, err
	}
	return scanRecoveryRun(s.db.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE connection_id=? AND generation=? AND event_key=?`, connectionID, generation, eventKey))
}

func sanitizeRecoveryErrorMessage(msg string) string {
	return sanitizeStoredErrorMessage(msg)
}

// UpdateRecoveryRunProgress atomically updates status and non-sensitive progress metadata.
// The update fails when the connection generation no longer matches.
func (s *Store) UpdateRecoveryRunProgress(ctx context.Context, runID, connectionID string, generation int64, status RecoveryRunStatus, progressJSON, errorCode, errorMessage string) (RecoveryRun, error) {
	if runID == "" {
		return RecoveryRun{}, errors.New("recovery run ID is required")
	}
	if err := validateRecoveryRunKey(connectionID, "event", generation); err != nil {
		return RecoveryRun{}, err
	}
	if !validRecoveryRunStatus(status) {
		return RecoveryRun{}, ErrRecoveryRunInvalidState
	}
	if progressJSON == "" {
		progressJSON = "{}"
	}
	if !json.Valid([]byte(progressJSON)) {
		return RecoveryRun{}, errors.New("recovery progress must be valid JSON")
	}
	errorCode = sanitizeRecoveryErrorMessage(errorCode)
	errorMessage = sanitizeRecoveryErrorMessage(errorMessage)

	var run RecoveryRun
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := ensureCurrentGeneration(ctx, tx, connectionID, generation); err != nil {
			return err
		}
		var err error
		run, err = scanRecoveryRun(tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE id=? AND connection_id=? AND generation=?`, runID, connectionID, generation))
		if err != nil {
			return err
		}
		if run.Status == RecoveryRunStatusCompleted || run.Status == RecoveryRunStatusFailed || run.Status == RecoveryRunStatusCanceled {
			return ErrRecoveryRunTerminal
		}
		if status == RecoveryRunStatusPending && run.Status != RecoveryRunStatusPending {
			return ErrRecoveryRunInvalidState
		}
		if status == RecoveryRunStatusRunning && run.Status != RecoveryRunStatusPending && run.Status != RecoveryRunStatusRunning {
			return ErrRecoveryRunInvalidState
		}
		if (status == RecoveryRunStatusCompleted || status == RecoveryRunStatusFailed || status == RecoveryRunStatusCanceled) && run.Status != RecoveryRunStatusRunning {
			return ErrRecoveryRunInvalidState
		}
		updatedAt := now()
		var finishedAt any
		if status == RecoveryRunStatusCompleted || status == RecoveryRunStatusFailed || status == RecoveryRunStatusCanceled {
			finishedAt = updatedAt
		}
		result, err := tx.ExecContext(ctx, `
				UPDATE recovery_runs
				SET status=?, progress_json=?, error_code=NULLIF(?,''), error_message=NULLIF(?,''), updated_at=?, finished_at=?
				WHERE id=? AND connection_id=? AND generation=?
				AND EXISTS (SELECT 1 FROM connections WHERE id=? AND generation=?)
			`, status, progressJSON, errorCode, errorMessage, updatedAt, finishedAt, runID, connectionID, generation, connectionID, generation)
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
		run, err = scanRecoveryRun(tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE id=?`, runID))
		return err
	})
	return run, err
}
