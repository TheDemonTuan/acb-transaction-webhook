package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrRecoverySuperseded      = errors.New("authentication recovery superseded")
	ErrRecoveryBudgetExhausted = errors.New("authentication recovery budget exhausted")
	ErrRecoveryPaused          = errors.New("authentication recovery is paused")
	ErrRecoveryNotReady        = errors.New("authentication recovery is not ready")
	ErrRecoveryCooldown        = errors.New("authentication recovery login cooldown")
	ErrChallengeExpired        = errors.New("authentication challenge expired")
	ErrChallengeConsumed       = errors.New("authentication challenge already consumed")
	ErrChallengeMismatch       = errors.New("authentication challenge mismatch")
)

const RecoveryOwner = "system:acb-recovery"

type AuthRecoveryEpisode struct {
	ID, ConnectionID                                                                string
	TriggerGeneration, Generation, ConfigRevision                                   int64
	AttemptID, State                                                                string
	AttemptCount, BudgetStartCount, CaptchaSubmissions, AIUsed, OTPSubmissions      int
	NextAttemptAt, LastLoginAt, RequiredFrom, RequiredTo, ReasonCode, RecoveryRunID string
	StatusMessageID                                                                 int64
	CreatedAt, UpdatedAt, FinishedAt                                                string
}

const episodeColumns = `id,connection_id,trigger_generation,generation,config_revision,COALESCE(attempt_id,''),state,attempt_count,budget_start_count,captcha_submissions,ai_used,otp_submissions,COALESCE(next_attempt_at,''),COALESCE(last_login_at,''),COALESCE(required_from,''),COALESCE(required_to,''),reason_code,COALESCE(recovery_run_id,''),COALESCE(status_message_id,0),created_at,updated_at,COALESCE(finished_at,'')`

type recoveryScanner interface{ Scan(...any) error }

func scanEpisode(row recoveryScanner) (AuthRecoveryEpisode, error) {
	var e AuthRecoveryEpisode
	err := row.Scan(&e.ID, &e.ConnectionID, &e.TriggerGeneration, &e.Generation, &e.ConfigRevision, &e.AttemptID, &e.State, &e.AttemptCount, &e.BudgetStartCount, &e.CaptchaSubmissions, &e.AIUsed, &e.OTPSubmissions, &e.NextAttemptAt, &e.LastLoginAt, &e.RequiredFrom, &e.RequiredTo, &e.ReasonCode, &e.RecoveryRunID, &e.StatusMessageID, &e.CreatedAt, &e.UpdatedAt, &e.FinishedAt)
	return e, err
}
func (s *Store) AuthRecoveryEpisode(ctx context.Context, episodeID string) (AuthRecoveryEpisode, error) {
	return scanEpisode(s.db.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, episodeID))
}
func (s *Store) LatestAuthRecoveryEpisode(ctx context.Context, connectionID string) (AuthRecoveryEpisode, error) {
	return scanEpisode(s.db.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE connection_id=? ORDER BY created_at DESC,rowid DESC LIMIT 1`, connectionID))
}
func enqueueRecoveryNoticeTx(ctx context.Context, tx *sql.Tx, e AuthRecoveryEpisode, event string) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO auth_recovery_notices(id,episode_id,event_key,kind,status,created_at) VALUES(?,?,?,?,'PENDING',?)`, id("arn"), e.ID, fmt.Sprintf("%s:%s:%d", e.ID, event, e.AttemptCount), event, now())
	return err
}
func ensureRecoveryEpisodeTx(ctx context.Context, tx *sql.Tx, connectionID string, generation int64) (AuthRecoveryEpisode, error) {
	var current, revision int64
	if err := tx.QueryRowContext(ctx, `SELECT generation,config_revision FROM connections WHERE id=?`, connectionID).Scan(&current, &revision); err != nil {
		return AuthRecoveryEpisode{}, err
	}
	if current != generation {
		return AuthRecoveryEpisode{}, ErrRecoverySuperseded
	}
	// Historical trigger identity is immutable, even after terminal completion.
	e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE connection_id=? AND trigger_generation=?`, connectionID, generation))
	if err == nil {
		if e.FinishedAt == "" && (e.Generation != generation || e.ConfigRevision != revision) {
			return e, ErrRecoverySuperseded
		}
		return e, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return e, err
	}
	e, err = scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE connection_id=? AND (generation=? OR finished_at IS NULL) ORDER BY finished_at IS NULL DESC,created_at DESC LIMIT 1`, connectionID, generation))
	if err == nil {
		if e.Generation != generation || e.ConfigRevision != revision {
			return e, ErrRecoverySuperseded
		}
		return e, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return e, err
	}
	e = AuthRecoveryEpisode{ID: id("are"), ConnectionID: connectionID, TriggerGeneration: generation, Generation: generation, ConfigRevision: revision, State: "DETECTED", CreatedAt: now(), UpdatedAt: now()}
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_recovery_episodes(id,connection_id,trigger_generation,generation,config_revision,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, e.ID, e.ConnectionID, e.TriggerGeneration, e.Generation, e.ConfigRevision, e.State, e.CreatedAt, e.UpdatedAt)
	if err != nil {
		return e, err
	}
	return e, enqueueRecoveryNoticeTx(ctx, tx, e, "DETECTED")
}
func (s *Store) EnsureAuthRecoveryEpisode(ctx context.Context, connectionID string, generation int64) (AuthRecoveryEpisode, error) {
	var e AuthRecoveryEpisode
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		e, err = ensureRecoveryEpisodeTx(ctx, tx, connectionID, generation)
		return err
	})
	return e, err
}

