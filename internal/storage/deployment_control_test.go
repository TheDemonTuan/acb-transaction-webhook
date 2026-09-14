package storage_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestDeploymentControl_Lifecycle(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_gate_lifecycle.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Initial gate state must be OPEN
	gate, err := store.GetDeploymentGate(ctx)
	if err != nil {
		t.Fatalf("get gate: %v", err)
	}
	if gate.GateState != "OPEN" {
		t.Fatalf("expected initial gate state OPEN, got %s", gate.GateState)
	}
	if !gate.TableExists {
		t.Fatalf("expected tableExists true after migration")
	}

	// Normal mutations must be allowed
	if err := store.CheckMutationAllowed(ctx); err != nil {
		t.Fatalf("check mutation allowed: %v", err)
	}

	// Acquire gate
	acquired, err := store.AcquireMutationGate(ctx, "deploy-worker-1", 5*time.Second, "worker-upgrade")
	if err != nil {
		t.Fatalf("acquire gate: %v", err)
	}
	if acquired.GateState != "LOCKED" || acquired.Owner != "deploy-worker-1" {
		t.Fatalf("unexpected acquired gate: %+v", acquired)
	}
	if acquired.LeaseToken == "" {
		t.Fatalf("expected non-empty lease token")
	}

	// Mutations must now be BLOCKED
	err = store.CheckMutationAllowed(ctx)
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Fatalf("expected ErrMutationGateLocked, got %v", err)
	}

	// Another deployer cannot acquire active lease
	_, err = store.AcquireMutationGate(ctx, "deploy-schema-2", 5*time.Second, "schema-migration")
	if !errors.Is(err, storage.ErrMutationGateHeld) {
		t.Fatalf("expected ErrMutationGateHeld, got %v", err)
	}

	// Renew lease
	if err := store.RenewMutationGate(ctx, "deploy-worker-1", acquired.LeaseToken, 10*time.Second); err != nil {
		t.Fatalf("renew lease: %v", err)
	}

	// Wrong token cannot renew or release
	if err := store.ReleaseMutationGate(ctx, "deploy-worker-1", "wrong-token"); !errors.Is(err, storage.ErrInvalidLeaseToken) {
		t.Fatalf("expected ErrInvalidLeaseToken, got %v", err)
	}

	// Release lease with correct owner and token
	if err := store.ReleaseMutationGate(ctx, "deploy-worker-1", acquired.LeaseToken); err != nil {
		t.Fatalf("release gate: %v", err)
	}

	// Gate is OPEN again
	gate, err = store.GetDeploymentGate(ctx)
	if err != nil {
		t.Fatalf("get gate after release: %v", err)
	}
	if gate.GateState != "OPEN" {
		t.Fatalf("expected gate OPEN after release, got %s", gate.GateState)
	}
	if err := store.CheckMutationAllowed(ctx); err != nil {
		t.Fatalf("mutations should be allowed after release: %v", err)
	}
}

func TestDeploymentControl_StaleOwnerRecovery(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_gate_stale.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// Owner A acquires with very short lease (50ms)
	gateA, err := store.AcquireMutationGate(ctx, "owner-crash-A", 50*time.Millisecond, "worker-deploy")
	if err != nil {
		t.Fatalf("acquire gate A: %v", err)
	}

	// Wait for lease A to expire
	time.Sleep(100 * time.Millisecond)

	// Gate status should indicate stale
	status, err := store.GetDeploymentGate(ctx)
	if err != nil {
		t.Fatalf("get gate status: %v", err)
	}
	if !status.IsStale {
		t.Fatalf("expected gate to be marked stale after expiry")
	}

	// Owner B recovers the stale lease
	gateB, err := store.AcquireMutationGate(ctx, "owner-recovery-B", 2*time.Second, "recovery-deploy")
	if err != nil {
		t.Fatalf("acquire gate B: %v", err)
	}
	if gateB.Owner != "owner-recovery-B" {
		t.Fatalf("expected owner B, got %s", gateB.Owner)
	}
	if gateB.FenceGeneration <= gateA.FenceGeneration {
		t.Fatalf("expected fence generation to increase from %d, got %d", gateA.FenceGeneration, gateB.FenceGeneration)
	}

	_ = store.ReleaseMutationGate(ctx, "owner-recovery-B", gateB.LeaseToken)
}

func TestDeploymentControl_ConcurrentAuthStartVsDeploy(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_gate_concurrent.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	_, err = store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatalf("configure connection: %v", err)
	}

	const iterations = 50
	for i := 0; i < iterations; i++ {
		// Ensure clean state
		_ = store.ForceUnlockMutationGate(ctx, "reset")
		_, _ = store.DB().ExecContext(ctx, "DELETE FROM auth_attempts")
		_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state='AUTH_REQUIRED'")

		var wg sync.WaitGroup
		var authSuccess, deploySuccess atomic.Int32
		var authErrCount, deployErrCount atomic.Int32

		wg.Add(2)
		// Goroutine 1: Tries to start auth
		go func(iter int) {
			defer wg.Done()
			_, err := store.StartAuthAttempt(ctx, fmt.Sprintf("user-%d", iter), 2*time.Minute)
			if err == nil {
				authSuccess.Add(1)
			} else {
				authErrCount.Add(1)
			}
		}(i)

		// Goroutine 2: Tries to acquire mutation gate
		go func(iter int) {
			defer wg.Done()
			_, err := store.AcquireMutationGate(ctx, fmt.Sprintf("deployer-%d", iter), 2*time.Minute, "upgrade")
			if err == nil {
				deploySuccess.Add(1)
			} else {
				deployErrCount.Add(1)
			}
		}(i)

		wg.Wait()

		// Invariant: Both CANNOT succeed simultaneously!
		if authSuccess.Load() > 0 && deploySuccess.Load() > 0 {
			t.Fatalf("RACE CONDITION: Both StartAuthAttempt and AcquireMutationGate succeeded in iteration %d!", i)
		}
	}
}

