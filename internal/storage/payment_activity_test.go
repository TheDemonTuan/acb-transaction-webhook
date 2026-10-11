package storage

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

// Financial writes acquire writeMu before asking database/sql for a connection.
// Activity must use the same ordering: waiting metadata must not consume the
// pool or queue for all its slots while a financial operation owns that mutex.
func TestPaymentActivityWaitsForWriterBeforePool(t *testing.T) {
	store := paymentOrderTestStore(t)
	assertPaymentMetadataWriterAdmission(t, store, func(ctx context.Context) error { return store.RecordPaymentActivity(ctx, "fixture-channel", "WEBHOOK") })
	if err := store.RecordPaymentActivity(context.Background(), "fixture-channel", "WEBHOOK"); err != nil {
		t.Fatal(err)
	}
	activity, err := store.PaymentActivity(context.Background(), "fixture-channel")
	if err != nil || activity.LastWebhookAt == nil {
		t.Fatalf("activity did not persist after writer release: activity=%+v err=%v", activity, err)
	}
}

func TestManagedPaymentMetadataWaitsForWriterBeforePool(t *testing.T) {
	for _, operation := range []string{"renew", "end", "quiesce"} {
		t.Run(operation, func(t *testing.T) {
			store := paymentOrderTestStore(t)
			write := func(ctx context.Context) error {
				switch operation {
				case "renew":
					return store.RenewPaymentProviderOperation(ctx, "fixture-token")
				case "end":
					return store.EndPaymentProviderOperation(ctx, "fixture-token")
				default:
					return store.SetPaymentProviderQuiesced(ctx, false)
				}
			}
			assertPaymentMetadataWriterAdmission(t, store, write)
		})
	}
}

func assertPaymentMetadataWriterAdmission(t *testing.T, store *Store, write func(context.Context) error) {
	t.Helper()
	ctx := context.Background()
	var connections []*sql.Conn
	for range 4 {
		conn, err := store.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
	}
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	store.writeMu.Lock()
	locked := true
	defer func() {
		if locked {
			store.writeMu.Unlock()
		}
	}()
	before := store.DB().Stats().WaitCount
	started := make(chan struct{}, 4)
	results := make(chan error, 4)
	activityCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			started <- struct{}{}
			results <- write(activityCtx)
		}()
	}
	for range 4 {
		<-started
	}
	// Observe a bounded contention window, not a performance threshold. Old
	// code immediately queues all four DB acquires despite the held writer.
	window := time.NewTimer(100 * time.Millisecond)
	tick := time.NewTicker(time.Millisecond)
	increased := false
	observing := true
	for observing {
		select {
		case <-window.C:
			observing = false
		case <-tick.C:
			if store.DB().Stats().WaitCount != before {
				increased = true
				observing = false
			}
		}
	}
	window.Stop()
	tick.Stop()
	cancel()
	store.writeMu.Unlock()
	locked = false
	workers.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled metadata returned %v", err)
		}
	}
	if increased {
		t.Fatal("metadata queued for DB connections while financial writer mutex was held")
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
	connections = nil
}
