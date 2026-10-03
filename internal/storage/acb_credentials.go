package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
)

var (
	ErrCredentialsNotConfigured     = errors.New("CREDENTIALS_NOT_CONFIGURED")
	ErrCredentialsDecryptFailed     = errors.New("CREDENTIALS_DECRYPT_FAILED")
	ErrCredentialsRevisionConflict  = errors.New("CREDENTIALS_REVISION_CONFLICT")
	ErrCredentialsAlreadyConfigured = errors.New("CREDENTIALS_ALREADY_CONFIGURED")
	ErrCredentialAccountMismatch    = errors.New("CREDENTIAL_ACCOUNT_MISMATCH")
	ErrCredentialGrantExpired       = errors.New("CREDENTIAL_GRANT_EXPIRED")
	ErrACBSessionBusy               = errors.New("ACB_SESSION_BUSY")
	ErrInvalidCredentialInput       = errors.New("INVALID_CREDENTIAL_INPUT")
	ErrCredentialsUnavailable       = errors.New("CREDENTIALS_UNAVAILABLE")
)

type ACBCredentials struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	AccountNumber string `json:"accountNumber"`
	Revision      int64  `json:"-"`
}

type ACBCredentialGrantView struct {
	Revision       int64  `json:"revision"`
	UsernameMasked string `json:"usernameMasked"`
	AccountMasked  string `json:"accountMasked"`
	ExpiresAt      string `json:"expiresAt"`
	CanSave        bool   `json:"canSave"`
	BlockedReason  string `json:"blockedReason"`
}

// No database, crypto, or caller-supplied material escapes this boundary.
func credentialError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrCredentialsNotConfigured, ErrCredentialsDecryptFailed, ErrCredentialsRevisionConflict, ErrCredentialsAlreadyConfigured, ErrCredentialAccountMismatch, ErrCredentialGrantExpired, ErrACBSessionBusy, ErrInvalidCredentialInput, ErrCredentialsUnavailable, ErrMutationGateLocked} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrCredentialsUnavailable
}

func normalizeCredentialInput(username, password string) (string, error) {
	if !utf8.ValidString(username) || !utf8.ValidString(password) || strings.ContainsAny(username, "\r\n\x00") || strings.ContainsAny(password, "\r\n\x00") {
		return "", ErrInvalidCredentialInput
	}
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 256 || password == "" || len(password) > 1024 {
		return "", ErrInvalidCredentialInput
	}
	return username, nil
}

func validCredentialAccount(account string) bool {
	if account == "" {
		return false
	}
	for i := range len(account) {
		if account[i] < '0' || account[i] > '9' {
			return false
		}
	}
	return true
}

func (s *Store) decryptACBCredentials(connectionID string, revision int64, encoded []byte, keyID string) (ACBCredentials, error) {
	if s.keyring == nil {
		return ACBCredentials{}, ErrCredentialsDecryptFailed
	}
	var envelope security.Envelope
	if json.Unmarshal(encoded, &envelope) != nil || envelope.KeyID != keyID {
		return ACBCredentials{}, ErrCredentialsDecryptFailed
	}
	plaintext, err := s.keyring.Decrypt(envelope, security.CredentialsAAD(connectionID, revision))
	if err != nil {
		return ACBCredentials{}, ErrCredentialsDecryptFailed
	}
	defer clear(plaintext)
	var credentials ACBCredentials
	if json.Unmarshal(plaintext, &credentials) != nil {
		return ACBCredentials{}, ErrCredentialsDecryptFailed
	}
	username, err := normalizeCredentialInput(credentials.Username, credentials.Password)
	if err != nil || username != credentials.Username || !validCredentialAccount(credentials.AccountNumber) || revision < 1 {
		return ACBCredentials{}, ErrCredentialsDecryptFailed
	}
	credentials.Revision = revision
	return credentials, nil
}