func TestDeploymentControl_MutationGuards(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_gate_guards.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatalf("configure connection: %v", err)
	}

	// Acquire gate to lock mutations
	gate, err := store.AcquireMutationGate(ctx, "deploy-worker", 5*time.Minute, "upgrade")
	if err != nil {
		t.Fatalf("acquire gate: %v", err)
	}

	// 1. StartAuthAttempt must fail
	_, err = store.StartAuthAttempt(ctx, "owner", time.Minute)
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Errorf("StartAuthAttempt: expected ErrMutationGateLocked, got %v", err)
	}

	// 2. TransitionConnection must fail
	_, err = store.TransitionConnection(ctx, "pause")
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Errorf("TransitionConnection: expected ErrMutationGateLocked, got %v", err)
	}

	// 3. CreateEndpoint must fail
	_, err = store.CreateEndpoint(ctx, "webhook", "https://example.com/wh")
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Errorf("CreateEndpoint: expected ErrMutationGateLocked, got %v", err)
	}

	// 4. SavePaymentQR must fail
	_, err = store.SavePaymentQR(ctx, storage.PaymentQR{ConnectionID: conn.ID, AccountNumber: "123", AccountName: "Test"})
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Errorf("SavePaymentQR: expected ErrMutationGateLocked, got %v", err)
	}

	// 5. SaveVoiceSettings must fail
	_, err = store.SaveVoiceSettings(ctx, storage.VoiceSettings{ProviderMode: "ONLINE_AUTO", EdgeVoice: "vi-VN-HoaiMyNeural"})
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Errorf("SaveVoiceSettings: expected ErrMutationGateLocked, got %v", err)
	}

	// 6. SaveMonitorSettings must fail
	_, err = store.SaveMonitorSettings(ctx, storage.DefaultMonitorSettings)
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Errorf("SaveMonitorSettings: expected ErrMutationGateLocked, got %v", err)
	}

	// 7. CreateBarkChannel must fail
	_, err = store.CreateBarkChannel(ctx, "bark-chan", "device-key-1", nil)
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Errorf("CreateBarkChannel: expected ErrMutationGateLocked, got %v", err)
	}

	// 8. CreateHistorySyncJob must fail
	_, err = store.CreateHistorySyncJob(ctx, conn.ID, "2026-09-01", "2026-09-02")
	if !errors.Is(err, storage.ErrMutationGateLocked) {
		t.Errorf("CreateHistorySyncJob: expected ErrMutationGateLocked, got %v", err)
	}

	// Release gate
	if err := store.ReleaseMutationGate(ctx, "deploy-worker", gate.LeaseToken); err != nil {
		t.Fatalf("release gate: %v", err)
	}

	// Now StartAuthAttempt must succeed
	attempt, err := store.StartAuthAttempt(ctx, "owner", time.Minute)
	if err != nil {
		t.Fatalf("StartAuthAttempt after release failed: %v", err)
	}
	if attempt.Status != "STARTING" {
		t.Fatalf("expected attempt STARTING, got %s", attempt.Status)
	}

	// While auth is active, AcquireMutationGate must be rejected!
	_, err = store.AcquireMutationGate(ctx, "deployer", time.Minute, "upgrade")
	if !errors.Is(err, storage.ErrActiveAuthInProgress) {
		t.Fatalf("expected ErrActiveAuthInProgress, got %v", err)
	}
}

func TestDeploymentControl_BootstrapTransition(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_bootstrap.db")

	// Open raw store without migrations to simulate pre-v9 database
	rawStore, err := storage.OpenWithOptions(ctx, dbPath, storage.OpenOptions{RunMigrations: false})
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	defer rawStore.Close()

	// Gate status in bootstrap mode
	gate, err := rawStore.GetDeploymentGate(ctx)
	if err != nil {
		t.Fatalf("get gate in bootstrap: %v", err)
	}
	if !gate.Bootstrap || gate.TableExists || gate.GateState != "OPEN" {
		t.Fatalf("unexpected bootstrap gate: %+v", gate)
	}

	// Check mutation allowed should pass in bootstrap mode
	if err := rawStore.CheckMutationAllowed(ctx); err != nil {
		t.Fatalf("mutation allowed in bootstrap: %v", err)
	}

	// Acquire gate in bootstrap mode should succeed when 0 active auth
	acq, err := rawStore.AcquireMutationGate(ctx, "bootstrapper", time.Minute, "init")
	if err != nil {
		t.Fatalf("acquire in bootstrap: %v", err)
	}
	if !acq.Bootstrap {
		t.Fatalf("expected bootstrap flag on acquired gate")
	}

	_ = rawStore.Close()

	// Now run full migrations (1 to 9)
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer store.Close()

	// After migration, gate is fully initialized in database
	gate, err = store.GetDeploymentGate(ctx)
	if err != nil {
		t.Fatalf("get gate after migration: %v", err)
	}
	if gate.Bootstrap || !gate.TableExists || gate.GateState != "OPEN" {
		t.Fatalf("unexpected post-migration gate: %+v", gate)
	}
}
