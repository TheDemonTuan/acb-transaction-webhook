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

type SessionVerifier struct {
	sessions  *SessionLoader
	client    *acb.Client
	scheduler *scheduler.Scheduler
}

func NewSessionVerifier(sessions *SessionLoader, client *acb.Client, sched ...*scheduler.Scheduler) *SessionVerifier {
	v := &SessionVerifier{sessions: sessions, client: client}
	if len(sched) > 0 && sched[0] != nil {
		v.scheduler = sched[0]
	}
	return v
}

type verifyTask struct {
	id         string
	generation int64
	stepFn     func(ctx context.Context) error
	done       chan error
}

func (t *verifyTask) ID() string                 { return t.id }
func (t *verifyTask) Kind() string               { return "INTERACTIVE_VERIFY" }
func (t *verifyTask) Priority() UpstreamPriority { return PriorityInteractiveVerify }
func (t *verifyTask) Generation() int64          { return t.generation }

func (t *verifyTask) Step(ctx context.Context) (scheduler.TaskStepResult, error) {
	err := t.stepFn(ctx)
	select {
	case t.done <- err:
	default:
	}
	if err != nil {
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeAuth}, err
	}
	return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
}

func (v *SessionVerifier) VerifySession(ctx context.Context, connectionID string, generation int64, encrypted []byte) error {
	if v == nil || v.sessions == nil || v.client == nil {
		return errors.New("ACB session verifier is unavailable")
	}

	execFn := func(stepCtx context.Context) error {
		if err := v.sessions.RestoreEnvelope(connectionID, generation, encrypted); err != nil {
			return err
		}
		response, err := v.client.Bootstrap(stepCtx)
		if err != nil {
			return err
		}
		slog.Info("ACB session bootstrap verified", "kind", response.Kind, "classifier_reason", response.ClassifierReason, "status", response.StatusCode, "path", acb.SafePath(response.URL))
		switch response.Kind {
		case acb.AccountDetailPage, acb.HistoryPage:
			return nil
		case acb.LoginPage, acb.OTPChallenge, acb.CaptchaPage:
			return errors.New("ACB authentication was not preserved")
		case acb.MaintenancePage:
			return errors.New("ACB is under maintenance")
		default:
			return errors.New("ACB authenticated page is not recognized")
		}
	}

	if v.scheduler == nil || !v.scheduler.IsRunning() {
		return execFn(ctx)
	}

	task := &verifyTask{
		id:         fmt.Sprintf("verify_%s_%d_%d", connectionID, generation, time.Now().UnixNano()),
		generation: generation,
		stepFn:     execFn,
		done:       make(chan error, 1),
	}

	if err := v.scheduler.Enqueue(task); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-task.done:
		return err
	}
}
