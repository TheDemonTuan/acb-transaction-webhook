package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type AuthChallenge struct {
	ID, EpisodeID, ConnectionID              string
	Generation                               int64
	AttemptID, BrowserRevision, Kind, Status string
	ChatID, PromptMessageID                  int64
	ExpiresAt, CreatedAt, ConsumedAt         string
	PromptDeletedAt                          string
}

const challengeColumns = `id,episode_id,connection_id,generation,attempt_id,browser_revision,kind,status,chat_id,COALESCE(prompt_message_id,0),expires_at,created_at,COALESCE(consumed_at,''),COALESCE(prompt_deleted_at,'')`

func scanChallenge(row recoveryScanner) (AuthChallenge, error) {
	var c AuthChallenge
	err := row.Scan(&c.ID, &c.EpisodeID, &c.ConnectionID, &c.Generation, &c.AttemptID, &c.BrowserRevision, &c.Kind, &c.Status, &c.ChatID, &c.PromptMessageID, &c.ExpiresAt, &c.CreatedAt, &c.ConsumedAt, &c.PromptDeletedAt)
	return c, err
}
func challengeFenceTx(ctx context.Context, tx *sql.Tx, c AuthChallenge, at time.Time) error {
	var generation, revision int64
	var state string
	err := tx.QueryRowContext(ctx, `SELECT generation,config_revision,state FROM connections WHERE id=?`, c.ConnectionID).Scan(&generation, &revision, &state)
	if err != nil {
		return err
	}
	if generation != c.Generation || state != "AUTH_STARTING" {
		return ErrChallengeMismatch
	}
	var count int
	var expires string
	err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(MAX(a.expires_at),'') FROM auth_recovery_episodes e JOIN auth_attempts a ON a.id=e.attempt_id WHERE e.id=? AND e.connection_id=? AND e.generation=? AND e.config_revision=? AND e.attempt_id=? AND e.finished_at IS NULL AND e.state IN ('STARTING','LOGIN','WAITING_CAPTCHA','WAITING_OTP') AND a.generation=e.generation AND a.connection_id=e.connection_id AND a.owner_subject=? AND a.status IN ('STARTING','IN_PROGRESS')`, c.EpisodeID, c.ConnectionID, c.Generation, revision, c.AttemptID, RecoveryOwner).Scan(&count, &expires)
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrChallengeMismatch
	}
	ttl, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !at.Before(ttl) {
		return ErrChallengeExpired
	}
	challengeExpiry, err := time.Parse(time.RFC3339Nano, c.ExpiresAt)
	if err != nil || challengeExpiry.After(ttl) {
		return ErrChallengeMismatch
	}
	var paused bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_auth_state WHERE paused=1)`).Scan(&paused); err != nil {
		return err
	}
	if paused {
		return ErrChallengeMismatch
	}
	e, err := scanEpisode(tx.QueryRowContext(ctx, `SELECT `+episodeColumns+` FROM auth_recovery_episodes WHERE id=?`, c.EpisodeID))
	if err != nil {
		return err
	}
	if err := checkRecoveryConsentTx(ctx, tx, e, false); err != nil {
		return err
	}
	var chatID int64
	if err := tx.QueryRowContext(ctx, `SELECT chat_id FROM telegram_auth_actions WHERE id=?`, e.ConsentActionID).Scan(&chatID); err != nil {
		return err
	}
	if chatID != c.ChatID {
		return ErrChallengeMismatch
	}
	return nil
}
func (s *Store) CreateAuthChallenge(ctx context.Context, c AuthChallenge) (AuthChallenge, error) {
	if c.Kind != "CAPTCHA_TEXT" && c.Kind != "OTP" {
		return c, ErrChallengeMismatch
	}
	at := time.Now().UTC()
	expiry, err := time.Parse(time.RFC3339Nano, c.ExpiresAt)
	maxTTL := 180 * time.Second
	if c.Kind == "OTP" {
		maxTTL = 120 * time.Second
	}
	if err != nil || !at.Before(expiry) || expiry.After(at.Add(maxTTL)) || c.ChatID == 0 || c.BrowserRevision == "" {
		return c, ErrChallengeMismatch
	}
	c.ID = id("ach")
	c.Status = "DELIVERING"
	c.CreatedAt = at.Format(time.RFC3339Nano)
	c.PromptMessageID = 0
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		if err := challengeFenceTx(ctx, tx, c, at); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO auth_challenges(id,episode_id,connection_id,generation,attempt_id,browser_revision,kind,status,chat_id,expires_at,created_at) VALUES(?,?,?,?,?,?,?,'DELIVERING',?,?,?)`, c.ID, c.EpisodeID, c.ConnectionID, c.Generation, c.AttemptID, c.BrowserRevision, c.Kind, c.ChatID, c.ExpiresAt, c.CreatedAt)
		return err
	})
	return c, err
}
func (s *Store) DeliverAuthChallenge(ctx context.Context, challengeID string, chatID, messageID int64) error {
	if messageID <= 0 {
		return ErrChallengeMismatch
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		c, err := scanChallenge(tx.QueryRowContext(ctx, `SELECT `+challengeColumns+` FROM auth_challenges WHERE id=?`, challengeID))
		if err != nil {
			return err
		}
		if c.ChatID != chatID || c.Status != "DELIVERING" {
			return ErrChallengeMismatch
		}
		at := time.Now()
		expiry, err := time.Parse(time.RFC3339Nano, c.ExpiresAt)
		if err != nil || !at.Before(expiry) {
			return ErrChallengeExpired
		}
		if err := challengeFenceTx(ctx, tx, c, at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE auth_challenges SET status='PENDING',prompt_message_id=? WHERE id=? AND status='DELIVERING'`, messageID, c.ID)
		return err
	})
}
func (s *Store) AuthChallengeForPrompt(ctx context.Context, chatID, messageID int64) (AuthChallenge, error) {
	return scanChallenge(s.db.QueryRowContext(ctx, `SELECT `+challengeColumns+` FROM auth_challenges WHERE chat_id=? AND prompt_message_id=?`, chatID, messageID))
}
func (s *Store) ActiveAuthChallenge(ctx context.Context, attemptID string) (AuthChallenge, error) {
	return scanChallenge(s.db.QueryRowContext(ctx, `SELECT `+challengeColumns+` FROM auth_challenges WHERE attempt_id=? AND status IN ('DELIVERING','PENDING','CONSUMING')`, attemptID))
}

