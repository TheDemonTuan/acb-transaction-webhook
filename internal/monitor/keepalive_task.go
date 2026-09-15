package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
)

// KeepaliveTask periodically touches the ACB session using Bootstrap only (never History)
// to prevent session timeout and rotate session cookies during idle periods.
type KeepaliveTask struct {
	m            *Monitor
	id           string
	connectionID string
	generation   int64
	done         chan error
}

func NewKeepaliveTask(m *Monitor, connectionID string, generation int64) *KeepaliveTask {
	return &KeepaliveTask{
		m:            m,
		id:           fmt.Sprintf("keepalive_%d_%d", generation, time.Now().UnixNano()),
		connectionID: connectionID,
		generation:   generation,
	}
}

func (t *KeepaliveTask) ID() string                 { return t.id }
func (t *KeepaliveTask) Kind() string               { return "KEEPALIVE" }
func (t *KeepaliveTask) Priority() UpstreamPriority { return PriorityKeepalive }
func (t *KeepaliveTask) Generation() int64          { return t.generation }
func (t *KeepaliveTask) CoalesceKey() string        { return "KEEPALIVE" }

func (t *KeepaliveTask) Step(ctx context.Context) (scheduler.TaskStepResult, error) {
	if err := ctx.Err(); err != nil {
		t.finishDone(err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeTransient}, err
	}

	conn, err := t.m.store.Connection(ctx)
	if err != nil {
		t.finishDone(err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
	}
	if conn.State != "MONITORING" {
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	if t.generation > 0 && (conn.ID != t.connectionID || conn.Generation != t.generation) {
		slog.Info("stale keepalive task discarded due to generation mismatch",
			"task_gen", t.generation, "current_gen", conn.Generation)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	if t.m.IsBackoffActive() {
		return scheduler.TaskStepResult{Done: false, RequeueAt: t.m.BackoffUntil(), Outcome: scheduler.OutcomeTransient}, nil
	}

	hasActiveAttempt, err := t.m.store.HasActiveAuthAttempt(ctx, conn.ID)
	if err != nil {
		checkErr := fmt.Errorf("check active auth attempt: %w", err)
		t.finishDone(checkErr)
		return scheduler.TaskStepResult{Done: true, Error: checkErr, Outcome: scheduler.OutcomeFatal}, checkErr
	}
	if hasActiveAttempt {
		slog.Info("skipping keepalive: browser authentication in progress", "connection_id", conn.ID)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	if t.m.client == nil {
		clientErr := errors.New("bank client not configured")
		t.finishDone(clientErr)
		return scheduler.TaskStepResult{Done: true, Error: clientErr, Outcome: scheduler.OutcomeFatal}, clientErr
	}
	if s := t.m.SessionLoader(); s != nil {
		if err := s.Restore(ctx, conn.ID, conn.Generation); err != nil {
			restoreErr := fmt.Errorf("restore ACB session: %w", err)
			t.finishDone(restoreErr)
			return scheduler.TaskStepResult{Done: true, Error: restoreErr, Outcome: scheduler.OutcomeAuth}, restoreErr
		}
	}

	poll, err := t.m.store.StartPoll(ctx)
	if err != nil {
		startErr := fmt.Errorf("start poll: %w", err)
		t.finishDone(startErr)
		return scheduler.TaskStepResult{Done: true, Error: startErr, Outcome: scheduler.OutcomeFatal}, startErr
	}

	// Bootstrap page only to keep session alive and rotate cookies - NEVER calls History!
	resp, err := t.m.client.Bootstrap(ctx)
	if err != nil {
		poll.Status = "FAILED"
		poll.Error = acb.SanitizeTransportError(err)
		if finishErr := t.m.finishPoll(ctx, poll, 0); finishErr != nil {
			err = errors.Join(err, finishErr)
		}
		until := t.m.RecordNetworkFailure(err)
		slog.Warn("ACB request failed", "phase", "keepalive", "generation", conn.Generation, "backoff_until", until, "error", poll.Error)
		t.finishDone(err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeTransient}, err
	}

	poll.Classifier = string(resp.Kind)
	poll.HTTPStatus = resp.StatusCode

	if resp.Kind == acb.LoginPage || resp.Kind == acb.OTPChallenge || resp.Kind == acb.CaptchaPage {
		poll.Status = "AUTH_REQUIRED"
		poll.Error = "SESSION_EXPIRED"
		_ = t.m.finishPoll(ctx, poll, 0)
		slog.Warn("ACB session expired during keepalive; transitioned to AUTH_REQUIRED", "generation", conn.Generation, "status", resp.StatusCode, "classifier_reason", resp.ClassifierReason, "path", acb.SafePath(resp.URL))
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeAuth}, nil
	}

	if resp.StatusCode == 429 {
		poll.Status = "FAILED"
		poll.Error = "ACB_RATE_LIMITED"
		t.m.SetBackoff(60 * time.Second)
		_ = t.m.finishPoll(ctx, poll, 0)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeTransient}, nil
	}

	if resp.Kind == acb.MaintenancePage {
		poll.Status = "FAILED"
		poll.Error = "ACB_MAINTENANCE"
		t.m.SetBackoff(60 * time.Second)
		_ = t.m.finishPoll(ctx, poll, 0)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeTransient}, nil
	}

	if s := t.m.SessionLoader(); s != nil {
		if err := s.Persist(ctx, conn.ID, conn.Generation); err != nil {
			slog.Warn("could not persist refreshed ACB session", "generation", conn.Generation, "error", err)
		}
	}

	t.m.ClearBackoff()
	poll.Status = "SUCCEEDED"
	poll.Pages = 0
	poll.RowsSeen = 0
	finishErr := t.m.finishPoll(ctx, poll, 0)
	t.finishDone(finishErr)
	return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, finishErr
}

func (t *KeepaliveTask) finishDone(err error) {
	if t.done != nil {
		select {
		case t.done <- err:
		default:
		}
	}
}
