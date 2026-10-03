package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
)

func credentialStore(t *testing.T) (*Store, context.Context, Connection, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "credentials.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	keyring, err := security.NewKeyring(bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.WithKeyring(keyring)
	c, err := s.ImportACBCredentials(ctx, ACBCredentials{Username: "original-user", Password: " original-password ", AccountNumber: "001234567890"})
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, c, path
}

func credentialGrant(t *testing.T, s *Store, ctx context.Context, c Connection) (string, TelegramAuthAction) {
	t.Helper()
	a, err := s.CreateTelegramAuthAction(ctx, TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, ExpectedGeneration: c.Generation, Action: "UPDATE_CREDENTIALS"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverTelegramAuthAction(ctx, a.ID, 44); err != nil {
		t.Fatal(err)
	}
	a, disposition, err := s.ConsumeTelegramAuthAction(ctx, a.ID, 123, 456, 789, 44, time.Now())
	if err != nil || disposition != "CREDENTIAL_GRANT" {
		t.Fatalf("grant disposition=%s err=%v", disposition, err)
	}
	return a.CredentialGrantToken, a
}

func bindCredentialGrant(t *testing.T, s *Store, ctx context.Context, token string) ACBCredentialGrantView {
	t.Helper()
	view, err := s.ValidateACBCredentialGrant(ctx, token, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func TestACBCredentialsSaveEncryptedPreservesAccountAndRequiresConsent(t *testing.T) {
	s, ctx, c, path := credentialStore(t)
	old, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
	if err != nil {
		t.Fatal(err)
	}
	login, err := s.CreateTelegramAuthAction(ctx, TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, EpisodeID: old.ID, ExpectedGeneration: c.Generation, Action: "LOGIN"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverTelegramAuthAction(ctx, login.ID, 45); err != nil {
		t.Fatal(err)
	}
	token, action := credentialGrant(t, s, ctx, c)
	encoded, err := json.Marshal(action)
	if err != nil || bytes.Contains(encoded, []byte(token)) {
		t.Fatalf("raw token escaped transient action: %v", err)
	}
	view := bindCredentialGrant(t, s, ctx, token)
	if !view.CanSave || view.Revision != 1 || view.AccountMasked != "****7890" {
		t.Fatalf("view=%+v", view)
	}
	response, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"original-user", "original-password", "001234567890", token} {
		if bytes.Contains(response, []byte(secret)) {
			t.Fatal("grant response contains secret")
		}
	}
	if rev, err := s.SaveACBCredentials(ctx, token, "owner-a", 1, " new-user ", " new-password "); err != nil || rev != 2 {
		t.Fatalf("revision=%d err=%v", rev, err)
	}
	got, err := s.ReadACBCredentials(ctx, c.ID)
	if err != nil || got.Username != "new-user" || got.Password != " new-password " || got.AccountNumber != "001234567890" || got.Revision != 2 {
		t.Fatalf("credential round trip mismatch, err=%v", err)
	}
	current, err := s.Connection(ctx)
	if err != nil || current.ID != c.ID || current.State != "AUTH_REQUIRED" || current.Generation != c.Generation+1 {
		t.Fatalf("connection=%+v err=%v", current, err)
	}
	var config int64
	if err := s.DB().QueryRow(`SELECT config_revision FROM connections WHERE id=?`, c.ID).Scan(&config); err != nil || config != 1 {
		t.Fatalf("config=%d err=%v", config, err)
	}
	e, err := s.LatestAuthRecoveryEpisode(ctx, c.ID)
	if err != nil || e.ID == old.ID || e.ConsentActionID != "" || e.ConsentConsumedAt != "" || e.AttemptID != "" || e.ReasonCode != "CREDENTIALS_UPDATED" {
		t.Fatalf("episode=%+v err=%v", e, err)
	}
	if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); !errors.Is(err, ErrRecoveryConsentRequired) {
		t.Fatalf("save conferred login consent: %v", err)
	}
	if _, _, err := s.ConsumeTelegramAuthAction(ctx, login.ID, 123, 456, 789, 45, time.Now()); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("old button remained active: %v", err)
	}
	notices, err := s.PendingAuthRecoveryNotices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].Kind != "CREDENTIALS_UPDATED" || notices[0].EpisodeID != e.ID {
		t.Fatalf("notices=%+v", notices)
	}
	var audit string
	if err := s.DB().QueryRow(`SELECT details_json FROM audit_logs WHERE action='ACB_CREDENTIALS_UPDATED'`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"new-user", "new-password", "001234567890", token} {
		if strings.Contains(audit, secret) {
			t.Fatal("audit contains secret")
		}
		for _, filename := range []string{path, path + "-wal"} {
			data, err := os.ReadFile(filename)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte(secret)) {
				t.Fatal("database contains plaintext secret")
			}
		}
	}
	if _, err := s.SaveACBCredentials(ctx, token, "owner-a", 1, "other", "other"); !errors.Is(err, ErrCredentialGrantExpired) {
		t.Fatalf("replay=%v", err)
	}
	if _, err := s.ImportACBCredentials(ctx, ACBCredentials{Username: "legacy", Password: "legacy", AccountNumber: "001234567890"}); !errors.Is(err, ErrCredentialsAlreadyConfigured) {
		t.Fatalf("legacy overwrite=%v", err)
	}
}

