package monitor

import (
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
)

// Scheduler returns the single-owner priority scheduler governing upstream ACB requests.
func (m *Monitor) Scheduler() *scheduler.Scheduler {
	if m == nil {
		return nil
	}
	return m.scheduler
}

// WithScheduler overrides the scheduler instance on the Monitor.
func (m *Monitor) WithScheduler(s *scheduler.Scheduler) *Monitor {
	if m != nil {
		m.scheduler = s
	}
	return m
}
