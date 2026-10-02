package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoveryRequiresButtonNotReasonOrResume(t *testing.T) {
	s, ctx, e := recoveryStore(t)
	if _, err := s.DB().ExecContext(ctx, `UPDATE auth_recovery_episodes SET reason_code='OPERATOR_CONFIRMED' WHERE id=?`, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TelegramAuthState(ctx, 123); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyTelegramAuthOperation(ctx, 123, e.ID, e.Generation, "RESUME"); err != nil {
		t.Fatal(err)
	}
	if err := s.RearmAuthRecovery(ctx, e.ID, e.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); !errors.Is(err, ErrRecoveryConsentRequired) {
		t.Fatalf("reason/resume/rearm authorized login: %v", err)
	}
	if _, err := s.StartAuthAttempt(ctx, RecoveryOwner, time.Minute); !errors.Is(err, ErrRecoveryConsentRequired) {
		t.Fatalf("generic recovery-owner admission bypassed consent: %v", err)
	}
	var count int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_attempts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("attempts=%d err=%v", count, err)
	}
}

func TestRecoveryConsentSingleAttemptAndBoundGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.db")
	s, ctx, e := recoveryStoreAt(t, path)
	action := consentRecovery(t, s, ctx, e)
	other, err := OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	a, err := other.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	current, err := other.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ConsentActionID != action.ID || current.ConsentConsumedAt == "" || current.CredentialRevision != 1 || current.Generation != action.ExpectedGeneration+1 {
		t.Fatalf("unbound consent: %+v", current)
	}
	expiry, err := time.Parse(time.RFC3339Nano, a.ExpiresAt)
	if err != nil || expiry.After(time.Now().Add(15*time.Minute)) {
		t.Fatalf("attempt exceeded TTL: %s %v", a.ExpiresAt, err)
	}
	if err := other.CheckRecoveryAuthAttempt(ctx, e.ID, a.Generation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.ConsumeTelegramAuthAction(ctx, action.ID, 123, 456, 789, 44, time.Now()); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("click replay: %v", err)
	}
	if _, err := other.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); !errors.Is(err, ErrRecoverySuperseded) {
		t.Fatalf("generation replay: %v", err)
	}
	if err := other.FinishRecoveryAuthAttempt(ctx, e.ID, a.Generation, "FAILED", "WAIT_OPERATOR", "LOGIN_OUTCOME_UNKNOWN", time.Time{}); err != nil {
		t.Fatal(err)
	}
	current, err = other.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || current.ConsentActionID != "" || current.ConsentConsumedAt != "" || current.ConsentExpiresAt != "" {
		t.Fatalf("failure retained consent: %+v %v", current, err)
	}
	if _, err := other.StartRecoveryAuthAttempt(ctx, e.ID, current.Generation, time.Minute); err == nil {
		t.Fatal("failure auto-restarted browser")
	}
}