// RequireSessionRecovery invalidates only the generation whose local session was
// proven unusable. Operational failures never call this method.
func (s *Store) RequireSessionRecovery(ctx context.Context, connectionID string, generation int64, reasonCode string) error {
	if reasonCode != "SESSION_MISSING" && reasonCode != "SESSION_INVALID" && reasonCode != "SESSION_DECRYPT_FAILED" {
		return errors.New("invalid local session recovery reason")
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE connections SET state='AUTH_REQUIRED',generation=generation+1,updated_at=? WHERE id=? AND generation=? AND state='MONITORING'`, now(), connectionID, generation)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrGenerationFenceMismatch
		}
		// Preserve an automatic episode across an internal loss, including catch-up.
		if err := advanceRecoveryAfterAuthLossTx(ctx, tx, connectionID, generation, reasonCode); err != nil {
			return err
		}
		e, err := ensureRecoveryEpisodeTx(ctx, tx, connectionID, generation+1)
		if err != nil {
			return err
		}
		state := "DETECTED"
		if reasonCode == "SESSION_DECRYPT_FAILED" {
			state = "MANUAL_REQUIRED"
		}
		if e.FinishedAt != "" {
			return nil
		}
		if state == "DETECTED" && e.AttemptCount > 0 {
			state = recoveryRetryState(e)
		}
		_, err = tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET state=?,reason_code=?,updated_at=? WHERE id=?`, state, reasonCode, now(), e.ID)
		if err != nil {
			return err
		}
		return enqueueRecoveryNoticeTx(ctx, tx, e, state)
	})
}

func requireRecoveryRow(result sql.Result, conflict error) error {
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return conflict
	}
	return nil
}

func recoveryTerminal(state string) bool {
	return state == "COMPLETED" || state == "SUPERSEDED" || state == "CANCELLED"
}

func validRecoveryState(state string) bool {
	switch state {
	case "DETECTED", "STARTING", "LOGIN", "WAITING_CAPTCHA", "WAITING_OTP", "VERIFYING", "CATCHING_UP", "RETRY_WAIT", "WAIT_OPERATOR", "MAINTENANCE_WAIT", "MANUAL_REQUIRED", "COMPLETED", "SUPERSEDED", "CANCELLED":
		return true
	}
	return false
}

