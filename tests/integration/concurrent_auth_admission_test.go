package integration_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// TestConcurrentAuthAndDeployAdmission proves Gates 5 and 8:
// 1. Active auth attempt blocks deploy mutation gate admission (Gate 8).
// 2. While deploy mutation gate is LOCKED, application mutations are blocked.
// 3. Concurrent auth attempts are serialized / fenced.
func TestConcurrentAuthAndDeployAdmission(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "gateway.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	defer store.Close()

	// Setup connection
	conn, err := store.ConfigureConnection(ctx, "***5555")
	if err != nil {
		t.Fatalf("configure connection: %v", err)
	}

	// 1. Initially no active auth attempts: deploy mutation gate can be acquired
	gate, err := store.AcquireMutationGate(ctx, "deploy-script", 1*time.Minute, "routine deploy")
	if err != nil {
		t.Fatalf("expected mutation gate acquisition to succeed: %v", err)
	}
	if gate.GateState != "LOCKED" {
		t.Fatalf("expected gate to be LOCKED, got %s", gate.GateState)
	}

	// While gate is locked, application mutations are forbidden (fail-closed)
	if err := store.CheckMutationAllowed(ctx); err == nil {
		t.Fatal("expected CheckMutationAllowed to fail while mutation gate is LOCKED")
	}

	// Release mutation gate
	if err := store.ReleaseMutationGate(ctx, "deploy-script", gate.LeaseToken); err != nil {
		t.Fatalf("release mutation gate: %v", err)
	}

	// After release, mutations allowed again
	if err := store.CheckMutationAllowed(ctx); err != nil {
		t.Fatalf("expected CheckMutationAllowed to succeed after gate release: %v", err)
	}

	// 2. Create an active auth attempt (in-progress browser login session)
	attempt, err := store.StartAuthAttempt(ctx, "operator@example.com", 5*time.Minute)
	if err != nil {
		t.Fatalf("start auth attempt: %v", err)
	}
	if attempt.ID == "" {
		t.Fatal("empty attempt ID")
	}

	// Verify report shows active auth attempt count is 1
	report, err := store.ActiveAuthAttempts(ctx)
	if err != nil {
		t.Fatalf("get active auth report: %v", err)
	}
	if report.ActiveCount != 1 {
		t.Fatalf("expected 1 active auth attempt, got %d", report.ActiveCount)
	}

	// 3. Concurrent auth attempt: Second attempt must fail with ErrAuthAttemptActive
	_, err = store.StartAuthAttempt(ctx, "another-user@example.com", 5*time.Minute)
	if err == nil {
		t.Fatal("expected concurrent auth attempt to fail while another is active")
	}

	// 4. Gate 8: Active auth blocks deploy mutation gate acquisition
	_, err = store.AcquireMutationGate(ctx, "deploy-candidate", 1*time.Minute, "candidate deploy")
	if err == nil {
		t.Fatal("expected AcquireMutationGate to FAIL while active auth attempt is in progress")
	}

	// 5. Complete / terminalize auth attempt
	if err := store.FinishAuthAttempt(ctx, attempt.ID, "CANCELLED"); err != nil {
		t.Fatalf("finish auth attempt: %v", err)
	}

	// Verify active count is now 0
	report, err = store.ActiveAuthAttempts(ctx)
	if err != nil {
		t.Fatalf("report after completion: %v", err)
	}
	if report.ActiveCount != 0 {
		t.Fatalf("expected 0 active attempts, got %d", report.ActiveCount)
	}

	// 6. Now deploy mutation gate can be acquired again cleanly
	gate2, err := store.AcquireMutationGate(ctx, "deploy-candidate-2", 1*time.Minute, "clean deploy")
	if err != nil {
		t.Fatalf("expected mutation gate acquisition to succeed after auth cleared: %v", err)
	}
	_ = store.ReleaseMutationGate(ctx, "deploy-candidate-2", gate2.LeaseToken)
	_ = conn
}
