package storage

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChallengeConsumeAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TelegramAuthState(ctx, 123); err != nil {
		t.Fatal(err)
	}
	e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
	if err != nil {
		t.Fatal(err)
	}
	consentRecovery(t, s, ctx, e)
	a, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := s.CreateAuthChallenge(ctx, AuthChallenge{EpisodeID: e.ID, ConnectionID: c.ID, Generation: a.Generation, AttemptID: a.ID, BrowserRevision: "revision-one", Kind: "OTP", ChatID: 456, ExpiresAt: time.Now().UTC().Add(40 * time.Second).Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthChallengeForPrompt(ctx, 456, 789); !errors.Is(err, ErrNotFound) {
		t.Fatal("undelivered prompt accepted")
	}
	if err := s.DeliverAuthChallenge(ctx, challenge.ID, 456, 789); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		generation   int64
		revision     string
		chat, prompt int64
		at           time.Time
		want         error
	}{
		{a.Generation - 1, "revision-one", 456, 789, time.Now(), ErrChallengeMismatch},
		{a.Generation, "revision-two", 456, 789, time.Now(), ErrChallengeMismatch},
		{a.Generation, "revision-one", 999, 789, time.Now(), ErrChallengeMismatch},
		{a.Generation, "revision-one", 456, 780, time.Now(), ErrChallengeMismatch},
		{a.Generation, "revision-one", 456, 789, mustChallengeTime(t, challenge.ExpiresAt), ErrChallengeExpired},
	} {
		if _, err := s.ConsumeAuthChallenge(ctx, challenge.ID, tc.generation, tc.revision, tc.chat, tc.prompt, tc.at); !errors.Is(err, tc.want) {
			t.Fatalf("rejected correlation err=%v want=%v", err, tc.want)
		}
	}
	other, err := OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var successes atomic.Int32
	var wg sync.WaitGroup
	for _, store := range []*Store{s, other} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			if _, err := store.ConsumeAuthChallenge(ctx, challenge.ID, a.Generation, "revision-one", 456, 789, time.Now()); err == nil {
				successes.Add(1)
			}
		}(store)
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful claims=%d", successes.Load())
	}
	restarted, err := OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := restarted.ConsumeAuthChallenge(ctx, challenge.ID, a.Generation, "revision-one", 456, 789, time.Now()); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("consuming challenge replayed: %v", err)
	}
	persisted, err := restarted.ActiveAuthChallenge(ctx, a.ID)
	if err != nil || persisted.Status != "CONSUMING" {
		t.Fatalf("restart status=%s err=%v", persisted.Status, err)
	}
	current, err := restarted.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || current.OTPSubmissions != 1 {
		t.Fatalf("OTP count=%d err=%v", current.OTPSubmissions, err)
	}
	if err := restarted.FinishAuthChallenge(ctx, challenge.ID, "INVALIDATED"); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ConsumeAuthChallenge(ctx, challenge.ID, a.Generation, "revision-one", 456, 789, time.Now()); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatal("invalidated challenge replayed")
	}
}
func mustChallengeTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func challengeDeliveryFixture(t *testing.T) (*Store, AuthRecoveryEpisode, AuthChallenge) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c, err := s.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TelegramAuthState(ctx, 123); err != nil {
		t.Fatal(err)
	}
	e, err := s.EnsureAuthRecoveryEpisode(ctx, c.ID, c.Generation)
	if err != nil {
		t.Fatal(err)
	}
	consentRecovery(t, s, ctx, e)
	if _, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute); err != nil {
		t.Fatal(err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := s.CreateAuthChallenge(ctx, AuthChallenge{EpisodeID: e.ID, ConnectionID: e.ConnectionID, Generation: e.Generation, AttemptID: e.AttemptID, BrowserRevision: "revision-one", Kind: "OTP", ChatID: 456, ExpiresAt: time.Now().UTC().Add(40 * time.Second).Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	return s, e, challenge
}

func TestChallengeStartupRecoveryRetainsBoundPrompt(t *testing.T) {
	s, e, unbound := challengeDeliveryFixture(t)
	ctx := context.Background()
	if err := s.RecoverAuthChallengeDelivery(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.DB().QueryRow(`SELECT status FROM auth_challenges WHERE id=?`, unbound.ID).Scan(&status); err != nil || status != "INVALIDATED" {
		t.Fatalf("startup status=%s err=%v", status, err)
	}
	bound, err := s.CreateAuthChallenge(ctx, AuthChallenge{EpisodeID: e.ID, ConnectionID: e.ConnectionID, Generation: e.Generation, AttemptID: e.AttemptID, BrowserRevision: "revision-one", Kind: "OTP", ChatID: 456, ExpiresAt: unbound.ExpiresAt})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverAuthChallenge(ctx, bound.ID, 456, 789); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverAuthChallengeDelivery(ctx); err != nil {
		t.Fatal(err)
	}
	persisted, err := s.AuthChallengeForPrompt(ctx, 456, 789)
	if err != nil || persisted.Status != "PENDING" || persisted.ID != bound.ID || persisted.ExpiresAt != unbound.ExpiresAt {
		t.Fatalf("bound prompt not adopted: %+v err=%v", persisted, err)
	}
	if err := s.ValidateAuthChallenge(ctx, persisted, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestChallengeCleanupRetainsReplayCorrelation(t *testing.T) {
	s, _, c := challengeDeliveryFixture(t)
	ctx := context.Background()
	if err := s.DeliverAuthChallenge(ctx, c.ID, 456, 789); err != nil {
		t.Fatal(err)
	}
	c, err := s.AuthChallengeForPrompt(ctx, 456, 789)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAuthChallengePromptDeleted(ctx, c); !errors.Is(err, ErrChallengeMismatch) {
		t.Fatalf("pending prompt marked deleted: %v", err)
	}
	if err := s.FinishAuthChallenge(ctx, c.ID, "CANCELLED"); err != nil {
		t.Fatal(err)
	}
	cleanup, err := s.AuthChallengesForDelivery(ctx)
	if err != nil || len(cleanup) != 1 || cleanup[0].ID != c.ID || cleanup[0].Status != "CANCELLED" {
		t.Fatalf("cancelled prompt missing from cleanup: %+v err=%v", cleanup, err)
	}
	if err := s.MarkAuthChallengePromptDeleted(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAuthChallengePromptDeleted(ctx, c); err != nil {
		t.Fatal(err)
	}
	cleanup, err = s.AuthChallengesForDelivery(ctx)
	if err != nil || len(cleanup) != 0 {
		t.Fatalf("deleted prompt still scheduled: %+v err=%v", cleanup, err)
	}
	persisted, err := s.AuthChallengeForPrompt(ctx, 456, 789)
	if err != nil || persisted.PromptDeletedAt == "" || persisted.Status != "CANCELLED" {
		t.Fatalf("cleanup lost durable correlation: %+v err=%v", persisted, err)
	}
	if _, err := s.ConsumeAuthChallenge(ctx, c.ID, c.Generation, c.BrowserRevision, c.ChatID, c.PromptMessageID, time.Now()); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("deleted prompt replay=%v", err)
	}
}

func TestChallengeDeliveryRechecksConfigurationFence(t *testing.T) {
	s, e, c := challengeDeliveryFixture(t)
	ctx := context.Background()
	if _, err := s.DB().Exec(`UPDATE connections SET config_revision=config_revision+1 WHERE id=?`, e.ConnectionID); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateAuthChallenge(ctx, c, time.Now()); !errors.Is(err, ErrChallengeMismatch) {
		t.Fatalf("validation crossed config fence: %v", err)
	}
	if err := s.DeliverAuthChallenge(ctx, c.ID, c.ChatID, 789); !errors.Is(err, ErrChallengeMismatch) {
		t.Fatalf("binding crossed config fence: %v", err)
	}
	if _, err := s.AuthChallengeForPrompt(ctx, c.ChatID, 789); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale prompt bound: %v", err)
	}
}

func TestChallengeRecoveryAndValidationRespectDeploymentGate(t *testing.T) {
	s, _, c := challengeDeliveryFixture(t)
	ctx := context.Background()
	if _, err := s.DB().Exec(`UPDATE deployment_control SET gate_state='LOCKED',owner='restore',lease_token='fixture',lease_expires_at=?,fence_generation=fence_generation+1 WHERE id='singleton'`, time.Now().Add(time.Minute).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateAuthChallenge(ctx, c, time.Now()); !errors.Is(err, ErrMutationGateLocked) {
		t.Fatalf("validation bypassed gate: %v", err)
	}
	if err := s.RecoverAuthChallengeDelivery(ctx); !errors.Is(err, ErrMutationGateLocked) {
		t.Fatalf("startup mutation bypassed gate: %v", err)
	}
	persisted, err := s.ActiveAuthChallenge(ctx, c.AttemptID)
	if err != nil || persisted.Status != "DELIVERING" {
		t.Fatalf("locked recovery mutated challenge: %+v err=%v", persisted, err)
	}
}
