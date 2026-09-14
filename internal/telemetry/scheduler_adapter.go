package telemetry

import (
	"fmt"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
)

// SchedulerAdapter bridges scheduler.MetricsCollector events to the telemetry Registry.
type SchedulerAdapter struct {
	reg *Registry
}

func NewSchedulerAdapter(reg *Registry) *SchedulerAdapter {
	if reg == nil {
		reg = Default
	}
	return &SchedulerAdapter{reg: reg}
}

func priorityString(p scheduler.UpstreamPriority) string {
	switch p {
	case scheduler.PriorityAuth:
		return "INTERACTIVE_VERIFY"
	case scheduler.PriorityRealtime:
		return "REALTIME_POLL"
	case scheduler.PriorityManualSync:
		return "MANUAL_SYNC"
	case scheduler.PriorityCatchUp:
		return "CATCH_UP"
	case scheduler.PriorityHistory:
		return "FILTER_HISTORY"
	case scheduler.PriorityKeepalive:
		return "KEEPALIVE"
	default:
		return fmt.Sprintf("PRIORITY_%d", int(p))
	}
}

func (a *SchedulerAdapter) OnTaskEnqueued(kind string, priority scheduler.UpstreamPriority) {
	if a != nil && a.reg != nil {
		a.reg.RecordTaskEnqueued(kind, priorityString(priority))
	}
}

func (a *SchedulerAdapter) OnTaskStarted(kind string, priority scheduler.UpstreamPriority) {
	if a != nil && a.reg != nil {
		a.reg.RecordTaskStarted(kind, priorityString(priority))
	}
}

func (a *SchedulerAdapter) OnTaskCompleted(kind string, priority scheduler.UpstreamPriority, duration time.Duration, outcome scheduler.StepOutcome, err error) {
	if a != nil && a.reg != nil {
		a.reg.RecordTaskCompleted(kind, priorityString(priority), duration, string(outcome), err)
	}
}

func (a *SchedulerAdapter) OnTaskYielded(kind string, priority scheduler.UpstreamPriority, duration time.Duration) {
	if a != nil && a.reg != nil {
		a.reg.RecordTaskYielded(kind, priorityString(priority), duration)
	}
}

func (a *SchedulerAdapter) OnQueueOverloaded(kind string, priority scheduler.UpstreamPriority) {
	if a != nil && a.reg != nil {
		a.reg.RecordQueueOverloaded(kind, priorityString(priority))
	}
}
