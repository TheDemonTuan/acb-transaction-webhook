package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
)

const (
	realtimeEpoch = "ep1"
	replayBatch   = 250
)

func (s *Server) eventsStream(w http.ResponseWriter, r *http.Request) {
	s.eventsStreamFiltered(w, r, nil)
}

func (s *Server) publicEventsStream(w http.ResponseWriter, r *http.Request) {
	s.eventsStreamFiltered(w, r, func(eventType string) bool {
		return eventType == "bank.transaction.credit" || eventType == "payment.activated"
	})
}

func (s *Server) eventsStreamFiltered(w http.ResponseWriter, r *http.Request, allowEvent func(string) bool) {
	if s.eventHub == nil {
		http.Error(w, "event stream unavailable", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	_, live, cancel := s.eventHub.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("lastEventId")
	}

	var watermark int64
	if cursor == "" {
		maxSeq, err := s.store.GetMaxJournalSeq(ctx, realtimeEpoch)
		if err != nil {
			writeSSEError(rc, w, flusher, "storage_error")
			return
		}
		watermark = maxSeq
		if err := writeSSE(rc, w, flusher, "", "initial_state", fmt.Sprintf(`{"epoch":%q,"watermark":%d}`, realtimeEpoch, watermark)); err != nil {
			return
		}
	} else {
		parts := strings.SplitN(cursor, ":", 2)
		if len(parts) != 2 || parts[0] != realtimeEpoch {
			writeReset(rc, w, flusher, "invalid_cursor")
			return
		}
		afterSeq, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || afterSeq < 0 {
			writeReset(rc, w, flusher, "invalid_cursor")
			return
		}
		minSeq, err := s.store.GetMinJournalSeq(ctx, realtimeEpoch)
		if err != nil {
			writeSSEError(rc, w, flusher, "storage_error")
			return
		}
		maxSeq, err := s.store.GetMaxJournalSeq(ctx, realtimeEpoch)
		if err != nil {
			writeSSEError(rc, w, flusher, "storage_error")
			return
		}
		if afterSeq > maxSeq {
			writeReset(rc, w, flusher, "invalid_cursor")
			return
		}
		if minSeq > 0 && afterSeq < minSeq-1 {
			writeReset(rc, w, flusher, "retention_expired")
			return
		}
		watermark = afterSeq
	}

	for {
		entries, err := s.store.ReadJournalEvents(ctx, realtimeEpoch, watermark, replayBatch)
		if err != nil {
			writeSSEError(rc, w, flusher, "storage_error")
			return
		}
		for _, entry := range entries {
			if allowEvent != nil && !allowEvent(entry.EventType) {
				watermark = entry.Seq
				continue
			}
			if err := writeJournalEntry(rc, w, flusher, entry.Epoch, entry.Seq, entry.EventType, entry.Payload); err != nil {
				return
			}
			watermark = entry.Seq
		}
		if len(entries) < replayBatch {
			break
		}
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
			_ = rc.SetWriteDeadline(time.Time{})
		case hint, ok := <-live:
			if !ok {
				return
			}
			if hint.Epoch != realtimeEpoch || hint.Seq <= watermark {
				continue
			}
			if hint.Seq > watermark+1 {
				if !s.drainJournal(ctx, rc, w, flusher, &watermark, allowEvent) {
					return
				}
				if hint.Seq <= watermark {
					continue
				}
				if hint.Seq != watermark+1 {
					return
				}
			}
			if allowEvent != nil && !allowEvent(hint.EventType) {
				watermark = hint.Seq
				continue
			}
			start := time.Now()
			if err := writeJournalEntry(rc, w, flusher, hint.Epoch, hint.Seq, hint.EventType, hint.Payload); err != nil {
				return
			}
			telemetry.Default.RecordSSE(time.Since(start))
			recordCommitToBrowser(hint)
			watermark = hint.Seq
		}
	}
}

func (s *Server) drainJournal(ctx context.Context, rc *http.ResponseController, w http.ResponseWriter, flusher http.Flusher, watermark *int64, allowEvent func(string) bool) bool {
	for {
		entries, err := s.store.ReadJournalEvents(ctx, realtimeEpoch, *watermark, replayBatch)
		if err != nil {
			return false
		}
		for _, entry := range entries {
			start := time.Now()
			if allowEvent != nil && !allowEvent(entry.EventType) {
				*watermark = entry.Seq
				continue
			}
			if err := writeJournalEntry(rc, w, flusher, entry.Epoch, entry.Seq, entry.EventType, entry.Payload); err != nil {
				return false
			}
			telemetry.Default.RecordSSE(time.Since(start))
			*watermark = entry.Seq
		}
		if len(entries) < replayBatch {
			return true
		}
	}
}

func recordCommitToBrowser(event eventhub.Event) {
	if event.EventType != "bank.transaction.credit" || event.CommittedAt == "" {
		return
	}
	committedAt, err := time.Parse(time.RFC3339Nano, event.CommittedAt)
	if err != nil {
		return
	}
	d := time.Since(committedAt)
	if d >= 0 {
		telemetry.Default.RecordCommitToBrowserSSE(d)
	}
}

func writeJournalEntry(rc *http.ResponseController, w http.ResponseWriter, flusher http.Flusher, epoch string, seq int64, eventType string, payload []byte) error {
	data := string(payload)
	if eventType == "bank.transaction.credit" {
		var raw map[string]any
		if err := json.Unmarshal(payload, &raw); err != nil {
			return fmt.Errorf("sanitize credit event: %w", err)
		}
		delete(raw, "balance")
		delete(raw, "accountNumber")
		delete(raw, "sessionToken")
		safe, err := json.Marshal(raw)
		if err != nil {
			return fmt.Errorf("sanitize credit event: %w", err)
		}
		data = string(safe)
	}
	return writeSSE(rc, w, flusher, fmt.Sprintf("%s:%d", epoch, seq), eventType, data)
}

func writeSSE(rc *http.ResponseController, w http.ResponseWriter, flusher http.Flusher, id, event, data string) error {
	_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	flusher.Flush()
	_ = rc.SetWriteDeadline(time.Time{})
	return nil
}

func writeReset(rc *http.ResponseController, w http.ResponseWriter, flusher http.Flusher, reason string) {
	_ = writeSSE(rc, w, flusher, "", "reset_state", fmt.Sprintf(`{"reason":%q}`, reason))
}

func writeSSEError(rc *http.ResponseController, w http.ResponseWriter, flusher http.Flusher, reason string) {
	_ = writeSSE(rc, w, flusher, "", "stream_error", fmt.Sprintf(`{"reason":%q}`, reason))
}
