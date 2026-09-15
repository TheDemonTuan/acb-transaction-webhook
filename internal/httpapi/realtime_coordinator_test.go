package httpapi

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
)

func TestRealtimeCoordinatorSubmitDoesNotBlockWhenQueueIsFull(t *testing.T) {
	registry := telemetry.NewRegistry()
	previousRegistry := telemetry.Default
	telemetry.Default = registry
	t.Cleanup(func() { telemetry.Default = previousRegistry })

	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	coordinator := NewRealtimeCoordinator(New(config.Config{}, store).WithEventHub(eventhub.New()), time.Hour)
	defer close(coordinator.done)
	for i := 0; i < cap(coordinator.input); i++ {
		if err := coordinator.Submit(eventhub.Event{Seq: int64(i + 1), Epoch: realtimeEpoch}); err != nil {
			t.Fatal(err)
		}
	}

	result := make(chan error, 1)
	go func() {
		result <- coordinator.Submit(eventhub.Event{Seq: int64(cap(coordinator.input) + 1), Epoch: realtimeEpoch})
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("queue-full submit returned an error: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("queue-full submit blocked")
	}

	if got := len(coordinator.reconcileNow); got != 1 {
		t.Fatalf("expected one pending reconcile request, got %d", got)
	}
	if got := telemetry.Default.FullSnapshot().Realtime.CoordinatorQueueFullTotal; got != 1 {
		t.Fatalf("expected queue-full counter 1, got %d", got)
	}

	if err := coordinator.Submit(eventhub.Event{Seq: 999, Epoch: realtimeEpoch}); err != nil {
		t.Fatalf("second queue-full submit returned an error: %v", err)
	}
	if got := len(coordinator.reconcileNow); got != 1 {
		t.Fatalf("expected reconcile requests to coalesce, got %d", got)
	}
}

func TestRealtimeCoordinatorStopsWhenRunCannotStart(t *testing.T) {
	coordinator := NewRealtimeCoordinator(nil, time.Hour)
	coordinator.Run(context.Background())
	if err := coordinator.Submit(eventhub.Event{}); err != context.Canceled {
		t.Fatalf("expected cancellation after invalid run, got %v", err)
	}
}

func TestRealtimeCoordinatorSubmitStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	coordinator := NewRealtimeCoordinator(New(config.Config{}, store).WithEventHub(eventhub.New()), time.Hour)
	go coordinator.Run(ctx)
	waitForCoordinatorSeq(t, coordinator, 0)
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		err = coordinator.Submit(eventhub.Event{Seq: 1, Epoch: realtimeEpoch})
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("submit did not observe coordinator cancellation")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRealtimeCoordinatorPublishesContiguousEventDirectly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	hub := eventhub.New()
	server := New(config.Config{}, store).WithEventHub(hub)
	coordinator := NewRealtimeCoordinator(server, time.Hour)
	server.WithRealtimeSubmit(coordinator.Submit)
	go coordinator.Run(ctx)
	waitForCoordinatorSeq(t, coordinator, 0)

	_, events, stop := hub.Subscribe()
	defer stop()
	server.Publish(eventhub.Event{Seq: 1, Epoch: realtimeEpoch, EventType: "test.event", Payload: []byte(`{"value":1}`)})

	select {
	case event := <-events:
		if event.Seq != 1 {
			t.Fatalf("expected seq 1, got %d", event.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for contiguous event")
	}
}

func TestRealtimeCoordinatorRepairsGapFromJournal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	hub := eventhub.New()
	server := New(config.Config{}, store).WithEventHub(hub)
	coordinator := NewRealtimeCoordinator(server, time.Hour)
	server.WithRealtimeSubmit(coordinator.Submit)
	go coordinator.Run(ctx)
	waitForCoordinatorSeq(t, coordinator, 0)

	_, events, stop := hub.Subscribe()
	defer stop()
	for i := 1; i <= 3; i++ {
		if _, err := store.AppendJournalEvent(ctx, realtimeEpoch, "test.event", "test", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	server.Publish(eventhub.Event{Seq: 3, Epoch: realtimeEpoch, EventType: "test.event", Payload: []byte(`{}`)})

	for want := int64(1); want <= 3; want++ {
		select {
		case event := <-events:
			if event.Seq != want {
				t.Fatalf("expected seq %d, got %d", want, event.Seq)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for seq %d", want)
		}
	}
}

func TestRealtimeCoordinatorReconcilesMoreThanOneJournalBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	hub := eventhub.New()
	coordinator := NewRealtimeCoordinator(New(config.Config{}, store).WithEventHub(hub), time.Hour)
	go coordinator.Run(ctx)
	waitForCoordinatorSeq(t, coordinator, 0)
	const total = replayBatch*2 + 100
	for i := 1; i <= total; i++ {
		if _, err := store.AppendJournalEvent(ctx, realtimeEpoch, "test.event", "", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	coordinator.RequestReconcile()
	waitForCoordinatorSeq(t, coordinator, total)
}

func TestRealtimeCoordinatorAdvancesPastPermanentSequenceHole(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "coordinator-hole.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.DB().ExecContext(ctx, `INSERT INTO event_journal(seq, epoch, event_type, aggregate_id, payload_json, created_at) VALUES(2, 'ep1', 'test.event', 'test', '{}', '2026-09-14T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	hub := eventhub.New()
	server := New(config.Config{}, store).WithEventHub(hub)
	coordinator := NewRealtimeCoordinator(server, time.Hour)
	coordinator.setLastSeq(0)
	coordinator.reconcile(ctx, 2, false)
	if coordinator.LastSeq() != 2 {
		t.Fatalf("expected cursor to advance to durable high-water mark 2, got %d", coordinator.LastSeq())
	}
}

func waitForCoordinatorSeq(t *testing.T, coordinator *RealtimeCoordinator, want int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if coordinator.LastSeq() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("coordinator did not initialize at seq %d", want)
}
