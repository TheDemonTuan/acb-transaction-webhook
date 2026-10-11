package realtimestream

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestJournalPublisherInitAndStreaming(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "journal_publisher.db")
	store, err := storage.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Append initial event before Init
	_, err = store.AppendJournalEvent(context.Background(), "ep1", "initial.event", "agg-0", []byte(`{"init":true}`))
	if err != nil {
		t.Fatal(err)
	}

	hub := eventhub.New()
	pub := NewJournalPublisher(store, hub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := pub.Init(ctx); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if pub.Cursor() != 1 {
		t.Fatalf("expected cursor 1 after Init, got %d", pub.Cursor())
	}

	subID, subCh, unsub := hub.Subscribe()
	defer unsub()
	_ = subID

	go pub.Run(ctx)

	// Append a new event after Init
	seq, err := store.AppendJournalEvent(context.Background(), "ep1", "bank.transaction.credit", "tx-1", []byte(`{"credit":50000}`))
	if err != nil {
		t.Fatal(err)
	}
	if seq != 2 {
		t.Fatalf("expected seq 2, got %d", seq)
	}

	// Wake publisher
	pub.Wake()

	select {
	case event := <-subCh:
		if event.Seq != 2 || event.AggregateID != "tx-1" || event.EventType != "bank.transaction.credit" {
			t.Fatalf("unexpected event received: %+v", event)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for event from hub")
	}

	if pub.Cursor() != 2 {
		t.Fatalf("expected cursor 2, got %d", pub.Cursor())
	}
}
