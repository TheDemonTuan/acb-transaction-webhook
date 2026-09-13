package scheduler

import (
	"sync"
	"time"
)

// MetricsCollector defines the interface for recording scheduler telemetry.
type MetricsCollector interface {
	OnTaskEnqueued(kind string, priority UpstreamPriority)
	OnTaskStarted(kind string, priority UpstreamPriority)
	OnTaskCompleted(kind string, priority UpstreamPriority, duration time.Duration, outcome StepOutcome, err error)
	OnTaskYielded(kind string, priority UpstreamPriority, duration time.Duration)
	OnQueueOverloaded(kind string, priority UpstreamPriority)
}

// DefaultMetrics is an in-memory thread-safe metrics collector for observability and inspection.
type DefaultMetrics struct {
	mu           sync.RWMutex
	Enqueued     map[string]int64
	Started      map[string]int64
	Completed    map[string]int64
	Yielded      map[string]int64
	Overloaded   map[string]int64
	LastDuration map[string]time.Duration
}

func NewDefaultMetrics() *DefaultMetrics {
	return &DefaultMetrics{
		Enqueued:     make(map[string]int64),
		Started:      make(map[string]int64),
		Completed:    make(map[string]int64),
		Yielded:      make(map[string]int64),
		Overloaded:   make(map[string]int64),
		LastDuration: make(map[string]time.Duration),
	}
}

func (m *DefaultMetrics) OnTaskEnqueued(kind string, _ UpstreamPriority) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.Enqueued[kind]++
	m.mu.Unlock()
}

func (m *DefaultMetrics) OnTaskStarted(kind string, _ UpstreamPriority) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.Started[kind]++
	m.mu.Unlock()
}

func (m *DefaultMetrics) OnTaskCompleted(kind string, _ UpstreamPriority, duration time.Duration, _ StepOutcome, _ error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.Completed[kind]++
	m.LastDuration[kind] = duration
	m.mu.Unlock()
}

func (m *DefaultMetrics) OnTaskYielded(kind string, _ UpstreamPriority, duration time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.Yielded[kind]++
	m.LastDuration[kind] = duration
	m.mu.Unlock()
}

func (m *DefaultMetrics) OnQueueOverloaded(kind string, _ UpstreamPriority) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.Overloaded[kind]++
	m.mu.Unlock()
}

func (m *DefaultMetrics) Snapshot() (completed, yielded map[string]int64) {
	if m == nil {
		return nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	c := make(map[string]int64, len(m.Completed))
	for k, v := range m.Completed {
		c[k] = v
	}
	y := make(map[string]int64, len(m.Yielded))
	for k, v := range m.Yielded {
		y[k] = v
	}
	return c, y
}
