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
	ExpectedConfigRevision               int64
	AttemptID, BrowserRevision           string
	Action, Status, ExpiresAt, CreatedAt string
	CredentialGrantToken                 string `json:"-"`
}

const actionColumns = `id,bot_id,chat_id,user_id,COALESCE(message_id,0),COALESCE(episode_id,''),expected_generation,action,status,expires_at,created_at,expected_config_revision,COALESCE(attempt_id,''),COALESCE(browser_revision,'')`

func scanAuthAction(row recoveryScanner) (TelegramAuthAction, error) {
	var a TelegramAuthAction
	err := row.Scan(&a.ID, &a.BotID, &a.ChatID, &a.UserID, &a.MessageID, &a.EpisodeID, &a.ExpectedGeneration, &a.Action, &a.Status, &a.ExpiresAt, &a.CreatedAt, &a.ExpectedConfigRevision, &a.AttemptID, &a.BrowserRevision)
	return a, err
}
func (s *Store) CreateTelegramAuthAction(ctx context.Context, a TelegramAuthAction) (TelegramAuthAction, error) {
	switch a.Action {
	case "LOGIN", "RETRY", "CANCEL", "PAUSE", "RESUME", "LOGOUT", "UPDATE_CREDENTIALS", "CAPTCHA_IMAGE":
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
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		if a.EpisodeID != "" {
			e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, a.EpisodeID))
			if err != nil {
				return err
			}
			if e.Generation != a.ExpectedGeneration {
				return ErrRecoverySuperseded
			}
		}
		var generation, revision int64
		if err := tx.QueryRowContext(ctx, `SELECT generation,config_revision FROM connections ORDER BY created_at LIMIT 1`).Scan(&generation, &revision); err != nil {
			return err
		}
		if generation != a.ExpectedGeneration {
			return ErrRecoverySuperseded
		}
		a.ExpectedConfigRevision = revision
		if a.Action == "CAPTCHA_IMAGE" {
			e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, a.EpisodeID))
			if err != nil {
				return err
			}
			if e.AttemptID != a.AttemptID || a.BrowserRevision == "" {
				return ErrChallengeMismatch
			}
			if err := checkRecoveryAttemptTx(ctx, tx, e); err != nil {
				return err
			}
			var current bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_challenges WHERE episode_id=? AND attempt_id=? AND browser_revision=? AND kind='CAPTCHA_TEXT' AND status IN ('DELIVERING','PENDING'))`, e.ID, a.AttemptID, a.BrowserRevision).Scan(&current); err != nil {
				return err
			}
			if !current {
				return ErrChallengeMismatch
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO telegram_auth_actions(id,bot_id,chat_id,user_id,episode_id,expected_generation,expected_config_revision,attempt_id,browser_revision,action,status,expires_at,created_at) VALUES(?,?,?,?,?,?,?,NULLIF(?,''),NULLIF(?,''),?,'DELIVERING',?,?)`, a.ID, a.BotID, a.ChatID, a.UserID, a.EpisodeID, a.ExpectedGeneration, a.ExpectedConfigRevision, a.AttemptID, a.BrowserRevision, a.Action, a.ExpiresAt, a.CreatedAt)
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
	case "LOGOUT":
		if err := createACBLogoutTx(ctx, tx, botID, connectionID, generation); err != nil {
			return "", err
		}
		return "LOGOUT_QUEUED", nil
	case "RESUME":
		_, err = tx.ExecContext(ctx, `UPDATE telegram_auth_state SET paused=0,updated_at=? WHERE bot_id=?`, now(), botID)
		return "RESUMED", err
	case "PAUSE", "CANCEL":
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
		if err := checkNoACBLogoutTx(ctx, tx, connectionID); err != nil {
			return "", err
		}
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
		if err := recoveryDeadline(e.NextAttemptAt); err != nil {
			return "", err
		}
		var credentials bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM acb_credentials WHERE connection_id=?)`, connectionID).Scan(&credentials); err != nil {
			return "", err
		}
		if !credentials {
			return "", ErrRecoveryNotReady
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
		if operation != "STATUS" {
			if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
				return err
			}
		}
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
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
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
		if err != nil || !at.Before(expiry) || !time.Now().Before(expiry) {
			return ErrChallengeExpired
		}
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT config_revision FROM connections ORDER BY created_at LIMIT 1`).Scan(&revision); err != nil {
			return err
		}
		if revision != a.ExpectedConfigRevision {
			return ErrRecoverySuperseded
		}
		if a.Action == "CAPTCHA_IMAGE" {
			e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, a.EpisodeID))
			if err != nil {
				return err
			}
			if e.Generation != a.ExpectedGeneration || e.AttemptID != a.AttemptID || a.BrowserRevision == "" {
				return ErrChallengeMismatch
			}
			if err := checkRecoveryAttemptTx(ctx, tx, e); err != nil {
				return err
			}
			disposition = "CAPTCHA_IMAGE"
		} else if a.Action == "UPDATE_CREDENTIALS" {
			a.CredentialGrantToken, err = mintACBCredentialGrantTx(ctx, tx, a)
			if err != nil {
				return err
			}
			disposition = "CREDENTIAL_GRANT"
		} else {
			disposition, err = applyTelegramAuthOperationTx(ctx, tx, botID, a.EpisodeID, a.ExpectedGeneration, a.Action)
			if err != nil {
				return err
			}
		}
		if disposition == "REARMED" {
			// LOGIN can originate from a menu created before an episode exists.
			// Bind this exact consumed action to the selected episode before consent.
			if a.EpisodeID == "" {
				if err := tx.QueryRowContext(ctx, `SELECT id FROM auth_recovery_episodes WHERE generation=? AND config_revision=? AND finished_at IS NULL ORDER BY created_at DESC LIMIT 1`, a.ExpectedGeneration, a.ExpectedConfigRevision).Scan(&a.EpisodeID); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE telegram_auth_actions SET episode_id=? WHERE id=?`, a.EpisodeID, a.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET consent_action_id=?,consent_expires_at=?,consent_consumed_at=NULL,status_message_id=NULL WHERE id=?`, a.ID, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), a.EpisodeID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE telegram_auth_actions SET status='CONSUMED' WHERE id=?`, a.ID)
		if err != nil {
			return err
		}
		a.Status = "CONSUMED"
		return nil
	})
	if err != nil {
		a.CredentialGrantToken = ""
	}
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

// ReadTelegramAuthState does not initialize a row: navigation remains read-only
// during deployment maintenance, including the first operator visit.
func (s *Store) ReadTelegramAuthState(ctx context.Context, botID int64) (TelegramAuthState, error) {
	state := TelegramAuthState{BotID: botID}
	err := s.db.QueryRowContext(ctx, `SELECT bot_id,next_update_id,paused,updated_at FROM telegram_auth_state WHERE bot_id=?`, botID).Scan(&state.BotID, &state.NextUpdateID, &state.Paused, &state.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	return state, err
}

func (s *Store) HasACBCredentials(ctx context.Context, connectionID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM acb_credentials WHERE connection_id=?)`, connectionID).Scan(&exists)
	return exists, err
}

// SetAuthRecoveryStatusMessage binds delivery metadata to the current fence.
func (s *Store) SetAuthRecoveryStatusMessage(ctx context.Context, episodeID string, generation, messageID int64) error {
	if messageID <= 0 {
		return ErrChallengeMismatch
	}
	result, err := s.db.ExecContext(ctx, `UPDATE auth_recovery_episodes SET status_message_id=? WHERE id=? AND generation=? AND EXISTS(SELECT 1 FROM connections c WHERE c.id=auth_recovery_episodes.connection_id AND c.generation=?)`, messageID, episodeID, generation, generation)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrRecoverySuperseded
	}
	return err
}

func (s *Store) CoalesceAuthRecoveryNotice(ctx context.Context, noticeID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE auth_recovery_notices SET status='SENT',sent_at=? WHERE id=? AND status='PENDING'`, now(), noticeID)
	return err
}
