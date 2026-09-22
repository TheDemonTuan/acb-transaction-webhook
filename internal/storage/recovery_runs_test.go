package storage

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveryRunsDuplicateClaimAndGenerationFence(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	run, created, err := store.EnsureRecoveryRun(ctx, conn.ID, conn.Generation, "auth-required:1")
	if err != nil || !created {
		t.Fatalf("create recovery run: %+v created=%v err=%v", run, created, err)
	}
	duplicate, created, err := store.EnsureRecoveryRun(ctx, conn.ID, conn.Generation, "auth-required:1")
	if err != nil || created || duplicate.ID != run.ID {
		t.Fatalf("duplicate recovery run: %+v created=%v err=%v", duplicate, created, err)
	}
	other, created, err := store.EnsureRecoveryRun(ctx, conn.ID, conn.Generation, "auth-required:2")
	if err != nil || !created || other.ID == run.ID || other.EventKey != "auth-required:2" {
		t.Fatalf("distinct recovery event collapsed: %+v created=%v err=%v", other, created, err)
	}

	claimed, err := store.ClaimRecoveryRun(ctx, run.ID, conn.ID, conn.Generation)
	if err != nil || claimed.Status != RecoveryRunStatusRunning {
		t.Fatalf("claim recovery run: %+v err=%v", claimed, err)
	}
	claimedAgain, err := store.ClaimRecoveryRun(ctx, run.ID, conn.ID, conn.Generation)
	if err != nil || claimedAgain.ID != run.ID || claimedAgain.Status != RecoveryRunStatusRunning {
		t.Fatalf("idempotent claim: %+v err=%v", claimedAgain, err)
	}
	if _, err := store.UpdateRecoveryRunProgress(ctx, run.ID, conn.ID, conn.Generation, RecoveryRunStatusRunning, `{"step":"otp"}`, "", ""); err != nil {
		t.Fatal(err)
	}
	redacted, err := store.UpdateRecoveryRunProgress(ctx, run.ID, conn.ID, conn.Generation, RecoveryRunStatusRunning, `{}`, "AUTH token=secret", "password=secret cookie=secret")
	if err != nil || strings.Contains(redacted.ErrorCode, "secret") || strings.Contains(redacted.ErrorMessage, "secret") {
		t.Fatalf("recovery error was not sanitized: %+v err=%v", redacted, err)
	}
	for _, raw := range []string{
		`{"cookie":"cookie-secret","dse_sessionId":"session-secret"}`,
		`POST /history?token=query-secret dse_processorState=form-secret`,
		`<form><input name="rawForm" value="raw-secret"></form>`,
	} {
		redacted, err = store.UpdateRecoveryRunProgress(ctx, run.ID, conn.ID, conn.Generation, RecoveryRunStatusRunning, `{}`, "", raw)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(redacted.ErrorMessage, "secret") || strings.Contains(redacted.ErrorMessage, "rawForm") {
			t.Fatalf("sensitive recovery error leaked: %q", redacted.ErrorMessage)
		}
	}
	if _, err := store.ClaimRecoveryRun(ctx, other.ID, conn.ID, conn.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRecoveryRunProgress(ctx, other.ID, conn.ID, conn.Generation, RecoveryRunStatusCompleted, `{}`, "", ""); err != nil {
		t.Fatal(err)
	}

	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET generation=generation+1 WHERE id=?`, conn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateRecoveryRunProgress(ctx, run.ID, conn.ID, conn.Generation, RecoveryRunStatusCompleted, `{}`, "", ""); !errors.Is(err, ErrGenerationFenceMismatch) {
		t.Fatalf("expected generation fence error, got %v", err)
	}
	if _, err := store.ListOpenRecoveryRuns(ctx, conn.ID, conn.Generation); !errors.Is(err, ErrGenerationFenceMismatch) {
		t.Fatalf("expected list generation fence error, got %v", err)
	}
}

func TestCompleteAuthSessionCreatesRecoveryIntent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "auth-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAuthAttempt(ctx, "owner", 0)
	if err == nil {
		t.Fatal("expected invalid TTL")
	}
	attempt, err = store.StartAuthAttempt(ctx, "owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteAuthSession(ctx, attempt.ID, []byte("opaque-envelope"))
	if err != nil || completed.State != "MONITORING" {
		t.Fatalf("complete auth session: %+v err=%v", completed, err)
	}
	run, err := store.GetRecoveryRunByEvent(ctx, conn.ID, attempt.Generation, attempt.ID)
	if err != nil || run.Status != RecoveryRunStatusPending {
		t.Fatalf("auth recovery intent: %+v err=%v", run, err)
	}
}