func checkRecoveryCurrentTx(ctx context.Context, tx *sql.Tx, e AuthRecoveryEpisode) error {
	var matches bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connections WHERE id=? AND generation=? AND config_revision=?)`, e.ConnectionID, e.Generation, e.ConfigRevision).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return ErrRecoverySuperseded
	}
	return nil
}

func checkRecoveryUnpausedTx(ctx context.Context, tx *sql.Tx) error {
	var paused bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_auth_state WHERE paused=1)`).Scan(&paused); err != nil {
		return err
	}
	if paused {
		return ErrRecoveryPaused
	}
	return nil
}

func checkRecoveryAttemptTx(ctx context.Context, tx *sql.Tx, e AuthRecoveryEpisode) error {
	if e.FinishedAt != "" {
		return ErrRecoverySuperseded
	}
	if err := checkRecoveryCurrentTx(ctx, tx, e); err != nil {
		return err
	}
	var expiry string
	err := tx.QueryRowContext(ctx, `SELECT a.expires_at FROM auth_attempts a JOIN connections c ON c.id=a.connection_id WHERE a.id=? AND a.connection_id=? AND a.generation=? AND a.owner_subject=? AND a.status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING') AND c.state='AUTH_STARTING'`, e.AttemptID, e.ConnectionID, e.Generation, RecoveryOwner).Scan(&expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRecoverySuperseded
	}
	if err != nil {
		return err
	}
	expires, err := time.Parse(time.RFC3339Nano, expiry)
	if err != nil {
		return err
	}
	if !time.Now().Before(expires) {
		return ErrChallengeExpired
	}
	return checkRecoveryUnpausedTx(ctx, tx)
}

func invalidateRecoveryInputsTx(ctx context.Context, tx *sql.Tx, episodeID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE auth_challenges SET status='INVALIDATED' WHERE episode_id=? AND status IN ('DELIVERING','PENDING','CONSUMING')`, episodeID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE telegram_auth_actions SET status='INVALIDATED' WHERE episode_id=? AND status='PENDING'`, episodeID)
	return err
}

