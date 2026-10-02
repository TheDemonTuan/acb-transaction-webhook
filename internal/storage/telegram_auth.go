package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

var ErrRecoveryCommitted = errors.New("verified session recovery cannot be cancelled")

type TelegramAuthState struct {
	BotID, NextUpdateID int64
	Paused              bool
	UpdatedAt           string
}

func (s *Store) TelegramAuthState(ctx context.Context, botID int64) (TelegramAuthState, error) {
	var state TelegramAuthState
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if botID == 0 {
			return errors.New("invalid Telegram bot identity")
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO telegram_auth_state(bot_id,updated_at) VALUES(?,?)`, botID, now())
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT bot_id,next_update_id,paused,updated_at FROM telegram_auth_state WHERE bot_id=?`, botID).Scan(&state.BotID, &state.NextUpdateID, &state.Paused, &state.UpdatedAt)
	})
	return state, err
}
func (s *Store) AdvanceTelegramAuthOffset(ctx context.Context, botID, nextUpdateID int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE telegram_auth_state SET next_update_id=MAX(next_update_id,?),updated_at=? WHERE bot_id=?`, nextUpdateID, now(), botID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

type TelegramAuthAction struct {
	ID                                   string
	BotID, ChatID, UserID, MessageID     int64
	EpisodeID                            string
	ExpectedGeneration                   int64
	Action, Status, ExpiresAt, CreatedAt string
}

const actionColumns = `id,bot_id,chat_id,user_id,COALESCE(message_id,0),COALESCE(episode_id,''),expected_generation,action,status,expires_at,created_at`

func scanAuthAction(row recoveryScanner) (TelegramAuthAction, error) {
	var a TelegramAuthAction
	err := row.Scan(&a.ID, &a.BotID, &a.ChatID, &a.UserID, &a.MessageID, &a.EpisodeID, &a.ExpectedGeneration, &a.Action, &a.Status, &a.ExpiresAt, &a.CreatedAt)
	return a, err
}
func (s *Store) CreateTelegramAuthAction(ctx context.Context, a TelegramAuthAction) (TelegramAuthAction, error) {
	switch a.Action {
	case "LOGIN", "RETRY", "CANCEL", "MANUAL", "PAUSE", "RESUME", "STATUS":
	default:
		return a, ErrChallengeMismatch
	}
	if a.BotID == 0 || a.ChatID == 0 || a.UserID == 0 {
		return a, ErrChallengeMismatch
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return a, err
	}
	a.ID = base64.RawURLEncoding.EncodeToString(b)
	a.Status = "DELIVERING"
	a.CreatedAt = now()
	a.ExpiresAt = time.Now().UTC().Add(60 * time.Second).Format(time.RFC3339Nano)
	a.MessageID = 0
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if a.EpisodeID != "" {
			e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, a.EpisodeID))
			if err != nil {
				return err
			}
			if e.Generation != a.ExpectedGeneration {
				return ErrRecoverySuperseded
			}
		}
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT generation FROM connections ORDER BY created_at LIMIT 1`).Scan(&generation); err != nil {
			return err
		}
		if generation != a.ExpectedGeneration {
			return ErrRecoverySuperseded
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO telegram_auth_actions(id,bot_id,chat_id,user_id,episode_id,expected_generation,action,status,expires_at,created_at) VALUES(?,?,?,?,?,?,?,'DELIVERING',?,?)`, a.ID, a.BotID, a.ChatID, a.UserID, a.EpisodeID, a.ExpectedGeneration, a.Action, a.ExpiresAt, a.CreatedAt)
		return err
	})
	return a, err
}
func (s *Store) DeliverTelegramAuthAction(ctx context.Context, actionID string, messageID int64) error {
	if messageID <= 0 {
		return ErrChallengeMismatch
	}
	result, err := s.db.ExecContext(ctx, `UPDATE telegram_auth_actions SET message_id=?,status='PENDING' WHERE id=? AND status='DELIVERING'`, messageID, actionID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrChallengeMismatch
	}
	return nil
}

// retryRecoveryCatchupTx revives only the automatic episode's own failed intent,
// preserving coverage, cursor and event key. It never reauthenticates.
func retryRecoveryCatchupTx(ctx context.Context, tx *sql.Tx, e AuthRecoveryEpisode) error {
	if e.RecoveryRunID == "" || e.ReasonCode == "INVALID_CHECKPOINT" {
		return ErrRecoveryCommitted
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s JOIN connections c ON c.id=s.connection_id WHERE c.id=? AND c.generation=? AND c.state='MONITORING' AND s.generation=c.generation AND s.verified_at IS NOT NULL)`, e.ConnectionID, e.Generation).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrRecoverySuperseded
	}
	run, err := scanRecoveryRun(tx.QueryRowContext(ctx, `SELECT `+recoveryRunColumns+` FROM recovery_runs WHERE id=? AND connection_id=? AND generation=?`, e.RecoveryRunID, e.ConnectionID, e.Generation))
	if err != nil {
		return err
	}
	if run.Status != RecoveryRunStatusFailed && run.Status != RecoveryRunStatusCanceled {
		return ErrRecoveryRunInvalidState
	}
	if run.ErrorCode == "INVALID_CHECKPOINT" {
		return ErrRecoveryCommitted
	}
	_, err = tx.ExecContext(ctx, `UPDATE recovery_runs SET status='PENDING',error_code=NULL,error_message=NULL,finished_at=NULL,claimed_at=NULL,updated_at=? WHERE id=?`, now(), run.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET state='CATCHING_UP',reason_code='',updated_at=? WHERE id=? AND finished_at IS NULL`, now(), e.ID)
	if err != nil {
		return err
	}
	return enqueueRecoveryNoticeTx(ctx, tx, e, "CATCHING_UP")
}
func (s *Store) RetryAuthRecoveryCatchup(ctx context.Context, episodeID string, generation int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, episodeID))
		if err != nil {
			return err
		}
		if e.Generation != generation || e.FinishedAt != "" {
			return ErrRecoverySuperseded
		}
		return retryRecoveryCatchupTx(ctx, tx, e)
	})
}

func applyTelegramAuthOperationTx(ctx context.Context, tx *sql.Tx, botID int64, episodeID string, generation int64, operation string) (string, error) {
	var current, revision int64
	var state, connectionID string
	if err := tx.QueryRowContext(ctx, `SELECT id,generation,config_revision,state FROM connections ORDER BY created_at LIMIT 1`).Scan(&connectionID, &current, &revision, &state); err != nil {
		return "", err
	}
	if current != generation {
		return "", ErrRecoverySuperseded
	}
	var e AuthRecoveryEpisode
	var err error
	if episodeID != "" {
		e, err = scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, episodeID))
		if err != nil {
			return "", err
		}
		if e.ConnectionID != connectionID || e.Generation != current || e.ConfigRevision != revision {
			return "", ErrRecoverySuperseded
		}
	}
	switch operation {
	case "STATUS":
		return "STATUS", nil
	case "RESUME":
		_, err = tx.ExecContext(ctx, `UPDATE telegram_auth_state SET paused=0,updated_at=? WHERE bot_id=?`, now(), botID)
		return "RESUMED", err
	case "PAUSE", "MANUAL", "CANCEL":
		if operation == "CANCEL" && e.RecoveryRunID != "" && state == "MONITORING" {
			return "", ErrRecoveryCommitted
		}
		if operation == "CANCEL" && e.ID == "" && (state == "AUTH_REQUIRED" || state == "UNCONFIGURED") {
			e, err = ensureRecoveryEpisodeTx(ctx, tx, connectionID, generation)
			if err != nil {
				return "", err
			}
		}
		if operation != "CANCEL" {
			_, err = tx.ExecContext(ctx, `UPDATE telegram_auth_state SET paused=1,updated_at=? WHERE bot_id=?`, now(), botID)
			if err != nil {
				return "", err
			}
		}
		if e.ID != "" && e.FinishedAt == "" && state != "MONITORING" {
			if e.AttemptID != "" {
				var active bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_attempts WHERE id=? AND owner_subject=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING'))`, e.AttemptID, RecoveryOwner).Scan(&active); err != nil {
					return "", err
				}
				if active {
					if err := finishRecoveryAuthAttemptTx(ctx, tx, e, "CANCELLED", "CANCELLED", "OPERATOR_CANCELLED", time.Time{}); err != nil {
						return "", err
					}
				} else {
					if err := invalidateRecoveryInputsTx(ctx, tx, e.ID); err != nil {
						return "", err
					}
					_, err = tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET state='CANCELLED',finished_at=?,updated_at=?,reason_code='OPERATOR_CANCELLED' WHERE id=?`, now(), now(), e.ID)
					if err != nil {
						return "", err
					}
				}
			} else {
				if err := invalidateRecoveryInputsTx(ctx, tx, e.ID); err != nil {
					return "", err
				}
				_, err = tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET state='CANCELLED',finished_at=?,updated_at=?,reason_code='OPERATOR_CANCELLED' WHERE id=?`, now(), now(), e.ID)
				if err != nil {
					return "", err
				}
			}
			if err := enqueueRecoveryNoticeTx(ctx, tx, e, "CANCELLED"); err != nil {
				return "", err
			}
		}
		return operation, nil
	case "LOGIN", "RETRY":
		if state == "MONITORING" {
			if operation == "RETRY" && e.ID != "" && e.FinishedAt == "" {
				return "CATCHUP_RETRY", retryRecoveryCatchupTx(ctx, tx, e)
			}
			return "ACTIVE", nil
		}
		if state != "AUTH_REQUIRED" && state != "UNCONFIGURED" {
			return "", ErrAuthAttemptActive
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_attempts WHERE connection_id=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING'))`, connectionID).Scan(&active); err != nil {
			return "", err
		}
		if active {
			return "", ErrAuthAttemptActive
		}
		if e.ID == "" {
			e, err = ensureRecoveryEpisodeTx(ctx, tx, connectionID, generation)
			if err != nil {
				return "", err
			}
		}
		var challenge bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_challenges WHERE episode_id=? AND status IN ('DELIVERING','PENDING','CONSUMING'))`, e.ID).Scan(&challenge); err != nil {
			return "", err
		}
		if challenge {
			return "", ErrChallengeMismatch
		}
		if err := rearmAuthRecoveryTx(ctx, tx, e); err != nil {
			return "", err
		}
		return "REARMED", nil
	default:
		return "", ErrChallengeMismatch
	}
}
func (s *Store) ApplyTelegramAuthOperation(ctx context.Context, botID int64, episodeID string, generation int64, operation string) (string, error) {
	// Direct commands are non-destructive pause/resume/status only. Bank-impacting
	// operations must consume a persisted confirmation token.
	if operation != "PAUSE" && operation != "RESUME" && operation != "STATUS" {
		return "", ErrChallengeMismatch
	}
	var disposition string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		disposition, err = applyTelegramAuthOperationTx(ctx, tx, botID, episodeID, generation, operation)
		return err
	})
	return disposition, err
}
func (s *Store) ConsumeTelegramAuthAction(ctx context.Context, actionID string, botID, chatID, userID, messageID int64, at time.Time) (TelegramAuthAction, string, error) {
	var a TelegramAuthAction
	var disposition string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		a, err = scanAuthAction(tx.QueryRowContext(ctx, `SELECT `+actionColumns+` FROM telegram_auth_actions WHERE id=?`, actionID))
		if err != nil {
			return err
		}
		if a.Status != "PENDING" {
			return ErrChallengeConsumed
		}
		if a.BotID != botID || a.ChatID != chatID || a.UserID != userID || a.MessageID != messageID {
			return ErrChallengeMismatch
		}
		expiry, err := time.Parse(time.RFC3339Nano, a.ExpiresAt)
		if err != nil || !at.Before(expiry) {
			return ErrChallengeExpired
		}
		disposition, err = applyTelegramAuthOperationTx(ctx, tx, botID, a.EpisodeID, a.ExpectedGeneration, a.Action)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE telegram_auth_actions SET status='CONSUMED' WHERE id=?`, a.ID)
		if err != nil {
			return err
		}
		a.Status = "CONSUMED"
		return nil
	})
	return a, disposition, err
}

