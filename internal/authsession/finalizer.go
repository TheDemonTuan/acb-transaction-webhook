package authsession

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// Errors are deliberately independent of transport payloads and cryptographic material.
var (
	ErrVerificationPending   = errors.New("ACB session verification is pending")
	ErrConflict              = errors.New("ACB authentication session has been superseded")
	ErrExpired               = errors.New("ACB authentication session has expired")
	ErrAttemptNotFound       = errors.New("ACB authentication session was not found")
	ErrUnavailable           = errors.New("ACB session finalization is unavailable")
	ErrHandoff               = errors.New("ACB browser session handoff failed")
	ErrEncryption            = errors.New("ACB session encryption failed")
	ErrStorage               = errors.New("ACB session storage is unavailable")
	ErrRecoveryIntentMissing = errors.New("ACB session recovery intent was not found")
)

// VerificationError carries only a finite, safe verification result across RPC.
type VerificationError struct{ Code string }

func (e *VerificationError) Error() string {
	if code := VerificationCode(e); code != "" {
		return code
	}
	return "VERIFICATION_UNAVAILABLE"
}

// VerificationCode never interprets error text or exposes an underlying cause.
func VerificationCode(err error) string {
	var verification *VerificationError
	if !errors.As(err, &verification) || verification == nil {
		return ""
	}
	switch verification.Code {
	case "VERIFICATION_ACCOUNT_MISMATCH", "VERIFICATION_ACCOUNT_MISSING",
		"VERIFICATION_FORM_INVALID", "VERIFICATION_AUTH_REQUIRED",
		"VERIFICATION_PAGE_UNSUPPORTED", "VERIFICATION_MAINTENANCE",
		"VERIFICATION_UNAVAILABLE", "VERIFICATION_SUPERSEDED", "VERIFICATION_TIMEOUT":
		return verification.Code
	default:
		return ""
	}
}

type Store interface {
	AuthAttemptStatusForOwner(context.Context, string, string) (storage.AuthAttempt, error)
	Connection(context.Context) (storage.Connection, error)
	CompleteAuthSession(context.Context, string, []byte) (storage.Connection, error)
	GetRecoveryRunByEvent(context.Context, string, int64, string) (storage.RecoveryRun, error)
}

type Browser interface {
	Handoff(context.Context, string) (string, error)
	Complete(context.Context, string) error
}

// Verifier returns the encrypted, generation-bound session after bank verification.
// A failed verification must not return a session.
type Verifier interface {
	VerifySession(context.Context, string, int64, []byte) ([]byte, error)
}

type Scheduler interface {
	ScheduleRecovery(context.Context, string, int64, string) error
}

type Options struct {
	Store     Store
	Browser   Browser
	Keyring   *security.Keyring
	Verifier  Verifier
	Scheduler Scheduler
}

type Finalizer struct{ options Options }

func NewFinalizer(options Options) *Finalizer { return &Finalizer{options: options} }

// Complete verifies and commits a browser handoff. OwnerSubject must be the owner
// used for the attempt lookup; operator authorization remains the caller's responsibility.
// Cleanup and scheduling are best-effort: the transaction persists recovery intent
// before either side effect, and subsequent calls retry both without another handoff.
func (f *Finalizer) Complete(ctx context.Context, attempt storage.AuthAttempt) (storage.Connection, error) {
	if f == nil || f.options.Store == nil {
		return storage.Connection{}, ErrUnavailable
	}
	current, conn, err := f.current(ctx, attempt)
	if err != nil {
		return storage.Connection{}, err
	}
	if current.Status == "VERIFIED" {
		return f.resume(ctx, current, conn)
	}
	if f.options.Keyring == nil || f.options.Browser == nil || f.options.Verifier == nil {
		return storage.Connection{}, ErrUnavailable
	}
	handoff, err := f.options.Browser.Handoff(ctx, current.ID)
	if err != nil || handoff == "" {
		return storage.Connection{}, ErrHandoff
	}
	plaintext := []byte(handoff)
	handoff = ""
	envelope, err := f.options.Keyring.Encrypt(plaintext, security.SessionAAD(current.ConnectionID, current.Generation))
	clear(plaintext)
	if err != nil {
		return storage.Connection{}, ErrEncryption
	}
	encrypted, err := json.Marshal(envelope)
	if err != nil {
		return storage.Connection{}, ErrEncryption
	}
	defer clear(encrypted)
	current, conn, err = f.current(ctx, attempt)
	if err != nil {
		return storage.Connection{}, err
	}
	if current.Status == "VERIFIED" {
		return f.resume(ctx, current, conn)
	}
	verified, verificationErr := f.options.Verifier.VerifySession(ctx, current.ConnectionID, current.Generation, encrypted)
	defer clear(verified)
	if verificationErr != nil {
		latest, latestConn, lookupErr := f.current(ctx, attempt)
		if lookupErr != nil {
			return storage.Connection{}, lookupErr
		}
		if latest.Status == "VERIFIED" {
			return f.resume(ctx, latest, latestConn)
		}
		// Never wrap the verifier's error: RPC errors may contain envelope or bank data.
		if code := VerificationCode(verificationErr); code != "" {
			if code == "VERIFICATION_SUPERSEDED" {
				return storage.Connection{}, ErrConflict
			}
			return storage.Connection{}, &VerificationError{Code: code}
		}
		return storage.Connection{}, ErrVerificationPending
	}
	current, conn, err = f.current(ctx, attempt)
	if err != nil {
		return storage.Connection{}, err
	}
	if current.Status == "VERIFIED" {
		return f.resume(ctx, current, conn)
	}
	if !f.validVerifiedSession(current.ConnectionID, current.Generation, verified) {
		return storage.Connection{}, &VerificationError{Code: "VERIFICATION_UNAVAILABLE"}
	}
	conn, err = f.options.Store.CompleteAuthSession(ctx, current.ID, verified)
	if err != nil {
		if errors.Is(err, storage.ErrGenerationFenceMismatch) || errors.Is(err, storage.ErrRecoverySuperseded) {
			return storage.Connection{}, ErrConflict
		}
		// A concurrent finalizer may have committed between the last read and CAS.
		latest, latestConn, lookupErr := f.current(ctx, attempt)
		if lookupErr == nil && latest.Status == "VERIFIED" {
			return f.resume(ctx, latest, latestConn)
		}
		if lookupErr != nil && !errors.Is(lookupErr, ErrStorage) {
			return storage.Connection{}, lookupErr
		}
		return storage.Connection{}, ErrStorage
	}
	// Recheck after commit so a stale completion never schedules a new generation.
	current, conn, err = f.current(ctx, attempt)
	if err != nil {
		return storage.Connection{}, err
	}
	return f.resume(ctx, current, conn)
}