func (s *Store) encryptACBCredentials(connectionID string, credentials ACBCredentials) ([]byte, string, error) {
	if s.keyring == nil {
		return nil, "", ErrCredentialsUnavailable
	}
	plaintext, err := json.Marshal(credentials)
	if err != nil {
		return nil, "", ErrCredentialsUnavailable
	}
	defer clear(plaintext)
	envelope, err := s.keyring.Encrypt(plaintext, security.CredentialsAAD(connectionID, credentials.Revision))
	if err != nil {
		return nil, "", ErrCredentialsUnavailable
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, "", ErrCredentialsUnavailable
	}
	return encoded, envelope.KeyID, nil
}

type credentialQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) readACBCredentials(ctx context.Context, q credentialQuerier, connectionID string) (ACBCredentials, error) {
	var revision int64
	var encoded []byte
	var keyID string
	err := q.QueryRowContext(ctx, `SELECT revision,envelope,key_id FROM acb_credentials WHERE connection_id=?`, connectionID).Scan(&revision, &encoded, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return ACBCredentials{}, ErrCredentialsNotConfigured
	}
	if err != nil {
		return ACBCredentials{}, ErrCredentialsUnavailable
	}
	return s.decryptACBCredentials(connectionID, revision, encoded, keyID)
}

// ReadACBCredentials has no legacy-file or plaintext fallback.
func (s *Store) ReadACBCredentials(ctx context.Context, connectionID string) (ACBCredentials, error) {
	return s.readACBCredentials(ctx, s.db, connectionID)
}

