package httpapi

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestSSEStreamInitialStateAndLiveEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway_sse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	hub := eventhub.New()
	cfg := config.Config{Production: false, DevelopmentSubject: "dev@example.com"}
	server := New(cfg, store).WithEventHub(hub)

	// Append an initial event to DB
	seq1, err := store.AppendJournalEvent(ctx, "ep1", "bank.transaction.credit", "txn_101", []byte(`{"amount":101,"transactionId":"txn_101"}`))
	if err != nil {
		t.Fatal(err)
	}

	// 1. Initial connect without cursor
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		server.Handler().ServeHTTP(w, req)
	}()

	// Wait briefly for handler to start and write initial state
	time.Sleep(50 * time.Millisecond)

	// Publish live event
	hub.Publish(eventhub.Event{
		Seq:         seq1 + 1,
		Epoch:       "ep1",
		EventType:   "bank.transaction.credit",
		AggregateID: "txn_102",
		Payload:     []byte(`{"amount":102,"transactionId":"txn_102"}`),
	})

	time.Sleep(50 * time.Millisecond)
	// Cancel request context to terminate stream
	cancel()
	<-doneCh

	body := w.Body.String()
	if !strings.Contains(body, "event: initial_state") {
		t.Fatalf("expected initial_state in SSE body, got: %s", body)
	}
	if !strings.Contains(body, "txn_102") {
		t.Fatalf("expected live event txn_102 in SSE body, got: %s", body)
	}
}

func TestSSEReplayFromCursor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway_sse_replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	hub := eventhub.New()
	cfg := config.Config{Production: false, DevelopmentSubject: "dev@example.com"}
	server := New(cfg, store).WithEventHub(hub)

	// Append 2 events
	_, _ = store.AppendJournalEvent(ctx, "ep1", "bank.transaction.credit", "txn_201", []byte(`{"amount":201,"transactionId":"txn_201"}`))
	_, _ = store.AppendJournalEvent(ctx, "ep1", "bank.transaction.credit", "txn_202", []byte(`{"amount":202,"transactionId":"txn_202"}`))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("Last-Event-ID", "ep1:1")
	w := httptest.NewRecorder()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	server.Handler().ServeHTTP(w, req.WithContext(ctx))

	body := w.Body.String()
	// Should NOT contain txn_201 (seq 1), but SHOULD contain txn_202 (seq 2)
	if strings.Contains(body, "txn_201") {
		t.Errorf("expected txn_201 to be skipped with cursor ep1:1, got: %s", body)
	}
	if !strings.Contains(body, "txn_202") {
		t.Errorf("expected txn_202 replayed with cursor ep1:1, got: %s", body)
	}
}

func TestSSEResetStateOnInvalidCursor(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway_sse_reset.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	hub := eventhub.New()
	cfg := config.Config{Production: false, DevelopmentSubject: "dev@example.com"}
	server := New(cfg, store).WithEventHub(hub)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("Last-Event-ID", "ep999:invalid")
	w := httptest.NewRecorder()

	server.Handler().ServeHTTP(w, req)

	scanner := bufio.NewScanner(w.Body)
	var foundReset bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: reset_state") {
			foundReset = true
			break
		}
	}
	if !foundReset {
		t.Fatalf("expected reset_state event, got body: %s", w.Body.String())
	}
}

func TestJournalWatcher_DrainsOver100Events(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "journal_watcher_150.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	hub := eventhub.New()
	cfg := config.Config{Production: false}
	server := New(cfg, store).WithEventHub(hub)

	// Subscribe to live hub events
	_, subCh, subCancel := hub.Subscribe()
	defer subCancel()

	// Start JournalWatcher with a fast tick (20ms) before writing new events
	go server.RunJournalWatcher(ctx, 20*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	// Append 125 events to simulate external worker writing >100 events
	totalEvents := 125
	for i := 1; i <= totalEvents; i++ {
		payload := fmt.Sprintf(`{"index":%d,"pollId":"p_%d"}`, i, i)
		_, err := store.AppendJournalEvent(ctx, "ep1", "poll.completed", fmt.Sprintf("p_%d", i), []byte(payload))
		if err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}

	// Collect received events from hub
	received := 0
	timeout := time.After(3 * time.Second)
	for received < totalEvents {
		select {
		case ev := <-subCh:
			received++
			if ev.EventType != "poll.completed" {
				t.Errorf("unexpected event type: %s", ev.EventType)
			}
		case <-timeout:
			t.Fatalf("timed out waiting for all %d events, received only %d", totalEvents, received)
		}
	}
}

func TestSSEReplay_Over100Events(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "sse_replay_150.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	hub := eventhub.New()
	cfg := config.Config{Production: false}
	server := New(cfg, store).WithEventHub(hub)

	// Append 130 events
	totalEvents := 130
	for i := 1; i <= totalEvents; i++ {
		payload := fmt.Sprintf(`{"index":%d,"pollId":"p_%d"}`, i, i)
		_, _ = store.AppendJournalEvent(ctx, "ep1", "poll.completed", fmt.Sprintf("p_%d", i), []byte(payload))
	}

	// Reconnect with cursor ep1:30 -> expects 100 replayed events (31..130)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("Last-Event-ID", "ep1:30")
	w := httptest.NewRecorder()

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	server.Handler().ServeHTTP(w, req.WithContext(ctx))

	scanner := bufio.NewScanner(w.Body)
	eventCount := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: poll.completed") {
			eventCount++
		}
	}

	expectedCount := totalEvents - 30
	if eventCount != expectedCount {
		t.Fatalf("expected %d replayed events from cursor, got %d", expectedCount, eventCount)
	}
}

func TestJournalRetention(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Append an old event manually with created_at 48h ago
	oldTime := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO event_journal(epoch, event_type, aggregate_id, payload_json, created_at)
		VALUES('ep1', 'old.event', 'old_1', '{}', ?)
	`, oldTime)
	if err != nil {
		t.Fatal(err)
	}

	// Append a recent event
	_, err = store.AppendJournalEvent(ctx, "ep1", "recent.event", "recent_1", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	// Delete older than 24h
	deleted, err := store.DeleteJournalBefore(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteJournalBefore: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted old entry, got %d", deleted)
	}

	// Verify only recent event remains
	events, err := store.ReadJournalEvents(ctx, "ep1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].AggregateID != "recent_1" {
		t.Errorf("expected only recent_1 event to remain, got %v", events)
	}
}
