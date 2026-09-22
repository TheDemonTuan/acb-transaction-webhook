package monitor

import (
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
)

// UpstreamPriority aliases the canonical scheduler priority type.
type UpstreamPriority = scheduler.UpstreamPriority

// Upstream priority levels matching the canonical DAG order:
// auth > realtime > catch-up > history > keepalive.
const (
	PriorityInteractiveVerify    = scheduler.PriorityInteractiveVerify
	PriorityRealtimePoll         = scheduler.PriorityRealtimePoll
	PriorityManualSync           = scheduler.PriorityManualSync
	PriorityRealtimeContinuation = scheduler.PriorityRealtimeContinuation
	PriorityCatchUp              = scheduler.PriorityCatchUp
	PriorityFilterHistory        = scheduler.PriorityFilterHistory
	PriorityKeepalive            = scheduler.PriorityKeepalive
)

// TaskStepResult aliases the scheduler's quantum execution result.
type TaskStepResult = scheduler.TaskStepResult

// UpstreamTask aliases the scheduler's task interface.
type UpstreamTask = scheduler.UpstreamTask

// StepOutcome aliases the typed outcome classification.
type StepOutcome = scheduler.StepOutcome

const (
	OutcomeSuccess   = scheduler.OutcomeSuccess
	OutcomeTransient = scheduler.OutcomeTransient
	OutcomeAuth      = scheduler.OutcomeAuth
	OutcomeFatal     = scheduler.OutcomeFatal
)