// ImportACBCredentials is insert-only. Creating the initial connection and its
// encrypted credentials shares the mutation fence and rollback boundary.
func (s *Store) ImportACBCredentials(ctx context.Context, credentials ACBCredentials) (Connection, error) {
	username, err := normalizeCredentialInput(credentials.Username, credentials.Password)
	if err != nil || !validCredentialAccount(credentials.AccountNumber) {
		return Connection{}, ErrInvalidCredentialInput
	}
	credentials.Username, credentials.Revision = username, 1
	var c Connection
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT id,state,COALESCE(account_masked,''),generation,COALESCE(started_at,''),updated_at FROM connections ORDER BY created_at LIMIT 1`).Scan(&c.ID, &c.State, &c.AccountMasked, &c.Generation, &c.StartedAt, &c.UpdatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			c = Connection{ID: id("conn"), State: "AUTH_REQUIRED", AccountMasked: maskCredentialAccount(credentials.AccountNumber), StartedAt: now(), UpdatedAt: now()}
			if _, err = tx.ExecContext(ctx, `INSERT INTO connections(id,account_masked,state,started_at,created_at,updated_at) VALUES(?,?,?,?,?,?)`, c.ID, c.AccountMasked, c.State, c.StartedAt, c.UpdatedAt, c.UpdatedAt); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM acb_credentials WHERE connection_id=?)`, c.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrCredentialsAlreadyConfigured
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_attempts WHERE connection_id=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING')) OR EXISTS(SELECT 1 FROM acb_logout_jobs WHERE connection_id=? AND finished_at IS NULL)`, c.ID, c.ID).Scan(&active); err != nil {
			return err
		}
		if active || c.State == "AUTH_STARTING" {
			return ErrACBSessionBusy
		}
		if err := s.checkImportAccountTx(ctx, tx, c.ID, credentials.AccountNumber); err != nil {
			return err
		}
		encoded, keyID, err := s.encryptACBCredentials(c.ID, credentials)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES(?,1,?,?,?)`, c.ID, encoded, keyID, now())
		return err
	})
	if err != nil {
		return Connection{}, credentialError(err)
	}
	return c, nil
}

func (s *Store) checkImportAccountTx(ctx context.Context, tx *sql.Tx, connectionID, account string) error {
	var encoded []byte
	var generation int64
	var keyID string
	err := tx.QueryRowContext(ctx, `SELECT generation,envelope,key_id FROM sessions WHERE connection_id=?`, connectionID).Scan(&generation, &encoded, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.keyring == nil {
		return ErrCredentialsDecryptFailed
	}
	var envelope security.Envelope
	if json.Unmarshal(encoded, &envelope) != nil || envelope.KeyID != keyID {
		return ErrCredentialsDecryptFailed
	}
	plaintext, err := s.keyring.Decrypt(envelope, security.SessionAAD(connectionID, generation))
	if err != nil {
		plaintext, err = s.keyring.Decrypt(envelope, security.LegacySessionAAD(connectionID))
	}
	if err != nil {
		return ErrCredentialsDecryptFailed
	}
	defer clear(plaintext)
	handoff, err := authbrowser.DecodeHandoff(string(plaintext))
	if err != nil {
		return ErrCredentialsDecryptFailed
	}
	// Cookie-only legacy sessions contain no account-selection evidence. Import
	// the operator's initial account without claiming it has been verified;
	// a later consent-gated login must still select that exact account.
	if handoff.Version == 0 && handoff.Fields["AccountNbr"] == "" {
		return nil
	}
	if handoff.Fields["AccountNbr"] != account {
		return ErrCredentialAccountMismatch
	}
	return nil
}

func maskCredentialAccount(account string) string {
	if len(account) > 4 {
		return "****" + account[len(account)-4:]
	}
	return "****"
}

func credentialTokenHash(token string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != token {
		return "", ErrCredentialGrantExpired
	}
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:]), nil
}

// mintACBCredentialGrantTx is deliberately private: only a consumed Telegram
// action may create a grant, and its raw token is never persisted.
func mintACBCredentialGrantTx(ctx context.Context, tx *sql.Tx, a TelegramAuthAction) (string, error) {
	var connectionID string
	var generation, revision, credentialRevision int64
	err := tx.QueryRowContext(ctx, `SELECT c.id,c.generation,c.config_revision,COALESCE(k.revision,0) FROM connections c LEFT JOIN acb_credentials k ON k.connection_id=c.id ORDER BY c.created_at LIMIT 1`).Scan(&connectionID, &generation, &revision, &credentialRevision)
	if err != nil {
		return "", credentialError(err)
	}
	if generation != a.ExpectedGeneration || revision != a.ExpectedConfigRevision {
		return "", ErrCredentialsRevisionConflict
	}
	if credentialRevision == 0 {
		return "", ErrCredentialsNotConfigured
	}
	if a.EpisodeID != "" {
		var valid bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM auth_recovery_episodes WHERE id=? AND connection_id=? AND generation=? AND config_revision=?)`, a.EpisodeID, connectionID, generation, revision).Scan(&valid); err != nil {
			return "", credentialError(err)
		}
		if !valid {
			return "", ErrCredentialsRevisionConflict
		}
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", ErrCredentialsUnavailable
	}
	token := base64.RawURLEncoding.EncodeToString(b[:])
	tokenHash, _ := credentialTokenHash(token)
	if _, err := tx.ExecContext(ctx, `UPDATE acb_credential_grants SET status='REVOKED' WHERE connection_id=? AND status='PENDING'`, connectionID); err != nil {
		return "", credentialError(err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO acb_credential_grants(token_hash,connection_id,bot_id,chat_id,user_id,expected_generation,expected_config_revision,credential_revision,status,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,'PENDING',?,?)`, tokenHash, connectionID, a.BotID, a.ChatID, a.UserID, generation, revision, credentialRevision, time.Now().UTC().Add(5*time.Minute).Format(time.RFC3339Nano), now())
	if err != nil {
		return "", credentialError(err)
	}
	return token, nil
}

type acbCredentialGrant struct {
	hash, connectionID, owner, expiresAt           string
	generation, configRevision, credentialRevision int64
}

func readACBCredentialGrantTx(ctx context.Context, tx *sql.Tx, token, owner string) (acbCredentialGrant, error) {
	var g acbCredentialGrant
	if owner == "" {
		return g, ErrCredentialGrantExpired
	}
	hash, err := credentialTokenHash(token)
	if err != nil {
		return g, err
	}
	g.hash = hash
	var status string
	err = tx.QueryRowContext(ctx, `SELECT connection_id,expected_generation,expected_config_revision,credential_revision,COALESCE(owner_subject,''),status,expires_at FROM acb_credential_grants WHERE token_hash=?`, hash).Scan(&g.connectionID, &g.generation, &g.configRevision, &g.credentialRevision, &g.owner, &status, &g.expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrCredentialGrantExpired
	}
	if err != nil {
		return g, err
	}
	expiry, err := time.Parse(time.RFC3339Nano, g.expiresAt)
	if err != nil || !time.Now().Before(expiry) || status != "PENDING" || (g.owner != "" && g.owner != owner) {
		return g, ErrCredentialGrantExpired
	}
	var generation, configRevision, credentialRevision int64
	err = tx.QueryRowContext(ctx, `SELECT c.generation,c.config_revision,COALESCE(k.revision,0) FROM connections c LEFT JOIN acb_credentials k ON k.connection_id=c.id WHERE c.id=?`, g.connectionID).Scan(&generation, &configRevision, &credentialRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrCredentialsRevisionConflict
	}
	if err != nil {
		return g, err
	}
	if generation != g.generation || configRevision != g.configRevision || credentialRevision != g.credentialRevision {
		return g, ErrCredentialsRevisionConflict
	}
	return g, nil
}

func acbCredentialBusyTx(ctx context.Context, tx *sql.Tx, connectionID string) (bool, error) {
	var busy bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM connections WHERE id=? AND state IN ('MONITORING','AUTH_STARTING')) OR EXISTS(SELECT 1 FROM auth_attempts WHERE connection_id=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING')) OR EXISTS(SELECT 1 FROM acb_logout_jobs WHERE connection_id=? AND finished_at IS NULL) OR EXISTS(SELECT 1 FROM recovery_runs WHERE connection_id=? AND status IN ('PENDING','RUNNING')) OR EXISTS(SELECT 1 FROM history_sync_jobs WHERE connection_id=? AND status='RUNNING')`, connectionID, connectionID, connectionID, connectionID, connectionID).Scan(&busy)
	return busy, err
}

