package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestSessionRestoreLossCreatesDurableRecovery(t *testing.T) {
	for _, tc := range []struct{ name, reason, state string }{
		{"missing", "SESSION_MISSING", "DETECTED"},
		{"invalid", "SESSION_INVALID", "DETECTED"},
		{"invalid_nonce", "SESSION_INVALID", "DETECTED"},
		{"invalid_handoff", "SESSION_INVALID", "DETECTED"},
		{"decrypt", "SESSION_DECRYPT_FAILED", "MANUAL_REQUIRED"},
		{"closed_db", "", ""},
		{"stale", "", ""},
		{"restorer_error", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			conn, err := store.ConfigureConnection(ctx, "***1234")
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := store.StartAuthAttempt(ctx, "operator", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			keyring, err := security.NewKeyring(make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			plaintext := "invalid handoff"
			if tc.name == "restorer_error" {
				plaintext, err = authbrowser.EncodeHandoff(authbrowser.Handoff{Version: 1, URL: "https://online.acb.com.vn/acbib/AccountSummary", Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic", Domain: ".online.acb.com.vn", Path: "/"}}}, []byte("synthetic"))
				if err != nil {
					t.Fatal(err)
				}
			}
			env, err := keyring.Encrypt([]byte(plaintext), security.SessionAAD(conn.ID, attempt.Generation))
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "decrypt" {
				env.Ciphertext = "AAAAAAAAAAAAAAAAAAAAAA"
			}
			if tc.name == "invalid_nonce" {
				env.Nonce = "AA"
			}
			encoded, _ := json.Marshal(env)
			if tc.name == "invalid" {
				encoded = []byte("{invalid")
			}
			conn, err = store.CompleteAuthSession(ctx, attempt.ID, encoded)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.DB().ExecContext(ctx, `INSERT INTO checkpoints(connection_id,coverage_from,coverage_to,updated_at) VALUES(?,'2026-09-01','2026-09-02','preserved')`, conn.ID)
			if err != nil {
				t.Fatal(err)
			}
			var before int
			if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM event_journal`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if tc.name == "missing" || tc.name == "stale" {
				if _, err := store.DB().ExecContext(ctx, `DELETE FROM sessions WHERE connection_id=?`, conn.ID); err != nil {
					t.Fatal(err)
				}
			}
			generation := conn.Generation
			if tc.name == "stale" {
				generation--
			}
			if tc.name == "closed_db" {
				store.Close()
			}
			var restorer SessionRestorer = &recordingRestorer{}
			if tc.name == "restorer_error" {
				restorer = noRowsRestorer{}
			}
			loader := NewSessionLoader(store, keyring, restorer)
			if err := loader.Restore(ctx, conn.ID, generation); err == nil {
				t.Fatal("unusable session restored")
			}
			if tc.name == "closed_db" {
				return
			}
			current, err := store.Connection(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if tc.reason == "" {
				if current.Generation != conn.Generation || current.State != "MONITORING" {
					t.Fatal("operational/stale error mutated connection")
				}
				if _, err := store.LatestAuthRecoveryEpisode(ctx, conn.ID); !errors.Is(err, storage.ErrNotFound) {
					t.Fatalf("unexpected episode: %v", err)
				}
				return
			}
			if current.Generation != conn.Generation+1 || current.State != "AUTH_REQUIRED" {
				t.Fatalf("connection state=%s generation=%d", current.State, current.Generation)
			}
			e, err := store.LatestAuthRecoveryEpisode(ctx, conn.ID)
			if err != nil {
				t.Fatal(err)
			}
			if e.State != tc.state || e.ReasonCode != tc.reason {
				t.Fatalf("episode state=%s reason=%s", e.State, e.ReasonCode)
			}
			again, err := store.EnsureAuthRecoveryEpisode(ctx, conn.ID, current.Generation)
			if err != nil || again.ID != e.ID {
				t.Fatalf("episode duplicated: %v", err)
			}
			var count int
			if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_recovery_episodes`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("episodes=%d err=%v", count, err)
			}
			var checkpoint string
			if err := store.DB().QueryRowContext(ctx, `SELECT coverage_to FROM checkpoints WHERE connection_id=?`, conn.ID).Scan(&checkpoint); err != nil || checkpoint != "2026-09-02" {
				t.Fatal("checkpoint changed")
			}
			if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM event_journal`).Scan(&count); err != nil || count != before {
				t.Fatal("journal changed")
			}
			var notices int
			if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_recovery_notices WHERE episode_id=? AND kind='DETECTED'`, e.ID).Scan(&notices); err != nil || notices != 1 {
				t.Fatal("missing or duplicate detected notice")
			}
		})
	}
}

type noRowsRestorer struct{}

func (noRowsRestorer) RestoreSession(authbrowser.Handoff) error { return sql.ErrNoRows }