type AuthRecoveryNotice struct {
	ID, EpisodeID, EventKey, Kind, Status, NextAttemptAt, CreatedAt, SentAt string
	MessageID                                                               int64
}

func (s *Store) PendingAuthRecoveryNotices(ctx context.Context) ([]AuthRecoveryNotice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,episode_id,event_key,kind,status,COALESCE(message_id,0),COALESCE(next_attempt_at,''),created_at,COALESCE(sent_at,'') FROM auth_recovery_notices WHERE status='PENDING' ORDER BY created_at LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notices []AuthRecoveryNotice
	for rows.Next() {
		var n AuthRecoveryNotice
		if err := rows.Scan(&n.ID, &n.EpisodeID, &n.EventKey, &n.Kind, &n.Status, &n.MessageID, &n.NextAttemptAt, &n.CreatedAt, &n.SentAt); err != nil {
			return nil, err
		}
		notices = append(notices, n)
	}
	return notices, rows.Err()
}
func (s *Store) FinishAuthRecoveryNotice(ctx context.Context, noticeID string, messageID int64, nextAttemptAt time.Time) error {
	if messageID > 0 {
		_, err := s.db.ExecContext(ctx, `UPDATE auth_recovery_notices SET status='SENT',message_id=?,sent_at=? WHERE id=? AND status='PENDING'`, messageID, now(), noticeID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE auth_recovery_notices SET next_attempt_at=? WHERE id=? AND status='PENDING'`, nextAttemptAt.UTC().Format(time.RFC3339Nano), noticeID)
	return err
}
