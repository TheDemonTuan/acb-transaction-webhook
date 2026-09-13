package notification

import (
	"context"
	"sync"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type Outcome string

const (
	ProviderWebhook = "WEBHOOK"
	ProviderBark    = "BARK"

	OutcomeSuccess         Outcome = "SUCCESS"
	OutcomeRetry           Outcome = "RETRY"
	OutcomeTerminalFailure Outcome = "TERMINAL_FAILURE"
)

type SendRequest struct {
	DeliveryID    string
	EventID       string
	EventType     string
	Target        storage.DeliveryTarget
	EventPayload  []byte
	AttemptNumber int
}

type SendResult struct {
	Outcome           Outcome
	StatusCode        int
	LatencyMs         int
	ProviderErrorCode string
	SanitizedError    string
}

type Sender interface {
	Send(ctx context.Context, req SendRequest) SendResult
}

type Registry struct {
	mu      sync.RWMutex
	senders map[string]Sender
}

func NewRegistry() *Registry {
	return &Registry{
		senders: make(map[string]Sender),
	}
}

func (r *Registry) Register(provider string, s Sender) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.senders[provider] = s
}

func (r *Registry) Get(provider string) (Sender, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.senders[provider]
	return s, ok
}