func TestRecoveryAdmissionRejectsExpiredOrChangedConsent(t *testing.T) {
	for _, kind := range []string{"expired", "action-config", "action-generation", "action-identity", "consumed", "no-credentials"} {
		t.Run(kind, func(t *testing.T) {
			s, ctx, e := recoveryStore(t)
			action := consentRecovery(t, s, ctx, e)
			var err error
			switch kind {
			case "expired":
				_, err = s.DB().ExecContext(ctx, `UPDATE auth_recovery_episodes SET consent_expires_at=? WHERE id=?`, now(), e.ID)
			case "action-config":
				_, err = s.DB().ExecContext(ctx, `UPDATE telegram_auth_actions SET expected_config_revision=expected_config_revision+1 WHERE id=?`, action.ID)
			case "action-generation":
				_, err = s.DB().ExecContext(ctx, `UPDATE telegram_auth_actions SET expected_generation=expected_generation+1 WHERE id=?`, action.ID)
			case "action-identity":
				_, err = s.DB().ExecContext(ctx, `UPDATE telegram_auth_actions SET user_id=0 WHERE id=?`, action.ID)
			case "consumed":
				_, err = s.DB().ExecContext(ctx, `UPDATE auth_recovery_episodes SET consent_consumed_at=? WHERE id=?`, now(), e.ID)
			case "no-credentials":
				_, err = s.DB().ExecContext(ctx, `DELETE FROM acb_credentials WHERE connection_id=?`, e.ConnectionID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); err == nil {
				t.Fatal("invalid consent admitted")
			}
			var count int
			if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_attempts`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("attempts=%d err=%v", count, err)
			}
		})
	}
}

func TestRecoverySubmissionCannotOutliveConsumedConsent(t *testing.T) {
	for _, mutation := range []string{"consent", "action", "credentials", "deploy"} {
		t.Run(mutation, func(t *testing.T) {
			s, ctx, e := recoveryStore(t)
			action := consentRecovery(t, s, ctx, e)
			a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.TransitionAuthRecovery(ctx, e.ID, a.Generation, "STARTING", "LOGIN", ""); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "consent":
				_, err = s.DB().ExecContext(ctx, `UPDATE auth_recovery_episodes SET consent_consumed_at=NULL WHERE id=?`, e.ID)
			case "action":
				_, err = s.DB().ExecContext(ctx, `UPDATE telegram_auth_actions SET attempt_id='different' WHERE id=?`, action.ID)
			case "credentials":
				_, err = s.DB().ExecContext(ctx, `UPDATE acb_credentials SET revision=revision+1 WHERE connection_id=?`, e.ConnectionID)
			case "deploy":
				// Deployment admission normally waits for attempts. Emulate a restore fence
				// established independently after admission to prove each later I/O checks it.
				_, err = s.DB().ExecContext(ctx, `UPDATE deployment_control SET gate_state='LOCKED',owner='restore',lease_token='fixture',lease_expires_at=?,fence_generation=fence_generation+1 WHERE id='singleton'`, time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
			}
			if err != nil {
				t.Fatal(err)
			}
			for name, call := range map[string]func() error{
				"observe":    func() error { return s.CheckRecoveryAuthAttempt(ctx, e.ID, a.Generation) },
				"login":      func() error { return s.RecordRecoveryLogin(ctx, e.ID, a.Generation) },
				"submission": func() error { return s.ReserveRecoverySubmission(ctx, e.ID, a.Generation, "CAPTCHA_TEXT", true) },
				"AI":         func() error { return s.ClaimRecoveryAI(ctx, e.ID, a.Generation, "captcha-A") },
			} {
				if err := call(); err == nil {
					t.Fatalf("%s crossed %s fence", name, mutation)
				}
			}
			current, err := s.AuthRecoveryEpisode(ctx, e.ID)
			if err != nil || current.LastLoginAt != "" || current.CaptchaSubmissions != 0 || current.AIUsed != 0 {
				t.Fatalf("denied I/O spent budget: %+v %v", current, err)
			}
		})
	}
}

func TestTelegramLoginBeforeEpisodeBindsExactAction(t *testing.T) {
	s, ctx, e := recoveryStore(t)
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM auth_recovery_notices; DELETE FROM auth_recovery_episodes`); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateTelegramAuthAction(ctx, TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, ExpectedGeneration: e.Generation, Action: "LOGIN"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverTelegramAuthAction(ctx, a.ID, 44); err != nil {
		t.Fatal(err)
	}
	a, disposition, err := s.ConsumeTelegramAuthAction(ctx, a.ID, 123, 456, 789, 44, time.Now())
	if err != nil || disposition != "REARMED" || a.EpisodeID == "" {
		t.Fatalf("unbound action: %+v %s %v", a, disposition, err)
	}
	episode, err := s.AuthRecoveryEpisode(ctx, a.EpisodeID)
	if err != nil || episode.ConsentActionID != a.ID {
		t.Fatalf("wrong consent: %+v %v", episode, err)
	}
	if _, err := s.StartRecoveryAuthAttempt(ctx, episode.ID, episode.Generation, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramConsentConfigAndDeployFence(t *testing.T) {
	for _, fence := range []string{"config", "deploy"} {
		t.Run(fence, func(t *testing.T) {
			s, ctx, e := recoveryStore(t)
			a, err := s.CreateTelegramAuthAction(ctx, TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, EpisodeID: e.ID, ExpectedGeneration: e.Generation, Action: "LOGIN"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.DeliverTelegramAuthAction(ctx, a.ID, 44); err != nil {
				t.Fatal(err)
			}
			want := ErrRecoverySuperseded
			if fence == "config" {
				_, err = s.DB().ExecContext(ctx, `UPDATE connections SET config_revision=config_revision+1 WHERE id=?`, e.ConnectionID)
			} else {
				_, err = s.AcquireMutationGate(ctx, "deploy", time.Minute, "test")
				want = ErrMutationGateLocked
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.ConsumeTelegramAuthAction(ctx, a.ID, 123, 456, 789, 44, time.Now()); !errors.Is(err, want) {
				t.Fatalf("fence=%s err=%v", fence, err)
			}
			episode, err := s.AuthRecoveryEpisode(ctx, e.ID)
			if err != nil || episode.ConsentActionID != "" {
				t.Fatalf("denied click granted consent: %+v %v", episode, err)
			}
			var status string
			if err := s.DB().QueryRowContext(ctx, `SELECT status FROM telegram_auth_actions WHERE id=?`, a.ID).Scan(&status); err != nil || status != "PENDING" {
				t.Fatalf("failed transaction consumed click: %s %v", status, err)
			}
		})
	}
}

func TestSessionControlMigrationCancelsOnlyUnsafeLegacyLogin(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,checksum TEXT NOT NULL,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:12] {
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES(?,?,?)`, m.version, m.checksum, now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ id, state, attempt, episode string }{
		{"unsafe", "AUTH_STARTING", "IN_PROGRESS", "LOGIN"},
		{"healthy", "MONITORING", "VERIFIED", "COMPLETED"},
		{"catchup", "MONITORING", "VERIFIED", "CATCHING_UP"},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO connections(id,state,generation,config_revision,created_at,updated_at) VALUES(?,?,7,2,?,?)`, c.id, c.state, now(), now()); err != nil {
			t.Fatal(err)
		}
		var finished any
		if c.attempt == "VERIFIED" {
			finished = now()
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO auth_attempts(id,connection_id,generation,owner_subject,status,expires_at,created_at,finished_at) VALUES(?,?,7,?,?,?,?,?)`, c.id+"-attempt", c.id, RecoveryOwner, c.attempt, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano), now(), finished); err != nil {
			t.Fatal(err)
		}
		finished = nil
		var run any
		if c.episode == "COMPLETED" {
			finished = now()
		}
		if c.episode == "CATCHING_UP" {
			run = "committed-run"
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO auth_recovery_episodes(id,connection_id,trigger_generation,generation,config_revision,attempt_id,state,recovery_run_id,created_at,updated_at,finished_at) VALUES(?,?,6,7,2,?,?,?,?,?,?)`, c.id+"-episode", c.id, c.id+"-attempt", c.episode, run, now(), now(), finished); err != nil {
			t.Fatal(err)
		}
		if c.state == "MONITORING" {
			if _, err := db.ExecContext(ctx, `INSERT INTO sessions(connection_id,generation,envelope,key_id,verified_at,updated_at) VALUES(?,7,X'00','fixture',?,?)`, c.id, now(), now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	unsafe, err := s.AuthRecoveryEpisode(ctx, "unsafe-episode")
	if err != nil || unsafe.State != "WAIT_OPERATOR" || unsafe.Generation != 8 || unsafe.ConsentActionID != "" {
		t.Fatalf("unsafe legacy admission survived: %+v %v", unsafe, err)
	}
	var status string
	if err := s.DB().QueryRowContext(ctx, `SELECT status FROM auth_attempts WHERE id='unsafe-attempt'`).Scan(&status); err != nil || status != "CANCELLED" {
		t.Fatalf("unsafe attempt=%s %v", status, err)
	}
	for _, id := range []string{"healthy", "catchup"} {
		var generation int64
		if err := s.DB().QueryRowContext(ctx, `SELECT state,generation FROM connections WHERE id=?`, id).Scan(&status, &generation); err != nil || status != "MONITORING" || generation != 7 {
			t.Fatalf("healthy fence changed: %s %d %v", status, generation, err)
		}
		var count int
		if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE connection_id=? AND generation=7`, id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("healthy session removed: %d %v", count, err)
		}
	}
	catchup, err := s.AuthRecoveryEpisode(ctx, "catchup-episode")
	if err != nil || catchup.State != "CATCHING_UP" || catchup.RecoveryRunID != "committed-run" {
		t.Fatalf("committed catchup lost: %+v %v", catchup, err)
	}
}

func TestTelegramCatchupRetryDoesNotGrantLogin(t *testing.T) {
	ctx, s, e, run := automaticCompletionFixture(t)
	if _, err := s.ClaimRecoveryRun(ctx, run.ID, e.ConnectionID, e.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRecoveryRunProgress(ctx, run.ID, e.ConnectionID, e.Generation, RecoveryRunStatusFailed, "{}", "UPSTREAM_UNAVAILABLE", ""); err != nil {
		t.Fatal(err)
	}
	e, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	action, err := s.CreateTelegramAuthAction(ctx, TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, EpisodeID: e.ID, ExpectedGeneration: e.Generation, Action: "RETRY"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverTelegramAuthAction(ctx, action.ID, 44); err != nil {
		t.Fatal(err)
	}
	_, disposition, err := s.ConsumeTelegramAuthAction(ctx, action.ID, 123, 456, 789, 44, time.Now())
	if err != nil || disposition != "CATCHUP_RETRY" {
		t.Fatalf("retry=%s err=%v", disposition, err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || e.State != "CATCHING_UP" || e.ConsentActionID != "" || e.ConsentConsumedAt != "" {
		t.Fatalf("catchup authorized login: %+v %v", e, err)
	}
	run, err = s.GetRecoveryRun(ctx, run.ID)
	if err != nil || run.Status != RecoveryRunStatusPending {
		t.Fatalf("retry did not reuse durable run: %+v %v", run, err)
	}
	var count int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_attempts`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("catchup opened browser: count=%d err=%v", count, err)
	}
}