func TestACBCredentialGrantBindingRevocationExpiryAndFences(t *testing.T) {
	for _, scenario := range []string{"unbound-save", "different-owner", "expired", "revoked", "generation", "config", "revision", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			s, ctx, c, _ := credentialStore(t)
			token, _ := credentialGrant(t, s, ctx, c)
			want := ErrCredentialGrantExpired
			if scenario != "unbound-save" {
				bindCredentialGrant(t, s, ctx, token)
			}
			owner := "owner-a"
			var err error
			switch scenario {
			case "different-owner":
				owner = "owner-b"
			case "expired":
				_, err = s.DB().Exec(`UPDATE acb_credential_grants SET expires_at=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano))
			case "revoked":
				credentialGrant(t, s, ctx, c)
			case "generation":
				_, err = s.DB().Exec(`UPDATE connections SET generation=generation+1`)
				want = ErrCredentialsRevisionConflict
			case "config":
				_, err = s.DB().Exec(`UPDATE connections SET config_revision=config_revision+1`)
				want = ErrCredentialsRevisionConflict
			case "revision":
				_, err = s.DB().Exec(`UPDATE acb_credentials SET revision=revision+1`)
				want = ErrCredentialsRevisionConflict
			case "malformed":
				token = "not-a-token"
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SaveACBCredentials(ctx, token, owner, 1, "changed", "changed"); !errors.Is(err, want) {
				t.Fatalf("save=%v want=%v", err, want)
			}
			if scenario != "unbound-save" {
				if _, err := s.ValidateACBCredentialGrant(ctx, token, owner); !errors.Is(err, want) {
					t.Fatalf("validate=%v want=%v", err, want)
				}
			}
			var status string
			if err := s.DB().QueryRow(`SELECT status FROM acb_credential_grants ORDER BY created_at LIMIT 1`).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status == "CONSUMED" {
				t.Fatal("invalid grant was consumed")
			}
		})
	}
}

func TestACBCredentialSaveBusyLeavesGrantReusable(t *testing.T) {
	for _, scenario := range []string{"monitoring", "starting", "attempt", "logout", "catchup", "history"} {
		t.Run(scenario, func(t *testing.T) {
			s, ctx, c, _ := credentialStore(t)
			token, _ := credentialGrant(t, s, ctx, c)
			bindCredentialGrant(t, s, ctx, token)
			var err error
			switch scenario {
			case "monitoring":
				_, err = s.DB().Exec(`UPDATE connections SET state='MONITORING'`)
			case "starting":
				_, err = s.DB().Exec(`UPDATE connections SET state='AUTH_STARTING'`)
			case "attempt":
				_, err = s.DB().Exec(`INSERT INTO auth_attempts(id,connection_id,generation,status,expires_at,created_at) VALUES('busy',?,?,'IN_PROGRESS',?,?)`, c.ID, c.Generation, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano), now())
			case "logout":
				_, err = s.DB().Exec(`INSERT INTO acb_logout_jobs(id,connection_id,old_generation,fenced_generation,state,created_at,updated_at) VALUES('busy',?,0,1,'CLEARING',?,?)`, c.ID, now(), now())
			case "catchup":
				_, err = s.DB().Exec(`INSERT INTO recovery_runs(id,connection_id,generation,event_key,status,created_at,updated_at) VALUES('busy',?,0,'busy','RUNNING',?,?)`, c.ID, now(), now())
			case "history":
				_, err = s.DB().Exec(`INSERT INTO history_sync_jobs(id,connection_id,range_from,range_to,status,created_at,updated_at) VALUES('busy',?,'2026-09-01','2026-09-02','RUNNING',?,?)`, c.ID, now(), now())
			}
			if err != nil {
				t.Fatal(err)
			}
			view, err := s.ValidateACBCredentialGrant(ctx, token, "owner-a")
			if err != nil || view.CanSave || view.BlockedReason != "ACB_SESSION_BUSY" {
				t.Fatalf("busy view=%+v err=%v", view, err)
			}
			if _, err := s.SaveACBCredentials(ctx, token, "owner-a", 1, "changed", "changed"); !errors.Is(err, ErrACBSessionBusy) {
				t.Fatalf("busy save=%v", err)
			}
			var status string
			if err := s.DB().QueryRow(`SELECT status FROM acb_credential_grants`).Scan(&status); err != nil || status != "PENDING" {
				t.Fatalf("status=%s err=%v", status, err)
			}
			for _, q := range []string{`UPDATE connections SET state='AUTH_REQUIRED'`, `UPDATE auth_attempts SET status='CANCELLED'`, `UPDATE acb_logout_jobs SET finished_at='finished'`, `UPDATE recovery_runs SET status='CANCELED'`, `UPDATE history_sync_jobs SET status='CANCELED'`} {
				if _, err := s.DB().Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.SaveACBCredentials(ctx, token, "owner-a", 1, "changed", "changed"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestACBCredentialSaveConcurrentSingleCommitAndRollback(t *testing.T) {
	s, ctx, c, _ := credentialStore(t)
	token, _ := credentialGrant(t, s, ctx, c)
	bindCredentialGrant(t, s, ctx, token)
	if _, err := s.DB().Exec(`CREATE TRIGGER reject_credential_audit BEFORE INSERT ON audit_logs BEGIN SELECT RAISE(ABORT,'fixture database failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveACBCredentials(ctx, token, "owner-a", 1, "changed", "changed"); !errors.Is(err, ErrCredentialsUnavailable) {
		t.Fatalf("failure=%v", err)
	}
	got, err := s.ReadACBCredentials(ctx, c.ID)
	if err != nil || got.Revision != 1 || got.Password != " original-password " {
		t.Fatalf("rollback failed err=%v", err)
	}
	after, err := s.Connection(ctx)
	if err != nil || after.Generation != c.Generation {
		t.Fatalf("generation rollback=%+v err=%v", after, err)
	}
	if _, err := s.ValidateACBCredentialGrant(ctx, token, "owner-a"); err != nil {
		t.Fatalf("grant lost on rollback: %v", err)
	}
	if _, err := s.DB().Exec(`DROP TRIGGER reject_credential_audit`); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var results [2]error
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = s.SaveACBCredentials(ctx, token, "owner-a", 1, "changed", " changed ")
		}(i)
	}
	close(start)
	wg.Wait()
	commits := 0
	for _, err := range results {
		if err == nil {
			commits++
		} else if !errors.Is(err, ErrCredentialGrantExpired) {
			t.Fatal(err)
		}
	}
	if commits != 1 {
		t.Fatalf("commits=%d", commits)
	}
	got, err = s.ReadACBCredentials(ctx, c.ID)
	if err != nil || got.Revision != 2 || got.Password != " changed " {
		t.Fatalf("single commit failed err=%v", err)
	}
}

func TestACBCredentialsReadFailsClosedOnCorruption(t *testing.T) {
	for _, scenario := range []string{"missing", "ciphertext", "revision", "key", "nil-keyring"} {
		t.Run(scenario, func(t *testing.T) {
			s, ctx, c, _ := credentialStore(t)
			want := ErrCredentialsDecryptFailed
			var err error
			switch scenario {
			case "missing":
				_, err = s.DB().Exec(`DELETE FROM acb_credentials`)
				want = ErrCredentialsNotConfigured
			case "ciphertext":
				_, err = s.DB().Exec(`UPDATE acb_credentials SET envelope=X'00'`)
			case "revision":
				_, err = s.DB().Exec(`UPDATE acb_credentials SET revision=2`)
			case "key":
				key, _ := security.NewKeyring(bytes.Repeat([]byte{0x52}, 32))
				s.WithKeyring(key)
			case "nil-keyring":
				s.WithKeyring(nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.ReadACBCredentials(ctx, c.ID)
			if !errors.Is(err, want) || got.Password != "" || got.AccountNumber != "" {
				t.Fatalf("read did not fail closed: %v", err)
			}
		})
	}
}

func TestACBCredentialsImportChecksExactEncryptedSessionAccount(t *testing.T) {
	for _, scenario := range []string{"match", "same-last-four", "missing-field", "corrupt", "legacy", "cookie-only", "expired-empty", "expired-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			s, ctx, c, _ := credentialStore(t)
			if _, err := s.DB().Exec(`DELETE FROM acb_credentials`); err != nil {
				t.Fatal(err)
			}
			account := "001234567890"
			if scenario == "same-last-four" || scenario == "expired-mismatch" {
				account = "999999997890"
			}
			if scenario == "missing-field" || scenario == "expired-empty" {
				account = ""
			}
			handoff, err := authbrowser.EncodeHandoff(authbrowser.Handoff{Version: 1, Fields: map[string]string{"AccountNbr": account}, Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic"}}}, []byte("nonce"))
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "cookie-only" {
				payload, err := json.Marshal([]authbrowser.Cookie{{Name: "session", Value: "synthetic"}})
				if err != nil {
					t.Fatal(err)
				}
				handoff = base64.RawURLEncoding.EncodeToString(payload) + ".synthetic"
			}
			aad := security.SessionAAD(c.ID, c.Generation)
			if scenario == "legacy" {
				aad = security.LegacySessionAAD(c.ID)
			}
			env, err := s.keyring.Encrypt([]byte(handoff), aad)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "corrupt" {
				encoded = []byte("invalid")
			}
			if _, err := s.DB().Exec(`INSERT INTO sessions(connection_id,generation,envelope,key_id,updated_at) VALUES(?,?,?,?,?)`, c.ID, c.Generation, encoded, env.KeyID, now()); err != nil {
				t.Fatal(err)
			}
			if scenario == "expired-empty" || scenario == "expired-mismatch" {
				if _, err := s.DB().Exec(`UPDATE connections SET state='AUTH_REQUIRED',generation=generation+1 WHERE id=?`, c.ID); err != nil {
					t.Fatal(err)
				}
			}
			imported, err := s.ImportACBCredentials(ctx, ACBCredentials{Username: "user", Password: " password ", AccountNumber: "001234567890"})
			switch scenario {
			case "match", "legacy", "cookie-only", "expired-empty":
				if err != nil || imported.ID != c.ID {
					t.Fatalf("import=%+v err=%v", imported, err)
				}
			case "corrupt":
				if !errors.Is(err, ErrCredentialsDecryptFailed) {
					t.Fatalf("import=%v", err)
				}
			default:
				if !errors.Is(err, ErrCredentialAccountMismatch) {
					t.Fatalf("import=%v", err)
				}
			}
			if err != nil {
				if _, readErr := s.ReadACBCredentials(ctx, c.ID); !errors.Is(readErr, ErrCredentialsNotConfigured) {
					t.Fatalf("failed import wrote credentials: %v", readErr)
				}
			}
		})
	}
}

func TestACBCredentialMutationFenceAndInputBoundaries(t *testing.T) {
	s, ctx, c, _ := credentialStore(t)
	token, _ := credentialGrant(t, s, ctx, c)
	bindCredentialGrant(t, s, ctx, token)
	for _, input := range [][2]string{{"", "pw"}, {"user", ""}, {"u\n", "pw"}, {"user", "p\r"}, {"u\x00", "pw"}, {strings.Repeat("u", 257), "pw"}, {"user", strings.Repeat("p", 1025)}} {
		if _, err := s.SaveACBCredentials(ctx, token, "owner-a", 1, input[0], input[1]); !errors.Is(err, ErrInvalidCredentialInput) {
			t.Fatalf("input err=%v", err)
		}
	}
	if _, err := s.SaveACBCredentials(ctx, token, "owner-a", 2, "user", "pw"); !errors.Is(err, ErrCredentialsRevisionConflict) {
		t.Fatalf("revision=%v", err)
	}
	if _, err := s.DB().Exec(`UPDATE deployment_control SET gate_state='LOCKED',owner='restore',lease_token='fixture',lease_expires_at=? WHERE id='singleton'`, time.Now().Add(time.Minute).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"validate": func() error { _, err := s.ValidateACBCredentialGrant(ctx, token, "owner-a"); return err },
		"save":     func() error { _, err := s.SaveACBCredentials(ctx, token, "owner-a", 1, "user", "pw"); return err },
		"import": func() error {
			_, err := s.ImportACBCredentials(ctx, ACBCredentials{Username: "user", Password: "pw", AccountNumber: "001234567890"})
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrMutationGateLocked) {
			t.Fatalf("%s bypassed fence: %v", name, err)
		}
	}
}

func TestACBCredentialGrantOnlyAuthenticatedConsumptionMintsToken(t *testing.T) {
	s, ctx, c, _ := credentialStore(t)
	a, err := s.CreateTelegramAuthAction(ctx, TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, ExpectedGeneration: c.Generation, Action: "UPDATE_CREDENTIALS"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverTelegramAuthAction(ctx, a.ID, 44); err != nil {
		t.Fatal(err)
	}
	if result, _, err := s.ConsumeTelegramAuthAction(ctx, a.ID, 123, 456, 790, 44, time.Now()); !errors.Is(err, ErrChallengeMismatch) || result.CredentialGrantToken != "" {
		t.Fatalf("wrong identity minted grant: %v", err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM acb_credential_grants`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unauthorized grants=%d err=%v", count, err)
	}
	result, _, err := s.ConsumeTelegramAuthAction(ctx, a.ID, 123, 456, 789, 44, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	hash, err := credentialTokenHash(result.CredentialGrantToken)
	if err != nil {
		t.Fatal(err)
	}
	var storedHash, status string
	var owner *string
	if err := s.DB().QueryRow(`SELECT token_hash,status,owner_subject FROM acb_credential_grants`).Scan(&storedHash, &status, &owner); err != nil {
		t.Fatal(err)
	}
	if storedHash != hash || status != "PENDING" || owner != nil {
		t.Fatal("grant persisted token or was prematurely bound/consumed")
	}
	if replay, _, err := s.ConsumeTelegramAuthAction(ctx, a.ID, 123, 456, 789, 44, time.Now()); !errors.Is(err, ErrChallengeConsumed) || replay.CredentialGrantToken != "" {
		t.Fatalf("replay minted grant: %v", err)
	}
	bindCredentialGrant(t, s, ctx, result.CredentialGrantToken)
	bindCredentialGrant(t, s, ctx, result.CredentialGrantToken)
	if err := s.DB().QueryRow(`SELECT status FROM acb_credential_grants`).Scan(&status); err != nil || status != "PENDING" {
		t.Fatalf("validation consumed grant: %v", err)
	}
}

func TestACBCredentialSaveVersusRecoveryAdmission(t *testing.T) {
	for range 8 {
		s, ctx, c, _ := credentialStore(t)
		e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
		if err != nil {
			t.Fatal(err)
		}
		token, _ := credentialGrant(t, s, ctx, c)
		bindCredentialGrant(t, s, ctx, token)
		consentRecovery(t, s, ctx, e)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var saveErr, admitErr error
		var attempt AuthAttempt
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, saveErr = s.SaveACBCredentials(ctx, token, "owner-a", 1, "changed", " changed ")
		}()
		go func() {
			defer wg.Done()
			<-start
			attempt, admitErr = s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
		}()
		close(start)
		wg.Wait()
		got, err := s.ReadACBCredentials(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saveErr == nil {
			if !errors.Is(admitErr, ErrRecoverySuperseded) || got.Revision != 2 || got.Password != " changed " {
				t.Fatalf("save winner admit=%v revision=%d", admitErr, got.Revision)
			}
		} else {
			if admitErr != nil || (!errors.Is(saveErr, ErrACBSessionBusy) && !errors.Is(saveErr, ErrCredentialsRevisionConflict)) || got.Revision != 1 || got.Password != " original-password " {
				t.Fatalf("admission winner save=%v admit=%v revision=%d", saveErr, admitErr, got.Revision)
			}
			current, err := s.AuthRecoveryEpisode(ctx, e.ID)
			if err != nil || current.AttemptID != attempt.ID || current.CredentialRevision != 1 {
				t.Fatalf("mixed credential admission err=%v", err)
			}
		}
	}
}

func TestACBCredentialInitialImportRollsBackConnectionOnEncryptionFailure(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	input := ACBCredentials{Username: "user", Password: " password ", AccountNumber: "001234567890"}
	if _, err := s.ImportACBCredentials(ctx, input); !errors.Is(err, ErrCredentialsUnavailable) {
		t.Fatalf("nil keyring import=%v", err)
	}
	if _, err := s.Connection(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed import left connection: %v", err)
	}
	key, err := security.NewKeyring(bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.WithKeyring(key)
	c, err := s.ImportACBCredentials(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadACBCredentials(ctx, c.ID)
	if err != nil || got.Password != input.Password || got.AccountNumber != input.AccountNumber || got.Revision != 1 {
		t.Fatalf("import round trip=%v", err)
	}
}
