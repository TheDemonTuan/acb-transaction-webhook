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
