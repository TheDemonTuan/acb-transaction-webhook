package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func logoutAction(t *testing.T, s *Store, ctx context.Context, c Connection) ACBLogoutJob {
	t.Helper()
	if _, err := s.TelegramAuthState(ctx, 123); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateTelegramAuthAction(ctx, TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, ExpectedGeneration: c.Generation, Action: "LOGOUT"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeliverTelegramAuthAction(ctx, a.ID, 44); err != nil {
		t.Fatal(err)
	}
	_, disposition, err := s.ConsumeTelegramAuthAction(ctx, a.ID, 123, 456, 789, 44, time.Now())
	if err != nil || disposition != "LOGOUT_QUEUED" {
		t.Fatalf("logout disposition=%s err=%v", disposition, err)
	}
	if _, _, err = s.ConsumeTelegramAuthAction(ctx, a.ID, 123, 456, 789, 44, time.Now()); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("logout replay: %v", err)
	}
	j, err := s.OpenACBLogoutJob(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestACBLogoutSnapshotsExactSessionGenerationAndFencesAllInputs(t *testing.T) {
	s, ctx, c, _ := credentialStore(t)
	token, _ := credentialGrant(t, s, ctx, c)
	e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
	if err != nil {
		t.Fatal(err)
	}
	// Session survives auth loss at an older generation than the connection.
	snapshot := []byte("encrypted-old-generation")
	if _, err = s.DB().ExecContext(ctx, `INSERT INTO sessions(connection_id,generation,envelope,key_id,updated_at) VALUES(?,?,?,'k1',?)`, c.ID, c.Generation-1, snapshot, now()); err != nil {
		t.Fatal(err)
	}
	run, _, err := s.EnsureRecoveryRun(ctx, c.ID, c.Generation, "logout-catchup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimRecoveryRun(ctx, run.ID, c.ID, c.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err = s.IngestTransaction(ctx, TransactionInput{ConnectionID: c.ID, SemanticKey: "logout-preserved", CanonicalHash: "hash", TransactionAt: "2026-10-03", EffectiveAt: "2026-10-03", Credit: 1, ParserVersion: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`INSERT INTO checkpoints(connection_id,scan_id,coverage_from,coverage_to,updated_at) VALUES(?,'scan-preserved','2026-10-01','2026-10-02',?)`, c.ID, now()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`INSERT INTO history_sync_jobs(id,connection_id,generation,range_from,range_to,status,created_at,updated_at) VALUES('logout-history',?,?,'2026-10-01','2026-10-02','RUNNING',?,?)`, c.ID, c.Generation, now(), now()); err != nil {
		t.Fatal(err)
	}
	j := logoutAction(t, s, ctx, c)
	if j.SessionGeneration != c.Generation-1 || !bytes.Equal(j.SessionEnvelope, snapshot) || j.FencedGeneration != c.Generation+1 {
		t.Fatal("logout lost exact session snapshot binding")
	}
	current, err := s.Connection(ctx)
	if err != nil || current.State != "AUTH_REQUIRED" || current.Generation != j.FencedGeneration {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	state, err := s.TelegramAuthState(ctx, 123)
	if err != nil || !state.Paused {
		t.Fatalf("paused=%v err=%v", state.Paused, err)
	}
	if err = s.CheckACBLogoutFence(ctx, c.ID, j.FencedGeneration); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckACBLogoutFence(ctx, c.ID, c.Generation); !errors.Is(err, ErrACBLogoutFence) {
		t.Fatalf("stale clear=%v", err)
	}
	if _, err = s.Session(ctx, c.ID, c.Generation-1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("persisted session remains: %v", err)
	}
	old, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || old.State != "SUPERSEDED" || old.ConsentActionID != "" {
		t.Fatalf("episode=%+v err=%v", old, err)
	}
	if _, err = s.UpdateRecoveryRunProgress(ctx, run.ID, c.ID, c.Generation, RecoveryRunStatusCompleted, `{}`, "", ""); !errors.Is(err, ErrGenerationFenceMismatch) {
		t.Fatalf("old catchup commit=%v", err)
	}
	if _, err = s.ValidateACBCredentialGrant(ctx, token, "owner"); !errors.Is(err, ErrCredentialGrantExpired) {
		t.Fatalf("grant survived=%v", err)
	}
	if _, err = s.StartAuthAttempt(ctx, "owner", time.Minute); !errors.Is(err, ErrACBSessionBusy) {
		t.Fatalf("admission before clear=%v", err)
	}
	fresh, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, j.FencedGeneration)
	if err != nil {
		t.Fatal(err)
	}
	login, err := s.CreateTelegramAuthAction(ctx, TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, EpisodeID: fresh.ID, ExpectedGeneration: j.FencedGeneration, Action: "LOGIN"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeliverTelegramAuthAction(ctx, login.ID, 45); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ConsumeTelegramAuthAction(ctx, login.ID, 123, 456, 789, 45, time.Now()); !errors.Is(err, ErrACBSessionBusy) {
		t.Fatalf("LOGIN before local ack=%v", err)
	}
	credentials, err := s.ReadACBCredentials(ctx, c.ID)
	if err != nil || credentials.Revision != 1 || credentials.Password != " original-password " {
		t.Fatal("logout changed credentials")
	}
	transactions, err := s.ListTransactionsPage(ctx, 10, "")
	if err != nil || len(transactions.Items) != 1 || transactions.Items[0].SemanticKey != "logout-preserved" {
		t.Fatalf("transactions lost: %+v %v", transactions, err)
	}
	var scan, status string
	if err = s.DB().QueryRow(`SELECT scan_id FROM checkpoints WHERE connection_id=?`, c.ID).Scan(&scan); err != nil || scan != "scan-preserved" {
		t.Fatalf("checkpoint lost: %s %v", scan, err)
	}
	if err = s.DB().QueryRow(`SELECT status FROM history_sync_jobs WHERE id='logout-history'`).Scan(&status); err != nil || status != "CANCELED" {
		t.Fatalf("history not cancelled: %s %v", status, err)
	}
}

func TestACBLogoutLocalAndBankResultsAreIndependentAndNoticeCAS(t *testing.T) {
	for _, bank := range []string{"CONFIRMED", "ALREADY_EXPIRED", "UNCONFIRMED"} {
		t.Run(bank, func(t *testing.T) {
			s, ctx, c, _ := credentialStore(t)
			j := logoutAction(t, s, ctx, c)
			if err := s.BeginACBLogoutRevocation(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishACBLogoutRevocation(ctx, j.ID, bank, "fixture-result"); err != nil {
				t.Fatal(err)
			}
			pending, err := s.PendingACBLogoutNotices(ctx)
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending=%d err=%v", len(pending), err)
			}
			before := pending[0]
			if before.State != "CLEARING" || before.LocalClearedAt != "" || before.BankStatus != bank || before.FinishedAt != "" {
				t.Fatalf("bank must not fabricate local ack: %+v", before)
			}
			if err = s.MarkACBLogoutLocalCleared(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.FinishACBLogoutNotice(ctx, j.ID, 77, before.UpdatedAt); !errors.Is(err, ErrACBLogoutFence) {
				t.Fatalf("stale delivery ack=%v", err)
			}
			final, err := s.ACBLogoutJob(ctx, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			expected := "COMPLETED"
			if bank == "UNCONFIRMED" {
				expected = "LOCAL_ONLY"
			}
			if final.State != expected || final.LocalClearedAt == "" || final.BankStatus != bank || final.FinishedAt == "" || final.SessionEnvelope != nil {
				t.Fatalf("final=%+v", final)
			}
			if err = s.FinishACBLogoutNotice(ctx, j.ID, 78, final.UpdatedAt); err != nil {
				t.Fatal(err)
			}
			pending, err = s.PendingACBLogoutNotices(ctx)
			if err != nil || len(pending) != 0 {
				t.Fatalf("notice replay after ack: %d %v", len(pending), err)
			}
			if err = s.CheckACBLogoutFence(ctx, c.ID, j.FencedGeneration); !errors.Is(err, ErrACBLogoutFence) {
				t.Fatalf("closed job accepted=%v", err)
			}
		})
	}
}

func TestACBLogoutCrashAndSnapshotExpiryDoNotInventLocalAck(t *testing.T) {
	for _, scenario := range []string{"in-flight", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			s, ctx, c, path := credentialStore(t)
			if _, err := s.DB().Exec(`INSERT INTO sessions(connection_id,generation,envelope,key_id,updated_at) VALUES(?,?,?,'k1',?)`, c.ID, c.Generation, []byte("snapshot"), now()); err != nil {
				t.Fatal(err)
			}
			j := logoutAction(t, s, ctx, c)
			reason := "REVOKE_WINDOW_EXPIRED"
			if scenario == "in-flight" {
				if err := s.BeginACBLogoutRevocation(ctx, j.ID); err != nil {
					t.Fatal(err)
				}
				reason = "LOGOUT_OUTCOME_UNKNOWN"
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err = reopened.RecoverACBLogoutRevocation(ctx, j.ID, time.Now().Add(6*time.Minute)); err != nil {
				t.Fatal(err)
			}
			recovered, err := reopened.ACBLogoutJob(ctx, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.State != "CLEARING" || recovered.LocalClearedAt != "" || recovered.BankStatus != "UNCONFIRMED" || recovered.BankReasonCode != reason || recovered.SessionEnvelope != nil {
				t.Fatalf("crash recovery=%+v", recovered)
			}
			if err = reopened.BeginACBLogoutRevocation(ctx, j.ID); !errors.Is(err, ErrACBLogoutFence) {
				t.Fatalf("replayed bank=%v", err)
			}
			if err = reopened.MarkACBLogoutLocalCleared(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
			final, err := reopened.ACBLogoutJob(ctx, j.ID)
			if err != nil || final.State != "LOCAL_ONLY" {
				t.Fatalf("local retry=%+v err=%v", final, err)
			}
		})
	}
}

func TestACBLogoutRacingFinalizerLeavesNoResurrectedSession(t *testing.T) {
	s, ctx, _, _ := credentialStore(t)
	attempt, err := s.StartAuthAttempt(ctx, "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.TelegramAuthState(ctx, 123); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateTelegramAuthAction(ctx, TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, ExpectedGeneration: c.Generation, Action: "LOGOUT"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeliverTelegramAuthAction(ctx, a.ID, 44); err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]string{"Version": "v1", "KeyID": "k1"})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan error, 2)
	go func() {
		defer wg.Done()
		<-start
		_, err := s.CompleteAuthSession(ctx, attempt.ID, envelope)
		results <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, _, err := s.ConsumeTelegramAuthAction(ctx, a.ID, 123, 456, 789, 44, time.Now())
		results <- err
	}()
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success == 0 {
		t.Fatal("neither operation committed")
	}
	current, err := s.Connection(ctx)
	if err != nil || current.State != "AUTH_REQUIRED" || current.Generation != attempt.Generation+1 {
		t.Fatalf("session resurrected: %+v %v", current, err)
	}
	var sessions int
	if err = s.DB().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("sessions=%d err=%v", sessions, err)
	}
	j, err := s.OpenACBLogoutJob(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if success == 1 && j.AttemptID != attempt.ID {
		t.Fatal("active browser lost before revoke")
	}
	if _, err = s.CompleteAuthSession(ctx, attempt.ID, envelope); err == nil {
		t.Fatal("old finalizer replay resurrected session")
	}
}

func TestACBLogoutBlocksCredentialSaveUntilLocalAck(t *testing.T) {
	s, ctx, c, _ := credentialStore(t)
	j := logoutAction(t, s, ctx, c)
	current, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := credentialGrant(t, s, ctx, current)
	view := bindCredentialGrant(t, s, ctx, token)
	if view.CanSave || view.BlockedReason != "ACB_SESSION_BUSY" {
		t.Fatalf("grant ignored logout: %+v", view)
	}
	if _, err = s.SaveACBCredentials(ctx, token, "owner-a", 1, "updated", "updated"); !errors.Is(err, ErrACBSessionBusy) {
		t.Fatalf("save during local clear=%v", err)
	}
	if err = s.FinishACBLogoutRevocation(ctx, j.ID, "CONFIRMED", "SESSION_REVOKED"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveACBCredentials(ctx, token, "owner-a", 1, "updated", "updated"); !errors.Is(err, ErrACBSessionBusy) {
		t.Fatalf("bank outcome bypassed local ack=%v", err)
	}
	if err = s.MarkACBLogoutLocalCleared(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveACBCredentials(ctx, token, "owner-a", 1, "updated", "updated"); err != nil {
		t.Fatal(err)
	}
}

func TestACBLogoutRejectsOldPollRefreshAndWrongClearState(t *testing.T) {
	s, ctx := monitoringStore(t)
	defer s.Close()
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	poll, err := s.StartPoll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`INSERT INTO sessions(connection_id,generation,envelope,key_id,updated_at) VALUES(?,?,?,'k1',?)`, c.ID, c.Generation, []byte("encrypted"), now()); err != nil {
		t.Fatal(err)
	}
	j := logoutAction(t, s, ctx, c)
	poll.Status = "SUCCEEDED"
	if err = s.FinishPoll(ctx, poll); !errors.Is(err, ErrGenerationFenceMismatch) {
		t.Fatalf("old successful poll committed=%v", err)
	}
	if err = s.RefreshSession(ctx, c.ID, c.Generation, []byte("stale"), "k1"); !IsSessionNotRefreshable(err) {
		t.Fatalf("old refresh committed=%v", err)
	}
	for _, state := range []string{"MONITORING", "AUTH_STARTING"} {
		if _, err = s.DB().Exec(`UPDATE connections SET state=? WHERE id=?`, state, c.ID); err != nil {
			t.Fatal(err)
		}
		if err = s.CheckACBLogoutFence(ctx, c.ID, j.FencedGeneration); !errors.Is(err, ErrACBLogoutFence) {
			t.Fatalf("state %s accepted logout clear=%v", state, err)
		}
		if err = s.BeginACBLogoutRevocation(ctx, j.ID); !errors.Is(err, ErrACBLogoutFence) {
			t.Fatalf("state %s accepted bank revoke=%v", state, err)
		}
	}
}

func TestACBLogoutBlocksMissingCredentialImportUntilLocalAck(t *testing.T) {
	s, ctx := monitoringStore(t)
	defer s.Close()
	c, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j := logoutAction(t, s, ctx, c)
	credentials := ACBCredentials{Username: "import-user", Password: " import-password ", AccountNumber: "1234567890"}
	if _, err = s.ImportACBCredentials(ctx, credentials); !errors.Is(err, ErrACBSessionBusy) {
		t.Fatalf("import while clearing=%v", err)
	}
	if err = s.FinishACBLogoutRevocation(ctx, j.ID, "UNCONFIRMED", "NO_SESSION_SNAPSHOT"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ImportACBCredentials(ctx, credentials); !errors.Is(err, ErrACBSessionBusy) {
		t.Fatalf("bank result bypassed local import fence=%v", err)
	}
}