func (s *Store) ValidateACBCredentialGrant(ctx context.Context, token, owner string) (ACBCredentialGrantView, error) {
	var view ACBCredentialGrantView
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		g, err := readACBCredentialGrantTx(ctx, tx, token, owner)
		if err != nil {
			return err
		}
		credentials, err := s.readACBCredentials(ctx, tx, g.connectionID)
		if err != nil {
			return err
		}
		busy, err := acbCredentialBusyTx(ctx, tx, g.connectionID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE acb_credential_grants SET owner_subject=? WHERE token_hash=? AND owner_subject IS NULL AND status='PENDING'`, owner, g.hash); err != nil {
			return err
		}
		view = ACBCredentialGrantView{Revision: credentials.Revision, UsernameMasked: "****", AccountMasked: maskCredentialAccount(credentials.AccountNumber), ExpiresAt: g.expiresAt, CanSave: !busy}
		if busy {
			view.BlockedReason = ErrACBSessionBusy.Error()
		}
		return nil
	})
	if err != nil {
		return ACBCredentialGrantView{}, credentialError(err)
	}
	return view, nil
}

func (s *Store) SaveACBCredentials(ctx context.Context, token, owner string, expectedRevision int64, username, password string) (int64, error) {
	username, err := normalizeCredentialInput(username, password)
	if err != nil {
		return 0, err
	}
	var savedRevision int64
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkMutationAllowedTx(ctx, tx); err != nil {
			return err
		}
		g, err := readACBCredentialGrantTx(ctx, tx, token, owner)
		if err != nil {
			return err
		}
		// Save cannot be the first owner-binding request.
		if g.owner != owner {
			return ErrCredentialGrantExpired
		}
		if expectedRevision != g.credentialRevision {
			return ErrCredentialsRevisionConflict
		}
		busy, err := acbCredentialBusyTx(ctx, tx, g.connectionID)
		if err != nil {
			return err
		}
		if busy {
			return ErrACBSessionBusy
		}
		credentials, err := s.readACBCredentials(ctx, tx, g.connectionID)
		if err != nil {
			return err
		}
		credentials.Username, credentials.Password, credentials.Revision = username, password, credentials.Revision+1
		encoded, keyID, err := s.encryptACBCredentials(g.connectionID, credentials)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE acb_credentials SET revision=?,envelope=?,key_id=?,updated_at=? WHERE connection_id=? AND revision=?`, credentials.Revision, encoded, keyID, now(), g.connectionID, expectedRevision)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrCredentialsRevisionConflict
		}
		result, err = tx.ExecContext(ctx, `UPDATE connections SET config_revision=config_revision+1,generation=generation+1,state='AUTH_REQUIRED',updated_at=? WHERE id=? AND generation=? AND config_revision=?`, now(), g.connectionID, g.generation, g.configRevision)
		if err != nil {
			return err
		}
		n, err = result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrCredentialsRevisionConflict
		}
		if err := supersedeACBInputsTx(ctx, tx, g.connectionID, "CREDENTIALS_UPDATED"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE acb_credential_grants SET status='REVOKED' WHERE connection_id=? AND status='PENDING' AND token_hash<>?`, g.connectionID, g.hash); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE acb_credential_grants SET status='CONSUMED',consumed_at=? WHERE token_hash=? AND status='PENDING'`, now(), g.hash); err != nil {
			return err
		}
		e := AuthRecoveryEpisode{ID: id("are"), ConnectionID: g.connectionID, TriggerGeneration: g.generation + 1, Generation: g.generation + 1, ConfigRevision: g.configRevision + 1, CredentialRevision: credentials.Revision, State: "DETECTED"}
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_recovery_episodes(id,connection_id,trigger_generation,generation,config_revision,credential_revision,state,reason_code,created_at,updated_at) VALUES(?,?,?,?,?,?,'DETECTED','CREDENTIALS_UPDATED',?,?)`, e.ID, e.ConnectionID, e.TriggerGeneration, e.Generation, e.ConfigRevision, e.CredentialRevision, now(), now()); err != nil {
			return err
		}
		if err := enqueueRecoveryNoticeTx(ctx, tx, e, "CREDENTIALS_UPDATED"); err != nil {
			return err
		}
		details, _ := json.Marshal(struct {
			Revision int64  `json:"revision"`
			Result   string `json:"result"`
		}{credentials.Revision, "SAVED"})
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs(id,actor_subject,actor_role,action,target,request_id,details_json,created_at) VALUES(?,?,'OWNER','ACB_CREDENTIALS_UPDATED',?,'',?,?)`, id("audit"), owner, g.connectionID, string(details), now()); err != nil {
			return err
		}
		savedRevision = credentials.Revision
		return nil
	})
	if err != nil {
		return 0, credentialError(err)
	}
	return savedRevision, nil
}

