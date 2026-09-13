package httpapi

import (
	"context"
	"log/slog"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
)

func (s *Server) Publish(event eventhub.Event) {
	if s.eventHub != nil {
		s.eventHub.Publish(event)
	}
}

// RunJournalWatcher polls SQLite event_journal for new events written by other processes (e.g. acb-worker)
// and fans them out to local SSE clients via eventHub.
func (s *Server) RunJournalWatcher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 1 * time.Second
	}
	lastSeq, err := s.store.GetMaxJournalSeq(ctx, realtimeEpoch)
	if err != nil {
		slog.Warn("failed to initialize journal watcher max seq", "error", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				maxSeq, err := s.store.GetMaxJournalSeq(ctx, realtimeEpoch)
				if err != nil || maxSeq <= lastSeq {
					break
				}
				entries, err := s.store.ReadJournalEvents(ctx, realtimeEpoch, lastSeq, 100)
				if err != nil || len(entries) == 0 {
					break
				}
				for _, entry := range entries {
					s.Publish(eventhub.Event{
						Seq:         entry.Seq,
						Epoch:       entry.Epoch,
						EventType:   entry.EventType,
						AggregateID: entry.AggregateID,
						Payload:     entry.Payload,
						CreatedAt:   entry.CreatedAt,
					})
					if entry.Seq > lastSeq {
						lastSeq = entry.Seq
					}
				}
				if len(entries) < 100 {
					break
				}
			}
		}
	}
}

func (s *Server) RunJournalRetention(ctx context.Context, retention time.Duration) {
	if retention <= 0 {
		retention = 24 * time.Hour
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if _, err := s.store.DeleteJournalBefore(ctx, now.Add(-retention)); err != nil {
				slog.Warn("event journal retention failed", "error", err)
			}
		}
	}
}
