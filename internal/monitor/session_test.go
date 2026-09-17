package monitor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type recordingRestorer struct{ handoff authbrowser.Handoff }

func (r *recordingRestorer) RestoreSession(handoff authbrowser.Handoff) error {
	r.handoff = handoff
	return nil
}

func TestSessionLoaderDecryptsAndRestoresCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	key := "0123456789012345678901234567890123456789012345678901234567890123"
	if err := os.WriteFile(keyFile, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	keyring, err := security.LoadKeyring(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	connection, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAuthAttempt(ctx, "owner@example.com", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := authbrowser.EncodeHandoff(authbrowser.Handoff{Version: 1, URL: "https://online.acb.com.vn/acbib/AccountSummary", Cookies: []authbrowser.Cookie{{Name: "session", Value: "secret", Domain: ".online.acb.com.vn", Path: "/", Secure: true}}}, []byte("handoff"))
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := keyring.Encrypt([]byte(plaintext), []byte("acb-session:"+connection.ID))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(envelope)
	if _, err := store.CompleteAuthSession(ctx, attempt.ID, encoded); err != nil {
		t.Fatal(err)
	}
	restorer := &recordingRestorer{}
	loader := NewSessionLoader(store, keyring, restorer)
	if err := loader.Restore(ctx, connection.ID, attempt.Generation); err != nil {
		t.Fatal(err)
	}
	if restorer.handoff.URL != "https://online.acb.com.vn/acbib/AccountSummary" || len(restorer.handoff.Cookies) != 1 || restorer.handoff.Cookies[0].Value != "secret" {
		t.Fatalf("session=%+v", restorer.handoff)
	}
}

type mockSessionSnapshotter struct {
	handoff     authbrowser.Handoff
	snapshotErr error
}

func (m *mockSessionSnapshotter) RestoreSession(handoff authbrowser.Handoff) error {
	m.handoff = handoff
	return nil
}

func (m *mockSessionSnapshotter) SnapshotSession() (authbrowser.Handoff, error) {
	if m.snapshotErr != nil {
		return authbrowser.Handoff{}, m.snapshotErr
	}
	return m.handoff, nil
}

func TestSessionLoaderQuiesceWithoutLiveSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	key := "0123456789012345678901234567890123456789012345678901234567890123"
	if err := os.WriteFile(keyFile, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	keyring, err := security.LoadKeyring(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connection, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAuthAttempt(ctx, "owner@example.com", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := authbrowser.EncodeHandoff(authbrowser.Handoff{
		Version: 1,
		URL:     "https://online.acb.com.vn/acbib/AccountSummary",
		Cookies: []authbrowser.Cookie{{Name: "session", Value: "secret", Domain: ".online.acb.com.vn", Path: "/", Secure: true}},
	}, []byte("handoff"))
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := keyring.Encrypt([]byte(plaintext), []byte("acb-session:"+connection.ID))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(envelope)
	if _, err := store.CompleteAuthSession(ctx, attempt.ID, encoded); err != nil {
		t.Fatal(err)
	}

	// Candidate worker has no live snapshot (e.g. newly started, hasn't run poll cycle yet)
	snapshotter := &mockSessionSnapshotter{
		snapshotErr: acb.ErrAuthenticatedFormStateUnavailable,
	}
	loader := NewSessionLoader(store, keyring, snapshotter)

	// Case 1: durable current-generation session exists in database -> quiesce Persist succeeds without overwrite
	if err := loader.Persist(ctx, connection.ID, attempt.Generation); err != nil {
		t.Fatalf("expected quiesce to succeed reusing durable session, got: %v", err)
	}

	// Case 2: durable session does NOT exist in database (wrong generation / missing) -> fail closed
	if err := loader.Persist(ctx, connection.ID, attempt.Generation+1); err == nil {
		t.Fatal("expected quiesce to fail closed when durable session does not exist")
	}
}
