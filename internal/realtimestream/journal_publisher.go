package realtimestream

import (
	"context"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

const (
	journalEpoch        = "ep1"
	journalBatchSize    = 250
	journalRecoveryTick = time.Second
)

// JournalPublisher streams persisted journal entries from SQLite to the worker eventhub.Hub.
type JournalPublisher struct {
	store  *storage.Store
	hub    *eventhub.Hub
	cursor int64
	wakeCh chan struct{}
	mu     sync.Mutex
	ready  bool
}

// NewJournalPublisher creates a new JournalPublisher.
func NewJournalPublisher(store *storage.Store, hub *eventhub.Hub) *JournalPublisher {
	return &JournalPublisher{
		store:  store,
		hub:    hub,
		wakeCh: make(chan struct{}, 1),
	}
}

// Init captures the max journal seq of ep1 before worker readiness and traffic.
// If Init returns an error, the worker must not report readiness.
func (p *JournalPublisher) Init(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	maxSeq, err := p.store.GetMaxJournalSeq(ctx, journalEpoch)
	if err != nil {
		return err
	}
	p.cursor = maxSeq
	p.ready = true
	return nil
}

// Wake signals the publisher runner to check for newly committed journal rows.
func (p *JournalPublisher) Wake() {
	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

// Run executes the journal reader loop until lifecycle ctx is canceled.
func (p *JournalPublisher) Run(ctx context.Context) {
	ticker := time.NewTicker(journalRecoveryTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wakeCh:
			p.process(ctx)
		case <-ticker.C:
			p.process(ctx)
		}
	}
}

func (p *JournalPublisher) process(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	p.mu.Lock()
	cursor := p.cursor
	p.mu.Unlock()

	// Retention adjustment: if cursor is older than retention window, jump to minSeq - 1
	minSeq, err := p.store.GetMinJournalSeq(ctx, journalEpoch)
	if err == nil && minSeq > 0 && cursor < minSeq-1 {
		cursor = minSeq - 1
		p.mu.Lock()
		p.cursor = cursor
		p.mu.Unlock()
	}

	entries, err := p.store.ReadJournalEvents(ctx, journalEpoch, cursor, journalBatchSize)
	if err != nil {
		// Keep cursor and retry on next tick or hint
		return
	}
	if len(entries) == 0 {
		return
	}

	for _, entry := range entries {
		p.hub.Publish(eventhub.Event{
			Seq:         entry.Seq,
			Epoch:       entry.Epoch,
			EventType:   entry.EventType,
			AggregateID: entry.AggregateID,
			Payload:     entry.Payload,
			CreatedAt:   entry.CreatedAt,
		})
		p.mu.Lock()
		if entry.Seq > p.cursor {
			p.cursor = entry.Seq
		}
		p.mu.Unlock()
	}

	// If a full batch was read, self-enqueue to continue draining remaining work
	if len(entries) >= journalBatchSize {
		p.Wake()
	}
}

// Cursor returns the current journal cursor sequence.
func (p *JournalPublisher) Cursor() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cursor
}
