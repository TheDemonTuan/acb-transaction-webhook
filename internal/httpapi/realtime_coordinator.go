package httpapi

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
)

// RealtimeCoordinator serializes live hints and journal recovery through one
// cursor so gateway publishers cannot race each other.
type RealtimeCoordinator struct {
	server   *Server
	input    chan eventhub.Event
	interval time.Duration

	mu      sync.RWMutex
	lastSeq int64
}

func NewRealtimeCoordinator(server *Server, interval time.Duration) *RealtimeCoordinator {
	if interval <= 0 {
		interval = time.Second
	}
	return &RealtimeCoordinator{
		server:   server,
		input:    make(chan eventhub.Event, 256),
		interval: interval,
	}
}

func (c *RealtimeCoordinator) Input() chan<- eventhub.Event { return c.input }

func (c *RealtimeCoordinator) LastSeq() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSeq
}

func (c *RealtimeCoordinator) Run(ctx context.Context) {
	if c == nil || c.server == nil || c.server.store == nil {
		return
	}
	lastSeq, err := c.server.store.GetMaxJournalSeq(ctx, realtimeEpoch)
	if err != nil {
		slog.Warn("failed to initialize realtime coordinator", "error", err)
		lastSeq = 0
	}
	c.setLastSeq(lastSeq)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-c.input:
			c.handle(ctx, event)
		case <-ticker.C:
			c.reconcile(ctx, 0)
		}
	}
}

func (c *RealtimeCoordinator) handle(ctx context.Context, event eventhub.Event) {
	lastSeq := c.LastSeq()
	if event.Epoch != realtimeEpoch || event.Seq <= lastSeq {
		return
	}
	if event.Seq == lastSeq+1 {
		c.server.eventHub.Publish(event)
		c.setLastSeq(event.Seq)
		return
	}
	c.reconcile(ctx, event.Seq)
}

// reconcile publishes durable journal rows in order up to target. A zero target
// snapshots the current high-water mark.
func (c *RealtimeCoordinator) reconcile(ctx context.Context, target int64) {
	if target <= 0 {
		var err error
		target, err = c.server.store.GetMaxJournalSeq(ctx, realtimeEpoch)
		if err != nil {
			slog.Warn("realtime recovery watermark failed", "error", err)
			return
		}
	}
	for c.LastSeq() < target {
		entries, err := c.server.store.ReadJournalEvents(ctx, realtimeEpoch, c.LastSeq(), replayBatch)
		if err != nil {
			slog.Warn("realtime recovery read failed", "error", err)
			return
		}
		if len(entries) == 0 {
			return
		}
		for _, entry := range entries {
			if entry.Seq > target {
				return
			}
			c.server.eventHub.Publish(eventhub.Event{
				Seq:         entry.Seq,
				Epoch:       entry.Epoch,
				EventType:   entry.EventType,
				AggregateID: entry.AggregateID,
				Payload:     entry.Payload,
				CreatedAt:   entry.CreatedAt,
			})
			c.setLastSeq(entry.Seq)
		}
		if len(entries) < replayBatch {
			return
		}
	}
}

func (c *RealtimeCoordinator) setLastSeq(seq int64) {
	c.mu.Lock()
	c.lastSeq = seq
	c.mu.Unlock()
}
