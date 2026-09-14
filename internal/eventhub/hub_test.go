package eventhub

import (
	"sync"
	"testing"
	"time"
)

func TestHubPublishSubscribe(t *testing.T) {
	hub := New()
	_, ch, cancel := hub.Subscribe()
	defer cancel()

	if count := hub.SubscriberCount(); count != 1 {
		t.Fatalf("expected 1 subscriber, got %d", count)
	}

	event := Event{
		Seq:         1,
		Epoch:       "ep1",
		EventType:   "bank.transaction.credit",
		AggregateID: "txn_1",
		Payload:     []byte(`{"amount":100}`),
		CreatedAt:   "2026-09-12T00:00:00Z",
	}

	hub.Publish(event)

	select {
	case received := <-ch:
		if received.Seq != 1 || received.EventType != "bank.transaction.credit" {
			t.Fatalf("received unexpected event: %+v", received)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for event")
	}

	cancel()
	if count := hub.SubscriberCount(); count != 0 {
		t.Fatalf("expected 0 subscribers after cancel, got %d", count)
	}
}

func TestHubNonBlockingSlowConsumer(t *testing.T) {
	hub := New()
	_, slowCh, cancel := hub.Subscribe()
	defer cancel()

	// Fill slowCh buffer (cap 128)
	for i := 0; i < 150; i++ {
		hub.Publish(Event{Seq: int64(i)})
	}

	if count := hub.SubscriberCount(); count != 0 {
		t.Fatalf("expected slow subscriber to be disconnected, got %d", count)
	}
	for range slowCh {
	}
	if hub.DroppedNotifications() == 0 {
		t.Fatal("expected overflow counter to increase")
	}

	// Cancellation remains safe after overflow removed the subscriber.
	cancel()
}

func TestHubConcurrentPublishers(t *testing.T) {
	hub := New()
	_, ch, cancel := hub.Subscribe()
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			hub.Publish(Event{Seq: int64(idx)})
		}(i)
	}
	wg.Wait()

	// Read received events
	received := 0
drain:
	for {
		select {
		case <-ch:
			received++
		default:
			break drain
		}
	}
	if received == 0 {
		t.Fatal("expected to receive events")
	}
}