func (f *Finalizer) validVerifiedSession(connectionID string, generation int64, encoded []byte) bool {
	var envelope security.Envelope
	if len(encoded) == 0 || json.Unmarshal(encoded, &envelope) != nil {
		return false
	}
	// Match the session loader's structural checks before AES-GCM decryption,
	// including nonce length: malformed RPC replies must not cause a panic.
	nonce, nonceErr := base64.RawStdEncoding.DecodeString(envelope.Nonce)
	ciphertext, ciphertextErr := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if envelope.Version != security.EnvelopeVersion || envelope.KeyID == "" || nonceErr != nil || len(nonce) != 12 || ciphertextErr != nil || len(ciphertext) < 16 {
		return false
	}
	plaintext, err := f.options.Keyring.Decrypt(envelope, security.SessionAAD(connectionID, generation))
	if err != nil {
		return false
	}
	defer clear(plaintext)
	_, err = authbrowser.DecodeHandoff(string(plaintext))
	return err == nil
}

func (f *Finalizer) current(ctx context.Context, expected storage.AuthAttempt) (storage.AuthAttempt, storage.Connection, error) {
	a, err := f.options.Store.AuthAttemptStatusForOwner(ctx, expected.ID, expected.OwnerSubject)
	if errors.Is(err, sql.ErrNoRows) {
		return a, storage.Connection{}, ErrAttemptNotFound
	}
	if err != nil {
		return a, storage.Connection{}, ErrStorage
	}
	a.OwnerSubject = expected.OwnerSubject
	conn, err := f.options.Store.Connection(ctx)
	if err != nil {
		return a, conn, ErrStorage
	}
	if a.ID != expected.ID || a.ConnectionID != expected.ConnectionID || a.Generation != expected.Generation || conn.ID != a.ConnectionID || conn.Generation != a.Generation {
		return a, conn, ErrConflict
	}
	if a.Status == "VERIFIED" {
		if conn.State != "MONITORING" {
			return a, conn, ErrConflict
		}
		return a, conn, nil
	}
	if a.Status == "EXPIRED" {
		return a, conn, ErrExpired
	}
	if (a.Status != "STARTING" && a.Status != "IN_PROGRESS" && a.Status != "EXPORTING" && a.Status != "VERIFYING") || conn.State != "AUTH_STARTING" {
		return a, conn, ErrConflict
	}
	expires, err := time.Parse(time.RFC3339Nano, a.ExpiresAt)
	if err != nil || !time.Now().UTC().Before(expires) {
		return a, conn, ErrExpired
	}
	return a, conn, nil
}

func (f *Finalizer) resume(ctx context.Context, attempt storage.AuthAttempt, conn storage.Connection) (storage.Connection, error) {
	run, err := f.options.Store.GetRecoveryRunByEvent(ctx, conn.ID, conn.Generation, attempt.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Connection{}, ErrRecoveryIntentMissing
	}
	if errors.Is(err, storage.ErrGenerationFenceMismatch) {
		return storage.Connection{}, ErrConflict
	}
	if err != nil {
		return storage.Connection{}, ErrStorage
	}
	if run.ConnectionID != conn.ID || run.Generation != conn.Generation || run.EventKey != attempt.ID {
		return storage.Connection{}, ErrConflict
	}
	if f.options.Browser != nil {
		_ = f.options.Browser.Complete(ctx, attempt.ID)
	}
	if f.options.Scheduler != nil {
		_ = f.options.Scheduler.ScheduleRecovery(ctx, run.ConnectionID, run.Generation, run.EventKey)
	}
	return conn, nil
}
