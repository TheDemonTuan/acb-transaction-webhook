package monitor

import (
	"context"
	"encoding/json"
	"errors"
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
	if l == nil || l.keyring == nil || l.restorer == nil {
		return errors.New("ACB session loader is unavailable")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loadedID == connectionID && l.loadedGeneration == generation {
		return nil
	}
	stored, err := l.store.Session(ctx, connectionID, generation)
	if err != nil {
		return err
	}
	return l.restoreLocked(connectionID, generation, stored.Envelope)
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
		return errors.New("stored ACB session envelope is invalid")
	}
	plaintext, err := l.keyring.Decrypt(envelope, security.SessionAAD(connectionID, generation))
	if err != nil {
		// Existing sessions predate generation-bound AAD; accept once, then refresh
		// under the stronger binding during the next successful persistence cycle.
		plaintext, err = l.keyring.Decrypt(envelope, security.LegacySessionAAD(connectionID))
		if err != nil {
			return errors.New("stored ACB session cannot be decrypted")
		}
	}
	handoff, err := authbrowser.DecodeHandoff(string(plaintext))
	if err != nil {
		return errors.New("stored ACB session handoff is invalid")
	}
	if err := l.restorer.RestoreSession(handoff); err != nil {
		return err
	}
	l.loadedID = connectionID
	l.loadedGeneration = generation
	return nil
}
