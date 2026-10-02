package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func automaticCompletionFixture(t *testing.T) (context.Context, *Store, AuthRecoveryEpisode, RecoveryRun) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c, err := s.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.TelegramAuthState(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().In(recoveryLocation)
	older := today.AddDate(0, 0, -9).Format("2006-01-02")
	_, err = s.DB().ExecContext(ctx, `INSERT INTO checkpoints(connection_id,coverage_from,coverage_to,updated_at) VALUES(?,?,?,?)`, c.ID, older, older, now())
	if err != nil {
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
	_, err = s.CompleteAuthSession(ctx, a.ID, []byte("verified synthetic envelope"))
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRecoveryRun(ctx, e.RecoveryRunID)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, s, e, r
}
func commitAutomaticRange(t *testing.T, ctx context.Context, s *Store, e AuthRecoveryEpisode, r RecoveryRun) {
	t.Helper()
	_, err := s.ClaimRecoveryRun(ctx, r.ID, e.ConnectionID, e.Generation)
	if err != nil {
		t.Fatal(err)
	}
	from, err := time.Parse("2006-01-02", r.RangeFrom)
	if err != nil {
		t.Fatal(err)
	}
	to, err := time.Parse("2006-01-02", r.RangeTo)
	if err != nil {
		t.Fatal(err)
	}
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		_, err := s.CommitRecoveryDay(ctx, RecoveryDayCommit{RunID: r.ID, ConnectionID: e.ConnectionID, Generation: e.Generation, Day: day.Format("2006-01-02"), NextDay: day.AddDate(0, 0, 1).Format("2006-01-02"), CoverageFrom: e.RequiredFrom})
		if err != nil {
			t.Fatal(err)
		}
	}
}
func TestAutomaticRecoveryCoverageCompletionGate(t *testing.T) {
	ctx, s, e, r := automaticCompletionFixture(t)
	from, _ := time.Parse("2006-01-02", r.RangeFrom)
	to, _ := time.Parse("2006-01-02", r.RangeTo)
	if int(to.Sub(from)/(24*time.Hour)) != 9 || e.State != "CATCHING_UP" {
		t.Fatal("long gap freeze lost")
	}
	auto, err := s.IsAutomaticRecoveryRun(ctx, r.ID)
	if err != nil || !auto {
		t.Fatal("auto linkage absent")
	}
	blocked, err := s.HasBlockingAuthRecovery(ctx, e.ConnectionID, e.Generation)
	if err != nil || !blocked {
		t.Fatal("gate missing at verified commit")
	}
	_, err = s.ClaimRecoveryRun(ctx, r.ID, e.ConnectionID, e.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRecoveryRunProgress(ctx, r.ID, e.ConnectionID, e.Generation, RecoveryRunStatusCompleted, "{}", "", ""); !errors.Is(err, ErrRecoveryPlanConflict) {
		t.Fatal("incomplete coverage released gate")
	}
	commitAutomaticRange(t, ctx, s, e, r)
	if _, err := s.UpdateRecoveryRunProgress(ctx, r.ID, e.ConnectionID, e.Generation, RecoveryRunStatusCompleted, "{}", "", ""); err != nil {
		t.Fatal(err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || e.State != "COMPLETED" || e.FinishedAt == "" {
		t.Fatal("worker did not close episode")
	}
	blocked, err = s.HasBlockingAuthRecovery(ctx, e.ConnectionID, e.Generation)
	if err != nil || blocked {
		t.Fatal("completed episode blocked realtime")
	}
}
func TestAutomaticRecoveryMidnightExtension(t *testing.T) {
	ctx, s, e, r := automaticCompletionFixture(t)
	commitAutomaticRange(t, ctx, s, e, r)
	r, err := s.GetRecoveryRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	to, _ := time.Parse("2006-01-02", r.RangeTo)
	future := to.AddDate(0, 0, 3).Add(12 * time.Hour)
	if err := s.withTx(ctx, func(tx *sql.Tx) error { return completeAutomaticRecoveryRunTx(ctx, tx, r, future) }); err != nil {
		t.Fatal(err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	extension, err := s.GetRecoveryRun(ctx, e.RecoveryRunID)
	if err != nil {
		t.Fatal(err)
	}
	if e.State != "CATCHING_UP" || extension.RangeFrom != to.AddDate(0, 0, 1).Format("2006-01-02") || extension.RangeTo != to.AddDate(0, 0, 3).Format("2006-01-02") {
		t.Fatal("midnight multi-day tail lost")
	}
	blocked, err := s.HasBlockingAuthRecovery(ctx, e.ConnectionID, e.Generation)
	if err != nil || !blocked {
		t.Fatal("tail gate released")
	}
	commitAutomaticRange(t, ctx, s, e, extension)
	extension, err = s.GetRecoveryRun(ctx, extension.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.withTx(ctx, func(tx *sql.Tx) error { return completeAutomaticRecoveryRunTx(ctx, tx, extension, future) }); err != nil {
		t.Fatal(err)
	}
	e, err = s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || e.State != "COMPLETED" {
		t.Fatal("tail did not complete")
	}
}
func TestAutomaticRecoveryInvalidCheckpointKeepsVerifiedGate(t *testing.T) {
	ctx, s, e, r := automaticCompletionFixture(t)
	if _, err := s.DB().ExecContext(ctx, `UPDATE checkpoints SET coverage_to='corrupt' WHERE connection_id=?`, e.ConnectionID); err != nil {
		t.Fatal(err)
	}
	if err := s.withTx(ctx, func(tx *sql.Tx) error {
		return completeAutomaticSessionTx(ctx, tx, e.AttemptID, e.ConnectionID, e.Generation, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	e, err := s.AuthRecoveryEpisode(ctx, e.ID)
	if err != nil || e.State != "MANUAL_REQUIRED" || e.ReasonCode != "INVALID_CHECKPOINT" {
		t.Fatal("corrupt checkpoint not manual")
	}
	r, err = s.GetRecoveryRun(ctx, r.ID)
	if err != nil || r.Status != RecoveryRunStatusFailed || r.ErrorCode != "INVALID_CHECKPOINT" {
		t.Fatal("corrupt intent runnable")
	}
	if err := s.RetryAuthRecoveryCatchup(ctx, e.ID, e.Generation); !errors.Is(err, ErrRecoveryCommitted) {
		t.Fatal("invalid checkpoint retry permitted")
	}
	blocked, err := s.HasBlockingAuthRecovery(ctx, e.ConnectionID, e.Generation)
	if err != nil || !blocked {
		t.Fatal("invalid checkpoint gate missing")
	}
}
