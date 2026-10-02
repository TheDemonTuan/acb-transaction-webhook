package storage

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func recoveryStore(t *testing.T) (*Store, context.Context, AuthRecoveryEpisode) {
	t.Helper()
	return recoveryStoreAt(t, filepath.Join(t.TempDir(), "recovery.db"))
}

func recoveryStoreAt(t *testing.T, path string) (*Store, context.Context, AuthRecoveryEpisode) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c, err := s.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	seedRecoveryCredentials(t, s, ctx, c.ID)
	e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, e
}

func seedRecoveryCredentials(t *testing.T, s *Store, ctx context.Context, connectionID string) {
	t.Helper()
	if _, err := s.DB().ExecContext(ctx, `INSERT OR IGNORE INTO acb_credentials(connection_id,revision,envelope,key_id,updated_at) VALUES(?,1,X'00','fixture',?)`, connectionID, now()); err != nil {
		t.Fatal(err)
	}
}

func consentRecovery(t *testing.T, s *Store, ctx context.Context, e AuthRecoveryEpisode) TelegramAuthAction {
	t.Helper()
	seedRecoveryCredentials(t, s, ctx, e.ConnectionID)
	a, err := s.CreateTelegramAuthAction(ctx, TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, EpisodeID: e.ID, ExpectedGeneration: e.Generation, Action: "LOGIN"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverTelegramAuthAction(ctx, a.ID, 44); err != nil {
		t.Fatal(err)
	}
	a, _, err = s.ConsumeTelegramAuthAction(ctx, a.ID, 123, 456, 789, 44, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRecoveryEpisodeBudgetSurvivesGenerationChanges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recovery.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING' WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	poll, err := s.StartPoll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	poll.Status = "AUTH_REQUIRED"
	poll.AuthConfirmed = true
	if err := s.FinishPoll(ctx, poll); err != nil {
		t.Fatal(err)
	}
	c, err = s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		consentRecovery(t, s, ctx, e)
		a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if a.OwnerSubject != RecoveryOwner {
			t.Fatalf("wrong automatic owner: %q", a.OwnerSubject)
		}
		if err := s.FinishRecoveryAuthAttempt(ctx, e.ID, a.Generation, "FAILED", "RETRY_WAIT", "BROWSER_UNAVAILABLE", time.Time{}); err != nil {
			t.Fatal(err)
		}
		e, err = s.AuthRecoveryEpisode(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		c, err = s.Connection(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if e.Generation != c.Generation || e.AttemptCount != i || e.BudgetStartCount != 0 {
			t.Fatalf("lost episode budget: %+v connection=%+v", e, c)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); !errors.Is(err, ErrRecoveryBudgetExhausted) {
		t.Fatalf("fourth attempt admitted: %v", err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e.State != "MANUAL_REQUIRED" {
		t.Fatalf("exhausted circuit reopened: %+v", e)
	}
	if err := s.RearmAuthRecovery(ctx, e.ID, e.Generation); err != nil {
		t.Fatal(err)
	}
	consentRecovery(t, s, ctx, e)
	a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e.AttemptCount != 4 || e.BudgetStartCount != 3 {
		t.Fatalf("rearm reset ordinal: %+v", e)
	}
	if err := s.FinishRecoveryAuthAttempt(ctx, e.ID, a.Generation, "CANCELLED", "CANCELLED", "OPERATOR_CANCEL", time.Time{}); err != nil {
		t.Fatal(err)
	}
	c, err = s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.ID != e.ID || cancelled.State != "CANCELLED" || cancelled.Generation != c.Generation || cancelled.FinishedAt == "" {
		t.Fatalf("cancelled trigger recreated: %+v", cancelled)
	}
	if _, err := s.StartRecoveryAuthAttempt(ctx, cancelled.ID, c.Generation, time.Minute); !errors.Is(err, ErrRecoverySuperseded) {
		t.Fatalf("cancelled episode restarted: %v", err)
	}
}

func TestRecoveryAdmissionSerializesWithManualAndDeploy(t *testing.T) {
	for _, winner := range []string{"race", "deploy", "manual"} {
		t.Run(winner, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "recovery.db")
			s, ctx, e := recoveryStoreAt(t, path)
			consentRecovery(t, s, ctx, e)
			other, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			if winner == "deploy" {
				if _, err := other.AcquireMutationGate(ctx, "deploy", time.Minute, "test"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); !errors.Is(err, ErrMutationGateLocked) {
					t.Fatalf("auto crossed deploy gate: %v", err)
				}
				if _, err := s.StartAuthAttempt(ctx, "operator", time.Minute); !errors.Is(err, ErrMutationGateLocked) {
					t.Fatalf("manual crossed deploy gate: %v", err)
				}
				return
			}
			if winner == "manual" {
				manual, err := other.StartAuthAttempt(ctx, "operator", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); err == nil {
					t.Fatal("automatic attempt stole manual ownership")
				}
				current, err := s.AuthAttemptStatusForOwner(ctx, manual.ID, "operator")
				if err != nil || current.Status != "STARTING" {
					t.Fatalf("manual attempt mutated: %+v %v", current, err)
				}
				return
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			var autoErr, manualErr, gateErr error
			wg.Add(3)
			go func() {
				defer wg.Done()
				<-start
				_, autoErr = s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
			}()
			go func() { defer wg.Done(); <-start; _, manualErr = other.StartAuthAttempt(ctx, "operator", time.Minute) }()
			go func() {
				defer wg.Done()
				<-start
				_, gateErr = other.AcquireMutationGate(ctx, "deploy", time.Minute, "test")
			}()
			close(start)
			wg.Wait()
			var count int
			if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_attempts WHERE status IN ('STARTING','IN_PROGRESS','EXPORTING','VERIFYING')`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count > 1 || autoErr == nil && manualErr == nil {
				t.Fatalf("concurrent admission: count=%d auto=%v manual=%v", count, autoErr, manualErr)
			}
			if gateErr == nil && (count != 0 || autoErr == nil || manualErr == nil) {
				t.Fatalf("gate and authentication both won: count=%d auto=%v manual=%v", count, autoErr, manualErr)
			}
			if count == 0 && gateErr != nil {
				t.Fatalf("no admission winner: auto=%v manual=%v gate=%v", autoErr, manualErr, gateErr)
			}
		})
	}
}

func seedRecoveryInputs(t *testing.T, s *Store, ctx context.Context, e AuthRecoveryEpisode) {
	t.Helper()
	_, err := s.DB().ExecContext(ctx, `INSERT INTO auth_challenges(id,episode_id,connection_id,generation,attempt_id,browser_revision,kind,status,chat_id,expires_at,created_at) VALUES(?,?,?,?,?,'revision','OTP','PENDING',42,?,?)`, "challenge-"+e.ID, e.ID, e.ConnectionID, e.Generation, e.AttemptID, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB().ExecContext(ctx, `INSERT INTO telegram_auth_actions(id,bot_id,chat_id,user_id,episode_id,expected_generation,action,status,expires_at,created_at) VALUES(?,1,42,43,?,?,'CANCEL','PENDING',?,?)`, "action-"+e.ID, e.ID, e.Generation, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), now())
	if err != nil {
		t.Fatal(err)
	}
}

func assertRecoveryInputsInvalidated(t *testing.T, s *Store, ctx context.Context, e AuthRecoveryEpisode) {
	t.Helper()
	for _, table := range []string{"auth_challenges", "telegram_auth_actions"} {
		var status string
		prefix := "challenge-"
		if table == "telegram_auth_actions" {
			prefix = "action-"
		}
		if err := s.DB().QueryRowContext(ctx, `SELECT status FROM `+table+` WHERE id=?`, prefix+e.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "INVALIDATED" {
			t.Fatalf("%s remained reusable: %s", table, status)
		}
	}
}

func TestRecoveryExpiryAndConfirmedPollPreserveEpisode(t *testing.T) {
	for _, loss := range []string{"expiry", "poll", "local", "decrypt"} {
		t.Run(loss, func(t *testing.T) {
			s, ctx, e := recoveryStore(t)
			consentRecovery(t, s, ctx, e)
			a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			e, err = s.AuthRecoveryEpisode(ctx, e.ID)
			if err != nil {
				t.Fatal(err)
			}
			seedRecoveryInputs(t, s, ctx, e)
			if loss == "expiry" {
				if _, err := s.DB().ExecContext(ctx, `UPDATE auth_attempts SET expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), a.ID); err != nil {
					t.Fatal(err)
				}
				count, err := s.ExpireStaleAuthAttempts(ctx)
				if err != nil || count != 1 {
					t.Fatalf("expiry count=%d err=%v", count, err)
				}
			} else {
				if _, err := s.DB().ExecContext(ctx, `UPDATE auth_attempts SET status='VERIFIED',finished_at=? WHERE id=?`, now(), a.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING' WHERE id=?`, e.ConnectionID); err != nil {
					t.Fatal(err)
				}
				runID := "run-" + e.ID
				if _, err := s.DB().ExecContext(ctx, `INSERT INTO recovery_runs(id,connection_id,generation,event_key,status,created_at,updated_at) VALUES(?,?,?,?,'RUNNING',?,?)`, runID, e.ConnectionID, e.Generation, e.AttemptID, now(), now()); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().ExecContext(ctx, `UPDATE auth_recovery_episodes SET state='CATCHING_UP',recovery_run_id=?,required_from='2026-09-20',required_to='2026-10-02' WHERE id=?`, runID, e.ID); err != nil {
					t.Fatal(err)
				}
				blocked, err := s.HasBlockingAuthRecovery(ctx, e.ConnectionID, e.Generation)
				if err != nil || !blocked {
					t.Fatalf("incomplete automatic run did not gate monitoring: %v %v", blocked, err)
				}
				if err := s.TransitionAuthRecovery(ctx, e.ID, e.Generation, "CATCHING_UP", "COMPLETED", ""); err == nil {
					t.Fatal("coordinator released worker-owned coverage gate")
				}
				if err := s.TransitionAuthRecovery(ctx, e.ID, e.Generation, "CATCHING_UP", "CANCELLED", ""); !errors.Is(err, ErrRecoveryNotReady) {
					t.Fatalf("committed catch-up was cancellable: %v", err)
				}
				if loss == "poll" {
					poll, err := s.StartPoll(ctx)
					if err != nil {
						t.Fatal(err)
					}
					poll.Status = "AUTH_REQUIRED"
					if err := s.FinishPoll(ctx, poll); err == nil {
						t.Fatal("unconfirmed poll invalidated recovery")
					}
					unchanged, err := s.AuthRecoveryEpisode(ctx, e.ID)
					if err != nil || unchanged.Generation != e.Generation {
						t.Fatalf("unconfirmed poll mutation: %+v %v", unchanged, err)
					}
					poll.AuthConfirmed = true
					if err := s.FinishPoll(ctx, poll); err != nil {
						t.Fatal(err)
					}
				} else {
					reason := "SESSION_MISSING"
					if loss == "decrypt" {
						reason = "SESSION_DECRYPT_FAILED"
					}
					if err := s.RequireSessionRecovery(ctx, e.ConnectionID, a.Generation, reason); err != nil {
						t.Fatal(err)
					}
					if err := s.RequireSessionRecovery(ctx, e.ConnectionID, a.Generation, reason); !errors.Is(err, ErrGenerationFenceMismatch) {
						t.Fatalf("stale local loss replayed: %v", err)
					}
				}
			}
			updated, err := s.AuthRecoveryEpisode(ctx, e.ID)
			if err != nil {
				t.Fatal(err)
			}
			c, err := s.Connection(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Generation != a.Generation+1 || updated.Generation != c.Generation || updated.AttemptCount != 1 || updated.BudgetStartCount != 0 || c.State != "AUTH_REQUIRED" {
				t.Fatalf("internal loss reset/fenced episode: %+v %+v", updated, c)
			}
			if loss == "decrypt" && updated.State != "MANUAL_REQUIRED" {
				t.Fatalf("decryption failure auto-retried: %+v", updated)
			}
			if loss != "expiry" && (updated.RequiredFrom != "2026-09-20" || updated.RequiredTo != "2026-10-02") {
				t.Fatalf("required coverage lost: %+v", updated)
			}
			if updated.RecoveryRunID != "" {
				t.Fatalf("old-generation run retained after auth loss: %+v", updated)
			}
			if updated.ConsentActionID != "" || updated.ConsentConsumedAt != "" || updated.ConsentExpiresAt != "" {
				t.Fatalf("auth loss retained login consent: %+v", updated)
			}
			assertRecoveryInputsInvalidated(t, s, ctx, e)
		})
	}
}

func TestRecoveryReservationsAndExternalSupersession(t *testing.T) {
	s, ctx, e := recoveryStore(t)
	consentRecovery(t, s, ctx, e)
	a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionAuthRecovery(ctx, e.ID, a.Generation, "STARTING", "LOGIN", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimRecoveryAI(ctx, e.ID, a.Generation, "captcha-A"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimRecoveryAI(ctx, e.ID, a.Generation, "captcha-A"); !errors.Is(err, ErrRecoveryAIClaimed) {
		t.Fatalf("AI retried: %v", err)
	}
	if err := s.ReserveRecoverySubmission(ctx, e.ID, a.Generation, "CAPTCHA_TEXT", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRecoveryLogin(ctx, e.ID, a.Generation); !errors.Is(err, ErrRecoveryCooldown) {
		t.Fatalf("duplicate login allowed: %v", err)
	}
	for range 2 {
		if err := s.ReserveRecoverySubmission(ctx, e.ID, a.Generation, "CAPTCHA_TEXT", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReserveRecoverySubmission(ctx, e.ID, a.Generation, "CAPTCHA_TEXT", false); !errors.Is(err, ErrRecoveryBudgetExhausted) {
		t.Fatalf("CAPTCHA cap bypassed: %v", err)
	}
	if err := s.TransitionAuthRecovery(ctx, e.ID, a.Generation, "LOGIN", "WAITING_OTP", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveRecoverySubmission(ctx, e.ID, a.Generation, "OTP", false); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveRecoverySubmission(ctx, e.ID, a.Generation, "OTP", false); !errors.Is(err, ErrRecoveryBudgetExhausted) {
		t.Fatalf("OTP replayed: %v", err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedRecoveryInputs(t, s, ctx, e)
	if _, err := s.DB().ExecContext(ctx, `UPDATE connections SET generation=generation+7,config_revision=config_revision+1,state='AUTH_REQUIRED' WHERE id=?`, e.ConnectionID); err != nil {
		t.Fatal(err)
	}
	before, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveRecoverySubmission(ctx, e.ID, a.Generation, "OTP", false); !errors.Is(err, ErrRecoverySuperseded) {
		t.Fatalf("stale response accepted: %v", err)
	}
	if err := s.FinishRecoveryAuthAttempt(ctx, e.ID, a.Generation, "FAILED", "RETRY_WAIT", "STALE", time.Time{}); !errors.Is(err, ErrRecoverySuperseded) {
		t.Fatalf("stale attempt finished current generation: %v", err)
	}
	if err := s.SupersedeAuthRecovery(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	after, err := s.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Generation != after.Generation || before.State != after.State {
		t.Fatalf("supersession mutated new connection: %+v %+v", before, after)
	}
	assertRecoveryInputsInvalidated(t, s, ctx, e)
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || e.State != "SUPERSEDED" || e.FinishedAt == "" {
		t.Fatalf("supersession failed: %+v %v", e, err)
	}
}

func TestRecoveryPauseCooldownExpiryAndOnboarding(t *testing.T) {
	s, ctx, e := recoveryStore(t)
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO telegram_auth_state(bot_id,paused,updated_at) VALUES(1,1,?)`, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); !errors.Is(err, ErrRecoveryPaused) {
		t.Fatalf("pause bypassed: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE telegram_auth_state SET paused=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE connections SET state='UNCONFIGURED'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); !errors.Is(err, ErrRecoveryConsentRequired) {
		t.Fatalf("watcher onboarded without button: %v", err)
	}
	if err := s.RearmAuthRecovery(ctx, e.ID, e.Generation); err != nil {
		t.Fatal(err)
	}
	consentRecovery(t, s, ctx, e)
	a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionAuthRecovery(ctx, e.ID, a.Generation, "STARTING", "LOGIN", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRecoveryLogin(ctx, e.ID, a.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE auth_attempts SET expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimRecoveryAI(ctx, e.ID, a.Generation, "captcha-A"); !errors.Is(err, ErrChallengeExpired) {
		t.Fatalf("expired attempt used AI: %v", err)
	}
	if err := s.TransitionAuthRecovery(ctx, e.ID, a.Generation, "LOGIN", "WAITING_CAPTCHA", ""); !errors.Is(err, ErrChallengeExpired) {
		t.Fatalf("expired attempt progressed: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE telegram_auth_state SET paused=1`); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRecoveryAuthAttempt(ctx, e.ID, a.Generation, "CANCELLED", "CANCELLED", "OPERATOR_PAUSE", time.Time{}); err != nil {
		t.Fatalf("paused expired cleanup blocked: %v", err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RearmAuthRecovery(ctx, e.ID, e.Generation); !errors.Is(err, ErrRecoveryCooldown) {
		t.Fatalf("operator bypassed login cooldown: %v", err)
	}
}

func TestRecoveryPriorOperationalEvidenceSurvivesMissingSession(t *testing.T) {
	s, ctx, e := recoveryStore(t)
	known, err := s.HasPriorOperationalEvidence(ctx, e.ConnectionID)
	if err != nil || known {
		t.Fatalf("initial connection treated as operational: %v %v", known, err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO auth_attempts(id,connection_id,generation,owner_subject,status,expires_at,created_at,finished_at) VALUES('previous',?,?,'operator','VERIFIED',?,?,?)`, e.ConnectionID, e.Generation, now(), now(), now()); err != nil {
		t.Fatal(err)
	}
	known, err = s.HasPriorOperationalEvidence(ctx, e.ConnectionID)
	if err != nil || !known {
		t.Fatalf("missing session prevented operational recovery: %v %v", known, err)
	}
}

func TestRecoveryAdmissionRespectsRetryDeadline(t *testing.T) {
	s, ctx, e := recoveryStore(t)
	consentRecovery(t, s, ctx, e)
	a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRecoveryAuthAttempt(ctx, e.ID, a.Generation, "FAILED", "RETRY_WAIT", "BROWSER_UNAVAILABLE", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); !errors.Is(err, ErrRecoveryNotReady) {
		t.Fatalf("retry deadline bypassed: %v", err)
	}
	unchanged, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.AttemptCount != 1 || unchanged.Generation != e.Generation {
		t.Fatalf("early retry spent budget: %+v", unchanged)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE auth_recovery_episodes SET next_attempt_at=? WHERE id=?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), e.ID); err != nil {
		t.Fatal(err)
	}
	consentRecovery(t, s, ctx, e)
	next, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == a.ID || next.Generation != e.Generation+1 {
		t.Fatalf("due retry did not advance attempt: %+v", next)
	}
}

func TestRecoveryAIClaimsAreUniqueDurableAndAttemptScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-claims.db")
	s, ctx, e := recoveryStoreAt(t, path)
	consentRecovery(t, s, ctx, e)
	a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionAuthRecovery(ctx, e.ID, a.Generation, "STARTING", "LOGIN", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimRecoveryAI(ctx, e.ID, a.Generation, ""); !errors.Is(err, ErrChallengeMismatch) {
		t.Fatalf("empty revision: %v", err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- s.ClaimRecoveryAI(ctx, e.ID, a.Generation, "A") }()
	}
	successes, duplicates := 0, 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			successes++
		case errors.Is(err, ErrRecoveryAIClaimed):
			duplicates++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("concurrent claims: success=%d duplicate=%d", successes, duplicates)
	}
	if err := s.ClaimRecoveryAI(ctx, e.ID, a.Generation, "B"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.ClaimRecoveryAI(ctx, e.ID, a.Generation, "A"); !errors.Is(err, ErrRecoveryAIClaimed) {
		t.Fatalf("A-B-A replay after restart: %v", err)
	}
	if err := s.ClaimRecoveryAI(ctx, e.ID, a.Generation, "C"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimRecoveryAI(ctx, e.ID, a.Generation, "D"); !errors.Is(err, ErrRecoveryBudgetExhausted) {
		t.Fatalf("fourth AI request: %v", err)
	}
	current, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	var claims int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM auth_recovery_ai_claims WHERE attempt_id=?`, a.ID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if current.AIUsed != 3 || claims != 3 {
		t.Fatalf("claims and budget diverged: used=%d claims=%d", current.AIUsed, claims)
	}
	if err := s.FinishRecoveryAuthAttempt(ctx, e.ID, a.Generation, "FAILED", "WAIT_OPERATOR", "CAPTCHA_BUDGET_EXHAUSTED", time.Time{}); err != nil {
		t.Fatal(err)
	}
	current, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	consentRecovery(t, s, ctx, current)
	next, err := s.StartRecoveryAuthAttempt(ctx, e.ID, current.Generation, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionAuthRecovery(ctx, e.ID, next.Generation, "STARTING", "LOGIN", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimRecoveryAI(ctx, e.ID, next.Generation, "A"); err != nil {
		t.Fatalf("new consent attempt retained old claims: %v", err)
	}
	current, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || current.AIUsed != 1 {
		t.Fatalf("new attempt AI budget: %+v %v", current, err)
	}
}

func TestRecoveryWaitOperatorExhaustionAdmitsFreshConsentAndPreservesCooldown(t *testing.T) {
	for _, legacyWait := range []bool{false, true} {
		name := "current"
		if legacyWait {
			name = "previous-wait-operator"
		}
		t.Run(name, func(t *testing.T) {
			s, ctx, e := recoveryStore(t)
			for i := 1; i <= 3; i++ {
				consentRecovery(t, s, ctx, e)
				a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.FinishRecoveryAuthAttempt(ctx, e.ID, a.Generation, "FAILED", "WAIT_OPERATOR", "BROWSER_UNAVAILABLE", time.Time{}); err != nil {
					t.Fatal(err)
				}
				e, err = s.AuthRecoveryEpisode(ctx, e.ID)
				if err != nil {
					t.Fatal(err)
				}
				expected := "WAIT_OPERATOR"
				if i == 3 {
					expected = "MANUAL_REQUIRED"
				}
				if e.State != expected || e.AttemptCount != i || e.BudgetStartCount != 0 || e.ConsentActionID != "" {
					t.Fatalf("failure %d left unexpected episode: %+v", i, e)
				}
			}
			if legacyWait {
				if _, err := s.DB().Exec(`UPDATE auth_recovery_episodes SET state='WAIT_OPERATOR',reason_code='BROWSER_UNAVAILABLE' WHERE id=?`, e.ID); err != nil {
					t.Fatal(err)
				}
				var err error
				e, err = s.AuthRecoveryEpisode(ctx, e.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.DB().Exec(`UPDATE auth_recovery_episodes SET last_login_at=? WHERE id=?`, now(), e.ID); err != nil {
				t.Fatal(err)
			}
			a, err := s.CreateTelegramAuthAction(ctx, TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, EpisodeID: e.ID, ExpectedGeneration: e.Generation, Action: "LOGIN"})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.DeliverTelegramAuthAction(ctx, a.ID, 45); err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.ConsumeTelegramAuthAction(ctx, a.ID, 123, 456, 789, 45, time.Now()); !errors.Is(err, ErrRecoveryCooldown) {
				t.Fatalf("new consent bypassed login cooldown: %v", err)
			}
			earlier := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano)
			if _, err = s.DB().Exec(`UPDATE auth_recovery_episodes SET last_login_at=? WHERE id=?`, earlier, e.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.ConsumeTelegramAuthAction(ctx, a.ID, 123, 456, 789, 45, time.Now()); err != nil {
				t.Fatal(err)
			}
			attempt, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
			if err != nil {
				t.Fatalf("fresh consent stuck behind exhausted budget: %v", err)
			}
			current, err := s.AuthRecoveryEpisode(ctx, e.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.AttemptCount != 4 || current.BudgetStartCount != 3 || current.AttemptID != attempt.ID || current.ConsentConsumedAt == "" || current.LastLoginAt != earlier {
				t.Fatalf("fresh consent lost budget/cooldown: %+v", current)
			}
			if _, err = s.StartRecoveryAuthAttempt(ctx, e.ID, attempt.Generation, time.Minute); err == nil {
				t.Fatal("one fresh click admitted a second attempt")
			}
		})
	}
}