func supersedeAuthRecoveryTx(ctx context.Context, tx *sql.Tx, e AuthRecoveryEpisode) error {
	if e.FinishedAt != "" {
		return nil
	}
	// Only the linked old attempt may be closed; never mutate the new connection.
	if e.AttemptID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE auth_attempts SET status='CANCELLED',finished_at=? WHERE id=? AND connection_id=? AND generation=? AND owner_subject=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING')`, now(), e.AttemptID, e.ConnectionID, e.Generation, RecoveryOwner); err != nil {
			return err
		}
	}
	if err := invalidateRecoveryInputsTx(ctx, tx, e.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET state='SUPERSEDED',reason_code='EXTERNAL_CHANGE',updated_at=?,finished_at=? WHERE id=? AND finished_at IS NULL`, now(), now(), e.ID)
	if err != nil {
		return err
	}
	return enqueueRecoveryNoticeTx(ctx, tx, e, "SUPERSEDED")
}

func (s *Store) SupersedeAuthRecovery(ctx context.Context, episodeID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, episodeID))
		if err != nil {
			return err
		}
		if e.FinishedAt != "" {
			return nil
		}
		if err := checkRecoveryCurrentTx(ctx, tx, e); err == nil {
			return ErrRecoveryNotReady
		} else if !errors.Is(err, ErrRecoverySuperseded) {
			return err
		}
		return supersedeAuthRecoveryTx(ctx, tx, e)
	})
}

func recoveryRetryState(e AuthRecoveryEpisode) string {
	if e.AttemptCount-e.BudgetStartCount >= 3 {
		return "MANUAL_REQUIRED"
	}
	return "RETRY_WAIT"
}

// Called only after the connection's confirmed auth-loss CAS has succeeded.
func advanceRecoveryAfterAuthLossTx(ctx context.Context, tx *sql.Tx, connectionID string, generation int64, reason string) error {
	e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes e WHERE connection_id=? AND generation=? AND finished_at IS NULL AND config_revision=(SELECT config_revision FROM connections WHERE id=e.connection_id) AND (EXISTS(SELECT 1 FROM auth_attempts a WHERE a.id=e.attempt_id AND a.connection_id=e.connection_id AND a.generation=e.generation AND a.owner_subject=?) OR EXISTS(SELECT 1 FROM recovery_runs r WHERE r.id=e.recovery_run_id AND r.connection_id=e.connection_id AND r.generation=e.generation))`, connectionID, generation, RecoveryOwner))
	if errors.Is(err, sql.ErrNoRows) {
		stale, lookupErr := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE connection_id=? AND finished_at IS NULL`, connectionID))
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return nil
		}
		if lookupErr != nil {
			return lookupErr
		}
		return supersedeAuthRecoveryTx(ctx, tx, stale)
	}
	if err != nil {
		return err
	}
	if err := invalidateRecoveryInputsTx(ctx, tx, e.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_attempts SET status='FAILED',finished_at=? WHERE id=? AND generation=? AND owner_subject=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING')`, now(), e.AttemptID, generation, RecoveryOwner); err != nil {
		return err
	}
	state := recoveryRetryState(e)
	_, err = tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET generation=?,state=?,reason_code=?,next_attempt_at=?,recovery_run_id=NULL,updated_at=? WHERE id=? AND generation=? AND finished_at IS NULL`, generation+1, state, reason, time.Now().UTC().Add(30*time.Second).Format(time.RFC3339Nano), now(), e.ID, generation)
	if err != nil {
		return err
	}
	e.Generation = generation + 1
	return enqueueRecoveryNoticeTx(ctx, tx, e, state)
}

func (s *Store) StartRecoveryAuthAttempt(ctx context.Context, episodeID string, expectedGeneration int64, ttl time.Duration) (AuthAttempt, error) {
	var attempt AuthAttempt
	if ttl <= 0 {
		return attempt, errors.New("auth attempt TTL must be positive")
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, episodeID))
		if err != nil {
			return err
		}
		if e.Generation != expectedGeneration || e.FinishedAt != "" {
			return ErrRecoverySuperseded
		}
		if err := checkRecoveryCurrentTx(ctx, tx, e); err != nil {
			return err
		}
		if err := checkRecoveryUnpausedTx(ctx, tx); err != nil {
			return err
		}
		if e.AttemptCount-e.BudgetStartCount >= 3 {
			return ErrRecoveryBudgetExhausted
		}
		if e.State != "DETECTED" && e.State != "RETRY_WAIT" && e.State != "MAINTENANCE_WAIT" {
			return ErrRecoveryNotReady
		}
		if err := recoveryDeadline(e.NextAttemptAt); err != nil {
			return err
		}
		if err := recoveryLoginCooldown(e.LastLoginAt); err != nil {
			return err
		}
		var state, masked string
		if err := tx.QueryRowContext(ctx, `SELECT state,COALESCE(account_masked,'') FROM connections WHERE id=?`, e.ConnectionID).Scan(&state, &masked); err != nil {
			return err
		}
		if state != "AUTH_REQUIRED" && !(state == "UNCONFIGURED" && masked != "" && e.ReasonCode == "OPERATOR_CONFIRMED") {
			return ErrRecoveryNotReady
		}
		attempt, err = s.startAuthAttemptTx(ctx, tx, e.ConnectionID, e.Generation, RecoveryOwner, ttl)
		if err != nil {
			return err
		}
		if err := invalidateRecoveryInputsTx(ctx, tx, e.ID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET generation=?,attempt_id=?,state='STARTING',attempt_count=attempt_count+1,captcha_submissions=0,otp_submissions=0,ai_used=0,next_attempt_at=NULL,reason_code='',recovery_run_id=NULL,updated_at=? WHERE id=? AND generation=? AND finished_at IS NULL`, attempt.Generation, attempt.ID, now(), e.ID, e.Generation)
		if err != nil {
			return err
		}
		if err := requireRecoveryRow(result, ErrRecoverySuperseded); err != nil {
			return err
		}
		e.AttemptCount++
		return enqueueRecoveryNoticeTx(ctx, tx, e, "STARTING")
	})
	return attempt, err
}

