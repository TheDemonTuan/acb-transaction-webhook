package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
)

// UpstreamPriority defines task execution priority on the ACB upstream worker.
// Order: auth > realtime > catch-up > history > keepalive.
type UpstreamPriority int

const (
	PriorityAuth       UpstreamPriority = 100 // Interactive verify / auth challenges
	PriorityRealtime   UpstreamPriority = 80  // Realtime adaptive poll
	PriorityManualSync UpstreamPriority = 70  // Operator manual immediate refresh
	PriorityCatchUp    UpstreamPriority = 50  // Gap scan following downtime or startup
	PriorityHistory    UpstreamPriority = 30  // Durable background historical range sync
	PriorityKeepalive  UpstreamPriority = 10  // Idle keepalive session touch
)

// Compatibility aliases matching COMPATIBILITY_CONTRACTS.md and specifications.
const (
	PriorityInteractiveVerify = PriorityAuth
	PriorityRealtimePoll      = PriorityRealtime
	PriorityFilterHistory     = PriorityHistory
)

// StepOutcome classifies the outcome of an upstream step.
type StepOutcome string

const (
	OutcomeSuccess   StepOutcome = "SUCCESS"
	OutcomeTransient StepOutcome = "TRANSIENT"
	OutcomeAuth      StepOutcome = "AUTH"
	OutcomeFatal     StepOutcome = "FATAL"
)

// TaskStepResult represents the outcome of executing a single upstream quantum.
type TaskStepResult struct {
	Done      bool
	RequeueAt time.Time
	Error     error
	Outcome   StepOutcome
}

// UpstreamTask represents a schedulable unit of upstream work.
type UpstreamTask interface {
	ID() string
	Kind() string
	Priority() UpstreamPriority
	Generation() int64
	Step(ctx context.Context) (TaskStepResult, error)
}

// CoalescingTask allows a task to specify an explicit deduplication key.
type CoalescingTask interface {
	UpstreamTask
	CoalesceKey() string
}

// Standard typed scheduler errors.
var (
	ErrQueueFull        = errors.New("scheduler: queue capacity exceeded")
	ErrSchedulerStopped = errors.New("scheduler: scheduler stopped")
	ErrTaskCanceled     = errors.New("scheduler: task canceled")
)

// TransientError signals a retriable error (e.g. network timeout, 429, maintenance).
type TransientError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *TransientError) Error() string {
	if e == nil || e.Err == nil {
		return "transient error"
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("transient error (retry after %v): %v", e.RetryAfter, e.Err)
	}
	return fmt.Sprintf("transient error: %v", e.Err)
}

func (e *TransientError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *TransientError) Outcome() StepOutcome {
	return OutcomeTransient
}

// AuthError signals an authentication or authorization failure (session expired, challenge).
type AuthError struct {
	Err  error
	Kind acb.PageKind
}

func (e *AuthError) Error() string {
	if e == nil || e.Err == nil {
		return "auth error"
	}
	return fmt.Sprintf("auth error (%s): %v", e.Kind, e.Err)
}

func (e *AuthError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *AuthError) Outcome() StepOutcome {
	return OutcomeAuth
}

// FatalError signals an unrecoverable error that must abort the task without retry.
type FatalError struct {
	Err error
}

func (e *FatalError) Error() string {
	if e == nil || e.Err == nil {
		return "fatal error"
	}
	return fmt.Sprintf("fatal error: %v", e.Err)
}

func (e *FatalError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *FatalError) Outcome() StepOutcome {
	return OutcomeFatal
}

// ClassifyOutcome derives StepOutcome from an error and optional ACB response.
func ClassifyOutcome(err error, resp *acb.Response) StepOutcome {
	if err != nil {
		var aErr *AuthError
		if errors.As(err, &aErr) {
			return OutcomeAuth
		}
		var fErr *FatalError
		if errors.As(err, &fErr) {
			return OutcomeFatal
		}
		var tErr *TransientError
		if errors.As(err, &tErr) {
			return OutcomeTransient
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return OutcomeTransient
		}
		return OutcomeTransient
	}

	if resp != nil {
		switch resp.Kind {
		case acb.LoginPage, acb.OTPChallenge, acb.CaptchaPage:
			return OutcomeAuth
		case acb.MaintenancePage:
			return OutcomeTransient
		case acb.UnknownPage:
			return OutcomeFatal
		case acb.AccountDetailPage, acb.HistoryPage:
			return OutcomeSuccess
		}
	}
	return OutcomeSuccess
}
