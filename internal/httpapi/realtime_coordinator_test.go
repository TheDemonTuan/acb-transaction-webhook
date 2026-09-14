package httpapi

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

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
	server.WithRealtimeInput(coordinator.Input())
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
	server.WithRealtimeInput(coordinator.Input())
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