func recoveryDeadline(value string) error {
	if value == "" {
		return nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return err
	}
	if time.Now().Before(deadline) {
		return ErrRecoveryNotReady
	}
	return nil
}

func recoveryLoginCooldown(value string) error {
	if value == "" {
		return nil
	}
	last, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return err
	}
	if time.Now().Before(last.Add(time.Minute)) {
		return ErrRecoveryCooldown
	}
	return nil
}

func finishRecoveryAuthAttemptTx(ctx context.Context, tx *sql.Tx, e AuthRecoveryEpisode, attemptStatus, toState, reasonCode string, nextAttemptAt time.Time) error {
	switch toState {
	case "RETRY_WAIT", "WAIT_OPERATOR", "MAINTENANCE_WAIT", "MANUAL_REQUIRED", "CANCELLED":
	default:
		return ErrRecoveryNotReady
	}
	if e.FinishedAt != "" {
		return ErrRecoverySuperseded
	}
	if err := checkRecoveryCurrentTx(ctx, tx, e); err != nil {
		return err
	}
	var linked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_attempts WHERE id=? AND connection_id=? AND generation=? AND owner_subject=?)`, e.AttemptID, e.ConnectionID, e.Generation, RecoveryOwner).Scan(&linked); err != nil {
		return err
	}
	if !linked {
		return ErrRecoverySuperseded
	}
	if err := finishAuthAttemptTx(ctx, tx, e.AttemptID, attemptStatus); err != nil {
		return err
	}
	if (toState == "RETRY_WAIT" || toState == "MAINTENANCE_WAIT") && e.AttemptCount-e.BudgetStartCount >= 3 {
		toState = "MANUAL_REQUIRED"
		reasonCode = "ATTEMPT_BUDGET_EXHAUSTED"
	}
	var finished, next any
	if recoveryTerminal(toState) {
		finished = now()
	}
	if !nextAttemptAt.IsZero() {
		next = nextAttemptAt.UTC().Format(time.RFC3339Nano)
	}
	if err := invalidateRecoveryInputsTx(ctx, tx, e.ID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET generation=generation+1,state=?,reason_code=?,next_attempt_at=?,updated_at=?,finished_at=? WHERE id=? AND generation=? AND finished_at IS NULL`, toState, reasonCode, next, now(), finished, e.ID, e.Generation)
	if err != nil {
		return err
	}
	if err := requireRecoveryRow(result, ErrRecoverySuperseded); err != nil {
		return err
	}
	e.Generation++
	return enqueueRecoveryNoticeTx(ctx, tx, e, toState)
}

func (s *Store) FinishRecoveryAuthAttempt(ctx context.Context, episodeID string, generation int64, attemptStatus, toState, reasonCode string, nextAttemptAt time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, episodeID))
		if err != nil {
			return err
		}
		if e.Generation != generation {
			return ErrRecoverySuperseded
		}
		return finishRecoveryAuthAttemptTx(ctx, tx, e, attemptStatus, toState, reasonCode, nextAttemptAt)
	})
}

