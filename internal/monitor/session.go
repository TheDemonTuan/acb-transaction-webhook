package monitor

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type SessionRestorer interface {
	RestoreSession(authbrowser.Handoff) error
}

type SessionSnapshotter interface {
	SnapshotSession() (authbrowser.Handoff, error)
}

var (
	ErrSessionInvalid       = errors.New("stored ACB session is invalid")
	ErrSessionDecryptFailed = errors.New("stored ACB session cannot be decrypted")
)

type SessionLoader struct {
	store            *storage.Store
	keyring          *security.Keyring
	restorer         SessionRestorer
	mu               sync.Mutex
	loadedID         string
	loadedGeneration int64
}

func NewSessionLoader(store *storage.Store, keyring *security.Keyring, restorer SessionRestorer) *SessionLoader {
	return &SessionLoader{store: store, keyring: keyring, restorer: restorer}
}

// InvalidateCache clears in-memory generation caching so the next restore re-reads from storage.
func (l *SessionLoader) InvalidateCache() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadedID = ""
	l.loadedGeneration = 0
}

// Restore loads only the encrypted session for the current connection
// generation. It rejects stale, malformed or cross-connection browser state.
func (l *SessionLoader) Restore(ctx context.Context, connectionID string, generation int64) error {
	if l == nil || l.store == nil || l.keyring == nil || l.restorer == nil {
		return errors.New("ACB session loader is unavailable")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loadedID == connectionID && l.loadedGeneration == generation {
		return nil
	}
	stored, err := l.store.Session(ctx, connectionID, generation)
	missing := errors.Is(err, sql.ErrNoRows)
	if err == nil {
		err = l.restoreLocked(connectionID, generation, stored.Envelope)
	}
	reason := ""
	switch {
	case missing:
		reason = "SESSION_MISSING"
	case errors.Is(err, ErrSessionInvalid):
		reason = "SESSION_INVALID"
	case errors.Is(err, ErrSessionDecryptFailed):
		reason = "SESSION_DECRYPT_FAILED"
	}
	if reason != "" {
		if recoveryErr := l.store.RequireSessionRecovery(ctx, connectionID, generation, reason); recoveryErr != nil {
			return errors.Join(err, recoveryErr)
		}
	}
	return err
}

func (l *SessionLoader) Persist(ctx context.Context, connectionID string, generation int64) error {
	if l == nil || l.keyring == nil {
		return errors.New("ACB session loader is unavailable")
	}
	snapshotter, ok := l.restorer.(SessionSnapshotter)
	if !ok {
		return nil
	}
	handoff, err := snapshotter.SnapshotSession()
	if err != nil {
		if errors.Is(err, acb.ErrAuthenticatedFormStateUnavailable) && l.store != nil {
			conn, cErr := l.store.Connection(ctx)
			if cErr == nil && (conn.State == "AUTH_REQUIRED" || conn.State == "UNCONFIGURED" || conn.State == "DISCONNECTED") {
				return nil
			}
			stored, storeErr := l.store.Session(ctx, connectionID, generation)
			if storeErr == nil && stored.ConnectionID == connectionID && stored.Generation == generation && len(stored.Envelope) > 0 {
				return nil
			}
		}
		return err
	}
	plaintext, err := authbrowser.EncodeHandoff(handoff, []byte("refresh"))
	if err != nil {
		return err
	}
	envelope, err := l.keyring.Encrypt([]byte(plaintext), security.SessionAAD(connectionID, generation))
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return l.store.RefreshSession(ctx, connectionID, generation, encoded, envelope.KeyID)
}

func (l *SessionLoader) RestoreEnvelope(connectionID string, generation int64, encoded []byte) error {
	if l == nil || l.keyring == nil || l.restorer == nil {
		return errors.New("ACB session loader is unavailable")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.restoreLocked(connectionID, generation, encoded)
}

func (l *SessionLoader) restoreLocked(connectionID string, generation int64, encoded []byte) error {
	var envelope security.Envelope
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return ErrSessionInvalid
	}
	nonce, nonceErr := base64.RawStdEncoding.DecodeString(envelope.Nonce)
	ciphertext, ciphertextErr := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
	if envelope.Version != security.EnvelopeVersion || envelope.KeyID == "" || nonceErr != nil || len(nonce) != 12 || ciphertextErr != nil || len(ciphertext) < 16 {
		return ErrSessionInvalid
	}
	plaintext, err := l.keyring.Decrypt(envelope, security.SessionAAD(connectionID, generation))
	if err != nil {
		// Existing sessions predate generation-bound AAD; accept once, then refresh
		// under the stronger binding during the next successful persistence cycle.
		plaintext, err = l.keyring.Decrypt(envelope, security.LegacySessionAAD(connectionID))
		if err != nil {
			return ErrSessionDecryptFailed
		}
	}
	defer clear(plaintext)
	handoff, err := authbrowser.DecodeHandoff(string(plaintext))
	if err != nil {
		return ErrSessionInvalid
	}
	for _, cookie := range handoff.Cookies {
		domain := strings.ToLower(strings.TrimPrefix(cookie.Domain, "."))
		if cookie.Name == "" || cookie.Value == "" || (domain != "" && domain != acb.OfficialHost && domain != "acb.com.vn") {
			return ErrSessionInvalid
		}
	}
	for _, endpoint := range []string{handoff.URL, handoff.Action} {
		if endpoint == "" {
			continue
		}
		parsed, parseErr := url.Parse(endpoint)
		if parseErr != nil || parsed.User != nil || (parsed.IsAbs() && (parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), acb.OfficialHost))) || (!parsed.IsAbs() && parsed.Host != "") {
			return ErrSessionInvalid
		}
	}
	if handoff.Action != "" && (handoff.Fields["dse_sessionId"] == "" || handoff.Fields["dse_processorState"] == "") {
		return ErrSessionInvalid
	}
	if err := l.restorer.RestoreSession(handoff); err != nil {
		return err
	}
	l.loadedID = connectionID
	l.loadedGeneration = generation
	return nil
}