func supersedeACBInputsTx(ctx context.Context, tx *sql.Tx, connectionID, reason string) error {
	t := now()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE connection_id=?`, connectionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_attempts SET status='CANCELLED',finished_at=? WHERE connection_id=? AND status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING')`, t, connectionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_challenges SET status='INVALIDATED' WHERE connection_id=? AND status IN ('DELIVERING','PENDING','CONSUMING')`, connectionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE telegram_auth_actions SET status='INVALIDATED' WHERE status IN ('DELIVERING','PENDING') AND (episode_id IN (SELECT id FROM auth_recovery_episodes WHERE connection_id=?) OR episode_id IS NULL OR episode_id='')`, connectionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_recovery_episodes SET state='SUPERSEDED',reason_code=?,finished_at=?,updated_at=?,consent_action_id=NULL,consent_expires_at=NULL,consent_consumed_at=NULL WHERE connection_id=? AND finished_at IS NULL`, reason, t, t, connectionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recovery_runs SET status='CANCELED',error_code=?,finished_at=?,updated_at=? WHERE connection_id=? AND status IN ('PENDING','RUNNING')`, reason, t, t, connectionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE history_sync_jobs SET status='CANCELED',error_code=?,finished_at=?,updated_at=? WHERE connection_id=? AND status IN ('QUEUED','RUNNING')`, reason, t, t, connectionID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE auth_recovery_notices SET status='SENT',sent_at=? WHERE episode_id IN (SELECT id FROM auth_recovery_episodes WHERE connection_id=?) AND status='PENDING'`, t, connectionID)
	return err
}