func (s *Store) TransitionAuthRecovery(ctx context.Context, episodeID string, generation int64, fromState, toState, reasonCode string) error {
	if !validRecoveryState(toState) || toState == "COMPLETED" || toState == "SUPERSEDED" {
		return errors.New("invalid recovery transition")
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, episodeID))
		if err != nil {
			return err
		}
		if e.Generation != generation || e.State != fromState || e.FinishedAt != "" {
			return ErrRecoverySuperseded
		}
		if err := checkRecoveryCurrentTx(ctx, tx, e); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_attempts WHERE id=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING'))`, e.AttemptID).Scan(&active); err != nil {
			return err
		}
		browserState := toState == "STARTING" || toState == "LOGIN" || toState == "WAITING_CAPTCHA" || toState == "WAITING_OTP" || toState == "VERIFYING"
		if active || browserState {
			if recoveryTerminal(toState) {
				return ErrRecoveryNotReady
			}
			if err := checkRecoveryAttemptTx(ctx, tx, e); err != nil {
				return err
			}
		}
		if toState == "CANCELLED" && e.RecoveryRunID != "" {
			return ErrRecoveryNotReady
		}
		if toState == "CATCHING_UP" {
			var committed bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM recovery_runs r JOIN sessions s ON s.connection_id=r.connection_id AND s.generation=r.generation JOIN auth_attempts a ON a.id=? AND a.generation=r.generation AND a.status='VERIFIED' WHERE r.id=? AND r.connection_id=? AND r.generation=? AND s.verified_at IS NOT NULL)`, e.AttemptID, e.RecoveryRunID, e.ConnectionID, e.Generation).Scan(&committed); err != nil {
				return err
			}
			if !committed {
				return ErrRecoveryNotReady
			}
		}
		var finished any
		if recoveryTerminal(toState) {
			finished = now()
		}
		if recoveryTerminal(toState) || toState == "WAIT_OPERATOR" || toState == "MANUAL_REQUIRED" {
			if err := invalidateRecoveryInputsTx(ctx, tx, e.ID); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET state=?,reason_code=?,updated_at=?,finished_at=? WHERE id=? AND generation=? AND state=? AND finished_at IS NULL`, toState, reasonCode, now(), finished, e.ID, generation, fromState)
		if err != nil {
			return err
		}
		if err := requireRecoveryRow(result, ErrRecoverySuperseded); err != nil {
			return err
		}
		return enqueueRecoveryNoticeTx(ctx, tx, e, toState)
	})
}

func rearmAuthRecoveryTx(ctx context.Context, tx *sql.Tx, e AuthRecoveryEpisode) error {
	if err := checkRecoveryCurrentTx(ctx, tx, e); err != nil {
		return err
	}
	switch e.State {
	case "WAIT_OPERATOR", "MANUAL_REQUIRED", "RETRY_WAIT", "MAINTENANCE_WAIT", "CANCELLED", "DETECTED":
	default:
		return ErrRecoveryNotReady
	}
	if e.RecoveryRunID != "" {
		return ErrRecoveryNotReady
	}
	if err := recoveryLoginCooldown(e.LastLoginAt); err != nil {
		return err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_attempts WHERE connection_id=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING'))`, e.ConnectionID).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrAuthAttemptActive
	}
	var state, masked string
	if err := tx.QueryRowContext(ctx, `SELECT state,COALESCE(account_masked,'') FROM connections WHERE id=?`, e.ConnectionID).Scan(&state, &masked); err != nil {
		return err
	}
	if state != "AUTH_REQUIRED" && !(state == "UNCONFIGURED" && masked != "") {
		return ErrRecoveryNotReady
	}
	if err := invalidateRecoveryInputsTx(ctx, tx, e.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET state='DETECTED',finished_at=NULL,budget_start_count=attempt_count,next_attempt_at=NULL,reason_code='OPERATOR_CONFIRMED',updated_at=? WHERE id=? AND generation=?`, now(), e.ID, e.Generation)
	return err
}

func (s *Store) RearmAuthRecovery(ctx context.Context, episodeID string, generation int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, episodeID))
		if err != nil {
			return err
		}
		if e.Generation != generation {
			return ErrRecoverySuperseded
		}
		return rearmAuthRecoveryTx(ctx, tx, e)
	})
}

func (s *Store) ClaimRecoveryAI(ctx context.Context, episodeID string, generation int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, episodeID))
		if err != nil {
			return err
		}
		if e.Generation != generation {
			return ErrRecoverySuperseded
		}
		if err := checkRecoveryAttemptTx(ctx, tx, e); err != nil {
			return err
		}
		if e.State != "LOGIN" && e.State != "WAITING_CAPTCHA" {
			return ErrRecoveryNotReady
		}
		result, err := tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET ai_used=1,updated_at=? WHERE id=? AND ai_used=0`, now(), e.ID)
		if err != nil {
			return err
		}
		if err := requireRecoveryRow(result, ErrRecoveryBudgetExhausted); err != nil {
			return err
		}
		return enqueueRecoveryNoticeTx(ctx, tx, e, "AI_READING")
	})
}

