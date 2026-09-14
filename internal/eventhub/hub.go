package eventhub

import (
	"sync"
	"sync/atomic"
)

type Event struct {
	Seq         int64  `json:"seq"`
	Epoch       string `json:"epoch"`
	EventType   string `json:"eventType"`
	AggregateID string `json:"aggregateId"`
	Payload     []byte `json:"payload"`
	CreatedAt   string `json:"createdAt"`
	CommittedAt string `json:"committedAt,omitempty"`
}

type Hub struct {
	mu          sync.Mutex
	subscribers map[uint64]chan Event
	nextID      uint64
	dropped     atomic.Uint64
}

func New() *Hub {
	return &Hub{
		subscribers: make(map[uint64]chan Event),
	}
}

// Subscribe returns an event channel and a cancel function to deregister.
func (h *Hub) Subscribe() (uint64, <-chan Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.nextID++
	id := h.nextID
	ch := make(chan Event, 128)
	h.subscribers[id] = ch

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if c, ok := h.subscribers[id]; ok {
				delete(h.subscribers, id)
				close(c)
			}
		})
	}

	return id, ch, cancel
}

// Publish broadcasts an event without blocking publishers. A subscriber that
// cannot keep up is disconnected so it can recover from its durable cursor.
func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for id, ch := range h.subscribers {
		select {
		case ch <- e:
		default:
			delete(h.subscribers, id)
			close(ch)
			h.dropped.Add(1)
		}
	}
}

func (h *Hub) DroppedNotifications() uint64 {
	return h.dropped.Load()
}

// SubscriberCount returns current active subscriber count.
func (h *Hub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers)
}
