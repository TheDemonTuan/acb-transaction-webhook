package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAuthAttemptFencesConnectionGeneration(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	connection, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAuthAttempt(ctx, "owner", time.Minute)
	if err != nil || attempt.Generation != connection.Generation+1 {
		t.Fatalf("attempt=%+v err=%v", attempt, err)
	}
	if _, err := store.StartAuthAttempt(ctx, "owner", time.Minute); err == nil {
		t.Fatal("allowed a concurrent auth attempt")
	}
	if err := store.FinishAuthAttempt(ctx, attempt.ID, "CANCELLED"); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Connection(ctx)
	if err != nil || updated.State != "AUTH_REQUIRED" || updated.Generation != attempt.Generation+1 {
		t.Fatalf("connection=%+v err=%v", updated, err)
	}
}

func TestFinishSupersededAuthDoesNotAdvanceCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAuthAttempt(ctx, "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='AUTH_REQUIRED', generation=generation+3`); err != nil {
		t.Fatal(err)
	}
	before, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAuthAttempt(ctx, attempt.ID, "FAILED"); !errors.Is(err, ErrGenerationFenceMismatch) {
		t.Fatalf("expected stale auth attempt to be fenced, got %v", err)
	}
	after, err := store.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || after.Generation != before.Generation {
		t.Fatalf("stale attempt mutated current connection: before=%+v after=%+v", before, after)
	}
	active, err := store.AuthAttemptStatusForOwner(ctx, attempt.ID, "owner")
	if err != nil || active.Status == "FAILED" {
		t.Fatalf("stale attempt was terminalized: attempt=%+v err=%v", active, err)
	}
}

func TestAuthAttemptLookupAndOwnership(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}

	attempt, err := store.StartAuthAttempt(ctx, "alice@example.com", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Active and unexpired lookup succeeds for owner
	got, err := store.AuthAttemptForOwner(ctx, attempt.ID, "alice@example.com")
	if err != nil || got.ID != attempt.ID {
		t.Fatalf("AuthAttemptForOwner failed: got=%+v, err=%v", got, err)
	}

	// Wrong owner rejected
	if _, err := store.AuthAttemptForOwner(ctx, attempt.ID, "bob@example.com"); err == nil {
		t.Fatal("expected error for wrong owner in AuthAttemptForOwner")
	}
	if _, err := store.AuthAttemptStatusForOwner(ctx, attempt.ID, "bob@example.com"); err == nil {
		t.Fatal("expected error for wrong owner in AuthAttemptStatusForOwner")
	}

	// Status lookup succeeds for owner
	gotStatus, err := store.AuthAttemptStatusForOwner(ctx, attempt.ID, "alice@example.com")
	if err != nil || gotStatus.ID != attempt.ID {
		t.Fatalf("AuthAttemptStatusForOwner failed: got=%+v, err=%v", gotStatus, err)
	}

	// Finish attempt as CANCELLED
	if err := store.FinishAuthAttempt(ctx, attempt.ID, "CANCELLED"); err != nil {
		t.Fatal(err)
	}

	// Terminal attempt: AuthAttemptForOwner must reject (protects VNC)
	if _, err := store.AuthAttemptForOwner(ctx, attempt.ID, "alice@example.com"); err == nil {
		t.Fatal("AuthAttemptForOwner should reject terminal attempt")
	}

	// Terminal attempt: AuthAttemptStatusForOwner must return terminal attempt
	gotTerminal, err := store.AuthAttemptStatusForOwner(ctx, attempt.ID, "alice@example.com")
	if err != nil || gotTerminal.Status != "CANCELLED" {
		t.Fatalf("AuthAttemptStatusForOwner terminal failed: got=%+v, err=%v", gotTerminal, err)
	}
}

func TestStaleAuthAttemptExpiryAndRecovery(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}

	// 1. Create an attempt with short TTL
	attempt, err := store.StartAuthAttempt(ctx, "admin@example.com", time.Second)
	if err != nil {
		t.Fatalf("start initial attempt: %v", err)
	}

	// Active attempt should be found
	active, found, err := store.ActiveAuthAttemptForOwner(ctx, "admin@example.com")
	if err != nil || !found || active.ID != attempt.ID {
		t.Fatalf("expected active attempt %s, got %+v (found=%v, err=%v)", attempt.ID, active, found, err)
	}

	// Wait for TTL to elapse
	time.Sleep(1100 * time.Millisecond)

	// Expire stale attempts
	count, err := store.ExpireStaleAuthAttempts(ctx)
	if err != nil {
		t.Fatalf("expire stale attempts: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 expired attempt, got %d", count)
	}

	// Active attempt should now be false
	_, found, err = store.ActiveAuthAttemptForOwner(ctx, "admin@example.com")
	if err != nil || found {
		t.Fatalf("expected no active attempt, got found=%v, err=%v", found, err)
	}

	// Connection should be back in AUTH_REQUIRED
	conn, err := store.Connection(ctx)
	if err != nil || conn.State != "AUTH_REQUIRED" {
		t.Fatalf("expected connection to be AUTH_REQUIRED, got %+v, err=%v", conn, err)
	}

	// 2. Starting a new auth attempt should now succeed seamlessly without UNIQUE constraint failure
	attempt2, err := store.StartAuthAttempt(ctx, "admin@example.com", time.Minute)
	if err != nil {
		t.Fatalf("start attempt after expiry failed: %v", err)
	}
	if attempt2.ID == attempt.ID {
		t.Fatalf("expected new attempt ID, got %s", attempt2.ID)
	}
}

func TestActiveAuthAttempts(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Initial: 0 active attempts
	rep, err := store.ActiveAuthAttempts(ctx)
	if err != nil {
		t.Fatalf("ActiveAuthAttempts failed: %v", err)
	}
	if rep.ActiveCount != 0 || len(rep.Attempts) != 0 {
		t.Fatalf("expected 0 active attempts, got %+v", rep)
	}

	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}

	// Start an attempt with 1 hour TTL
	attempt, err := store.StartAuthAttempt(ctx, "admin@example.com", time.Hour)
	if err != nil {
		t.Fatalf("StartAuthAttempt: %v", err)
	}

	rep, err = store.ActiveAuthAttempts(ctx)
	if err != nil {
		t.Fatalf("ActiveAuthAttempts failed: %v", err)
	}
	if rep.ActiveCount != 1 || len(rep.Attempts) != 1 {
		t.Fatalf("expected 1 active attempt, got %+v", rep)
	}
	if rep.Attempts[0].ID != attempt.ID {
		t.Fatalf("expected attempt ID %s, got %s", attempt.ID, rep.Attempts[0].ID)
	}

	// Cancel attempt
	if err := store.FinishAuthAttempt(ctx, attempt.ID, "CANCELLED"); err != nil {
		t.Fatalf("FinishAuthAttempt: %v", err)
	}

	rep, err = store.ActiveAuthAttempts(ctx)
	if err != nil {
		t.Fatalf("ActiveAuthAttempts failed: %v", err)
	}
	if rep.ActiveCount != 0 || len(rep.Attempts) != 0 {
		t.Fatalf("expected 0 active attempts after cancellation, got %+v", rep)
	}
}