func (s *Store) ReserveRecoverySubmission(ctx context.Context, episodeID string, generation int64, kind string, login bool) error {
	if kind != "CAPTCHA_TEXT" && kind != "OTP" || login && kind != "CAPTCHA_TEXT" {
		return errors.New("invalid recovery submission")
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, episodeID))
		if err != nil {
			return err
		}
		if e.Generation != generation {
			return ErrRecoverySuperseded
		}
		if err := checkRecoveryAttemptTx(ctx, tx, e); err != nil {
			return err
		}
		if kind == "OTP" {
			if e.State != "WAITING_OTP" {
				return ErrRecoveryNotReady
			}
			result, err := tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET otp_submissions=otp_submissions+1,updated_at=? WHERE id=? AND otp_submissions<1`, now(), e.ID)
			if err != nil {
				return err
			}
			return requireRecoveryRow(result, ErrRecoveryBudgetExhausted)
		}
		if e.State != "LOGIN" && e.State != "WAITING_CAPTCHA" {
			return ErrRecoveryNotReady
		}
		if login {
			if err := recoveryLoginCooldown(e.LastLoginAt); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET captcha_submissions=captcha_submissions+1,last_login_at=CASE WHEN ? THEN ? ELSE last_login_at END,updated_at=? WHERE id=? AND captcha_submissions<3`, login, now(), now(), e.ID)
		if err != nil {
			return err
		}
		return requireRecoveryRow(result, ErrRecoveryBudgetExhausted)
	})
}

func (s *Store) RecordRecoveryLogin(ctx context.Context, episodeID string, generation int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, episodeID))
		if err != nil {
			return err
		}
		if e.Generation != generation {
			return ErrRecoverySuperseded
		}
		if err := checkRecoveryAttemptTx(ctx, tx, e); err != nil {
			return err
		}
		// Human challenge consumption already reserves its CAPTCHA counter.
		if e.State != "LOGIN" && e.State != "WAITING_CAPTCHA" {
			return ErrRecoveryNotReady
		}
		if err := recoveryLoginCooldown(e.LastLoginAt); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET last_login_at=?,updated_at=? WHERE id=?`, now(), now(), e.ID)
		return err
	})
}

func (s *Store) HasPriorOperationalEvidence(ctx context.Context, connectionID string) (bool, error) {
	var evidence bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE connection_id=? AND verified_at IS NOT NULL) OR EXISTS(SELECT 1 FROM auth_attempts WHERE connection_id=? AND status='VERIFIED') OR EXISTS(SELECT 1 FROM poll_runs WHERE connection_id=? AND status='SUCCEEDED') OR EXISTS(SELECT 1 FROM recovery_runs WHERE connection_id=? AND status='COMPLETED')`, connectionID, connectionID, connectionID, connectionID).Scan(&evidence)
	return evidence, err
}

func (s *Store) HasBlockingAuthRecovery(ctx context.Context, connectionID string, generation int64) (bool, error) {
	var blocked bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_recovery_episodes e WHERE e.connection_id=? AND e.generation=? AND e.finished_at IS NULL AND (e.state='CATCHING_UP' OR e.recovery_run_id IS NOT NULL OR EXISTS(SELECT 1 FROM auth_attempts a WHERE a.id=e.attempt_id AND a.generation=e.generation AND a.status='VERIFIED')))`, connectionID, generation).Scan(&blocked)
	return blocked, err
}
