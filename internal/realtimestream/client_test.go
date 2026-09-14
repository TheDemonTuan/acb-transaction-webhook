package realtimestream_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/realtimestream"
)

func TestClientRoundtripAndJSONPreserved(t *testing.T) {
	hub := eventhub.New()
	token := "worker-secret-42"
	srv := realtimestream.NewServer(realtimestream.ServerConfig{
		Hub:          hub,
		Token:        token,
		Heartbeat:    5 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client, err := realtimestream.NewClient(realtimestream.ClientConfig{
		BaseURL: ts.URL,
		Token:   token,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	received := make(chan eventhub.Event, 5)

	go func() {
		_ = client.Consume(ctx, func(e eventhub.Event) error {
			received <- e
			return nil
		})
	}()

	time.Sleep(50 * time.Millisecond)

	inputEvent := eventhub.Event{
		Seq:         101,
		Epoch:       "ep1",
		EventType:   "poll.completed",
		AggregateID: "agg-xyz",
		Payload:     []byte(`{"status":"success","transactions":3}`),
		CreatedAt:   "2026-09-14T15:04:05Z",
	}
	hub.Publish(inputEvent)

	select {
	case got := <-received:
		if got.Seq != inputEvent.Seq {
			t.Errorf("seq = %d, want %d", got.Seq, inputEvent.Seq)
		}
		if got.Epoch != inputEvent.Epoch {
			t.Errorf("epoch = %q, want %q", got.Epoch, inputEvent.Epoch)
		}
		if got.EventType != inputEvent.EventType {
			t.Errorf("eventType = %q, want %q", got.EventType, inputEvent.EventType)
		}
		if got.AggregateID != inputEvent.AggregateID {
			t.Errorf("aggregateId = %q, want %q", got.AggregateID, inputEvent.AggregateID)
		}
		if string(got.Payload) != string(inputEvent.Payload) {
			t.Errorf("payload = %q, want %q", string(got.Payload), string(inputEvent.Payload))
		}
		if got.CreatedAt != inputEvent.CreatedAt {
			t.Errorf("createdAt = %q, want %q", got.CreatedAt, inputEvent.CreatedAt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestClientHeartbeatsIgnored(t *testing.T) {
	// Server sending only heartbeats and comments
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Write heartbeat comments
		fmt.Fprintf(w, ": heartbeat\n\n")
		flusher.Flush()
		fmt.Fprintf(w, ": ping\n\n")
		flusher.Flush()

		// Then write real event
		fmt.Fprintf(w, "id: ep1:1\nevent: test\ndata: {\"ok\":true}\n\n")
		flusher.Flush()

		time.Sleep(100 * time.Millisecond)
	}))
	defer ts.Close()

	client, err := realtimestream.NewClient(realtimestream.ClientConfig{
		BaseURL: ts.URL,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var events []eventhub.Event
	var mu sync.Mutex

	err = client.Consume(ctx, func(e eventhub.Event) error {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		cancel()
		return nil
	})

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event (comments ignored), got %d: %v", len(events), events)
	}
	if events[0].EventType != "test" || events[0].Seq != 1 || events[0].Epoch != "ep1" {
		t.Errorf("unexpected event: %+v", events[0])
	}
}

func TestClientNoRedirects(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer ts.Close()

	client, err := realtimestream.NewClient(realtimestream.ClientConfig{
		BaseURL: ts.URL,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err = client.Consume(ctx, func(e eventhub.Event) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error on redirect, got nil")
	}
}

func TestClientBoundedFrames(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Write huge line exceeding MaxFrameBytes
		hugePayload := make([]byte, 2048)
		for i := range hugePayload {
			hugePayload[i] = 'A'
		}
		fmt.Fprintf(w, "data: %s\n\n", hugePayload)
		flusher.Flush()
	}))
	defer ts.Close()

	client, err := realtimestream.NewClient(realtimestream.ClientConfig{
		BaseURL:       ts.URL,
		MaxFrameBytes: 512, // small frame limit
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err = client.Consume(ctx, func(e eventhub.Event) error {
		return nil
	})
	if !errors.Is(err, realtimestream.ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestClientRunReconnectAndLastEventID(t *testing.T) {
	var connCount atomic.Int32
	var receivedLastEventID atomic.Value
	receivedLastEventID.Store("")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		c := connCount.Add(1)

		lastID := r.Header.Get("Last-Event-ID")
		if lastID != "" {
			receivedLastEventID.Store(lastID)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		if c == 1 {
			// First connection: emit event 1 then close connection
			fmt.Fprintf(w, "id: ep1:42\nevent: first\ndata: {\"conn\":1}\n\n")
			flusher.Flush()
			return // close conn
		}

		// Second connection: emit event 2
		fmt.Fprintf(w, "id: ep1:43\nevent: second\ndata: {\"conn\":2}\n\n")
		flusher.Flush()

		time.Sleep(200 * time.Millisecond)
	}))
	defer ts.Close()

	client, err := realtimestream.NewClient(realtimestream.ClientConfig{
		BaseURL:        ts.URL,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var events []eventhub.Event
	var mu sync.Mutex

	errChan := make(chan error, 1)
	go func() {
		errChan <- client.Run(ctx, func(e eventhub.Event) error {
			mu.Lock()
			events = append(events, e)
			count := len(events)
			mu.Unlock()

			if count >= 2 {
				cancel()
			}
			return nil
		})
	}()

	select {
	case err := <-errChan:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Run to finish")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("expected 2 events across reconnects, got %d", len(events))
	}
	if events[0].Seq != 42 || events[1].Seq != 43 {
		t.Errorf("unexpected seqs: %d, %d", events[0].Seq, events[1].Seq)
	}

	if got := receivedLastEventID.Load().(string); got != "ep1:42" {
		t.Errorf("expected Last-Event-ID 'ep1:42' on reconnect, got %q", got)
	}
}

func TestClientRunHandlerErrorStops(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		fmt.Fprintf(w, "id: ep1:1\nevent: boom\ndata: {}\n\n")
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer ts.Close()

	client, err := realtimestream.NewClient(realtimestream.ClientConfig{
		BaseURL: ts.URL,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	myErr := errors.New("fatal handler failure")
	err = client.Run(ctx, func(e eventhub.Event) error {
		return myErr
	})

	if !errors.Is(err, myErr) {
		t.Fatalf("expected handler error %v, got %v", myErr, err)
	}
}
