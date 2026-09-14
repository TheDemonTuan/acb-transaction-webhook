package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type recoveryMockSender struct {
	deliveredCount atomic.Int32
	processDelay   time.Duration
}

func (m *recoveryMockSender) Send(ctx context.Context, req notification.SendRequest) notification.SendResult {
	if m.processDelay > 0 {
		select {
		case <-ctx.Done():
			return notification.SendResult{
				Outcome:        notification.OutcomeRetry,
				StatusCode:     0,
				SanitizedError: "context cancelled during delivery",
			}
		case <-time.After(m.processDelay):
		}
	}
	m.deliveredCount.Add(1)
	return notification.SendResult{
		Outcome:    notification.OutcomeSuccess,
		StatusCode: 200,
		LatencyMs:  5,
	}
}

// TestDispatcherTermAndRestartRecovery verifies:
// 1. Dispatcher shuts down cleanly when context is canceled (SIGTERM);
// 2. In-flight deliveries with expired leases are safely recovered upon successor restart;
// 3. All pending notifications reach terminal status without duplicate delivery.
func TestDispatcherTermAndRestartRecovery(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "gateway.db")

	keyPath := filepath.Join(dbDir, "master.key")
	rawKey := make([]byte, 32)
	_, _ = rand.Read(rawKey)
	_ = os.WriteFile(keyPath, []byte(hex.EncodeToString(rawKey)), 0o600)
	kr, err := security.LoadKeyring(keyPath)
	if err != nil {
		t.Fatalf("load keyring: %v", err)
	}

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	defer store.Close()
	store.WithKeyring(kr)

	conn, _ := store.ConfigureConnection(ctx, "***3333")
	_, _ = store.DB().ExecContext(ctx, "UPDATE connections SET state = 'MONITORING' WHERE id = ?", conn.ID)

	ep, err := store.CreateEndpointWithSecret(ctx, "Webhook Hook", "https://example.com/webhook")
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	_ = store.SetEndpointStatus(ctx, ep.ID, "ACTIVE")

	// Ingest 5 transactions to generate 5 deliveries
	var items []storage.BatchTransactionItem
	for i := 1; i <= 5; i++ {
		items = append(items, storage.BatchTransactionItem{
			Number:        filepath.Base(dbDir) + "_TX_" + string(rune('0'+i)),
			TransactionAt: time.Now().UTC().Format(time.RFC3339),
			Credit:        100000,
			Description:   "Test delivery",
		})
	}
	_, err = store.IngestTransactionsBatch(ctx, conn.ID, conn.Generation, "***3333", items, false)
	if err != nil {
		t.Fatalf("ingest batch: %v", err)
	}

	// Setup mock sender
	sender1 := &recoveryMockSender{processDelay: 10 * time.Millisecond}
	reg1 := notification.NewRegistry()
	reg1.Register("WEBHOOK", sender1)

	disp1 := notification.NewDispatcher(store, reg1).SetWorkers(1)

	// Process 2 deliveries explicitly with DispatchOne
	p1, err := disp1.DispatchOne(ctx)
	if err != nil || !p1 {
		t.Fatalf("dispatch 1: processed=%v, err=%v", p1, err)
	}
	p2, err := disp1.DispatchOne(ctx)
	if err != nil || !p2 {
		t.Fatalf("dispatch 2: processed=%v, err=%v", p2, err)
	}

	deliveredPhase1 := sender1.deliveredCount.Load()
	if deliveredPhase1 != 2 {
		t.Fatalf("expected 2 delivered in phase 1, got %d", deliveredPhase1)
	}

	// Now start Dispatcher 1 in background with cancellable context (simulating SIGTERM drill)
	termCtx, cancelDisp1 := context.WithCancel(ctx)
	disp1Done := make(chan struct{})
	go func() {
		defer close(disp1Done)
		disp1.Start(termCtx)
	}()

	// Send SIGTERM immediately
	time.Sleep(10 * time.Millisecond)
	cancelDisp1()

	// Dispatcher 1 must stop cleanly within 2 seconds
	select {
	case <-disp1Done:
		// Clean exit
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not shut down cleanly on SIGTERM within timeout")
	}

	// Reopen/simulate time passage so any in-flight lease expires
	// In SQLite: reset any stuck IN_FLIGHT leases to past so ClaimDelivery can claim them
	pastTime := time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339Nano)
	_, err = store.DB().ExecContext(ctx, "UPDATE deliveries SET lease_until = ? WHERE status = 'IN_FLIGHT'", pastTime)
	if err != nil {
		t.Fatalf("expire leases: %v", err)
	}

	// Start Dispatcher 2 (successor instance after restart)
	sender2 := &recoveryMockSender{processDelay: 0}
	reg2 := notification.NewRegistry()
	reg2.Register("WEBHOOK", sender2)

	disp2 := notification.NewDispatcher(store, reg2)

	// Process remaining 3 deliveries
	for i := 0; i < 3; i++ {
		processed, err := disp2.DispatchOne(ctx)
		if err != nil {
			t.Fatalf("dispatch remaining %d: %v", i, err)
		}
		if !processed {
			t.Fatalf("expected delivery available at step %d", i)
		}
	}

	// No more deliveries left
	more, err := disp2.DispatchOne(ctx)
	if err != nil {
		t.Fatalf("dispatch check: %v", err)
	}
	if more {
		t.Fatal("unexpected extra delivery found")
	}

	totalDelivered := deliveredPhase1 + sender2.deliveredCount.Load()
	if totalDelivered != 5 {
		t.Fatalf("expected 5 total deliveries across lifecycle, got %d (p1=%d, p2=%d)",
			totalDelivered, deliveredPhase1, sender2.deliveredCount.Load())
	}

	// Verify no pending deliveries remain in store
	sum, err := store.DeliverySummary(ctx)
	if err != nil {
		t.Fatalf("delivery summary: %v", err)
	}
	if sum.Pending != 0 {
		t.Fatalf("expected 0 pending deliveries, got %d", sum.Pending)
	}
}
