package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTelegramAuthActionDurableDisposition(t *testing.T) {
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
	attempt, err := s.StartRecoveryAuthAttempt(ctx, e.ID, e.Generation, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	action, err := s.CreateTelegramAuthAction(ctx, TelegramAuthAction{BotID: 123, ChatID: 456, UserID: 789, EpisodeID: e.ID, ExpectedGeneration: e.Generation, Action: "PAUSE"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverTelegramAuthAction(ctx, action.ID, 12); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConsumeTelegramAuthAction(ctx, action.ID, 123, 456, 999, 12, time.Now()); !errors.Is(err, ErrChallengeMismatch) {
		t.Fatal("wrong user accepted")
	}
	if _, _, err := s.ConsumeTelegramAuthAction(ctx, action.ID, 123, 456, 789, 13, time.Now()); !errors.Is(err, ErrChallengeMismatch) {
		t.Fatal("wrong message accepted")
	}
	expiry, err := time.Parse(time.RFC3339Nano, action.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConsumeTelegramAuthAction(ctx, action.ID, 123, 456, 789, 12, expiry); !errors.Is(err, ErrChallengeExpired) {
		t.Fatal("expiry boundary accepted")
	}
	if _, _, err := s.ConsumeTelegramAuthAction(ctx, action.ID, 123, 456, 789, 12, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceTelegramAuthOffset(ctx, 123, 99); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	state, err := reopened.TelegramAuthState(ctx, 123)
	if err != nil || !state.Paused || state.NextUpdateID != 99 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	current, err := reopened.Connection(ctx)
	if err != nil || current.Generation != attempt.Generation+1 {
		t.Fatalf("generation=%d err=%v", current.Generation, err)
	}
	if _, _, err := reopened.ConsumeTelegramAuthAction(ctx, action.ID, 123, 456, 789, 12, time.Now()); !errors.Is(err, ErrChallengeConsumed) {
		t.Fatalf("callback replayed: %v", err)
	}
	unchanged, err := reopened.Connection(ctx)
	if err != nil || unchanged.Generation != current.Generation {
		t.Fatal("replay changed generation")
	}
	episode, err := reopened.EnsureAuthRecoveryEpisode(ctx, current.ID, current.Generation)
	if err != nil || episode.ID != e.ID || episode.State != "CANCELLED" {
		t.Fatalf("cancel suppression state=%s err=%v", episode.State, err)
	}
	if _, err := reopened.ApplyTelegramAuthOperation(ctx, 123, e.ID, current.Generation, "RESUME"); err != nil {
		t.Fatal(err)
	}
	episode, err = reopened.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || episode.State != "CANCELLED" || episode.AttemptCount != 1 {
		t.Fatal("resume reset circuit")
	}
	if err := reopened.AdvanceTelegramAuthOffset(ctx, 123, 10); err != nil {
		t.Fatal(err)
	}
	state, err = reopened.TelegramAuthState(ctx, 123)
	if err != nil || state.NextUpdateID != 99 {
		t.Fatal("offset regressed")
	}
}