// AuthChallengesForDelivery also returns terminal prompts so the sole delivery
// loop can remove messages invalidated by cancellation or generation changes.
func (s *Store) AuthChallengesForDelivery(ctx context.Context) ([]AuthChallenge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+challengeColumns+` FROM auth_challenges WHERE status IN ('DELIVERING','PENDING') OR (prompt_message_id IS NOT NULL AND prompt_deleted_at IS NULL AND status NOT IN ('DELIVERING','PENDING','CONSUMING')) ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var challenges []AuthChallenge
	for rows.Next() {
		c, err := scanChallenge(rows)
		if err != nil {
			return nil, err
		}
		challenges = append(challenges, c)
	}
	return challenges, rows.Err()
}

// MarkAuthChallengePromptDeleted retains the prompt ID for replay correlation.
// Cleanup acknowledgement is safe even while the bank result is CONSUMING.
func (s *Store) MarkAuthChallengePromptDeleted(ctx context.Context, c AuthChallenge) error {
	result, err := s.db.ExecContext(ctx, `UPDATE auth_challenges SET prompt_deleted_at=COALESCE(prompt_deleted_at,?) WHERE id=? AND chat_id=? AND prompt_message_id=? AND status NOT IN ('DELIVERING','PENDING')`, now(), c.ID, c.ChatID, c.PromptMessageID)
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

// ValidateAuthChallenge checks the durable attempt/consent fence immediately
// before external delivery. DeliverAuthChallenge repeats it before binding.
func (s *Store) ValidateAuthChallenge(ctx context.Context, c AuthChallenge, at time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		expiry, err := time.Parse(time.RFC3339Nano, c.ExpiresAt)
		if err != nil || !at.Before(expiry) {
			return ErrChallengeExpired
		}
		return challengeFenceTx(ctx, tx, c, at)
	})
}

// RecoverAuthChallengeDelivery is startup-only: a send without a committed
// message binding has an unknowable outcome and must never be replayed.
func (s *Store) RecoverAuthChallengeDelivery(ctx context.Context) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE auth_challenges SET status='INVALIDATED' WHERE status='DELIVERING' AND COALESCE(prompt_message_id,0)=0`)
		return err
	})
}
func (s *Store) ConsumeAuthChallenge(ctx context.Context, challengeID string, generation int64, revision string, chatID, messageID int64, at time.Time) (AuthChallenge, error) {
	var c AuthChallenge
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		var err error
		c, err = scanChallenge(tx.QueryRowContext(ctx, `SELECT `+challengeColumns+` FROM auth_challenges WHERE id=?`, challengeID))
		if err != nil {
			return err
		}
		if c.Status != "PENDING" {
			return ErrChallengeConsumed
		}
		if c.Generation != generation || c.BrowserRevision != revision || c.ChatID != chatID || c.PromptMessageID != messageID {
			return ErrChallengeMismatch
		}
		expiry, err := time.Parse(time.RFC3339Nano, c.ExpiresAt)
		if err != nil || !at.Before(expiry) {
			return ErrChallengeExpired
		}
		if err := challengeFenceTx(ctx, tx, c, at); err != nil {
			return err
		}
		column, cap := "captcha_submissions", 3
		if c.Kind == "OTP" {
			column, cap = "otp_submissions", 1
		}
		result, err := tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET `+column+`=`+column+`+1,updated_at=? WHERE id=? AND `+column+`<?`, at.UTC().Format(time.RFC3339Nano), c.EpisodeID, cap)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrRecoveryBudgetExhausted
		}
		result, err = tx.ExecContext(ctx, `UPDATE auth_challenges SET status='CONSUMING',consumed_at=? WHERE id=? AND status='PENDING'`, at.UTC().Format(time.RFC3339Nano), c.ID)
		if err != nil {
			return err
		}
		n, err = result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrChallengeConsumed
		}
		c.Status = "CONSUMING"
		c.ConsumedAt = at.UTC().Format(time.RFC3339Nano)
		return nil
	})
	return c, err
}
func (s *Store) FinishAuthChallenge(ctx context.Context, challengeID, status string) error {
	switch status {
	case "CONSUMED", "EXPIRED", "CANCELLED", "INVALIDATED":
	default:
		return errors.New("invalid challenge terminal state")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE auth_challenges SET status=? WHERE id=? AND status IN ('DELIVERING','PENDING','CONSUMING')`, status, challengeID)
	return err
}
