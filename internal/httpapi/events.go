package httpapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/eventhub"
)

func (s *Server) publishStateEvent(eventType, aggregateID string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	seq, err := s.store.AppendJournalEvent(ctx, realtimeEpoch, eventType, aggregateID, payload)
	if err != nil {
		return
	}
	s.Publish(eventhub.Event{Seq: seq, Epoch: realtimeEpoch, EventType: eventType, AggregateID: aggregateID, Payload: payload, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
}
