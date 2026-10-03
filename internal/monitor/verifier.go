package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authsession"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
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

func (v *SessionVerifier) InvalidateSession(ctx context.Context, connectionID string, generation int64) error {
	if v == nil || v.sessions == nil {
		return errors.New("ACB session verifier is unavailable")
	}
	return v.sessions.InvalidateSession(ctx, connectionID, generation)
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
		outcome := scheduler.ClassifyOutcome(err, nil)
		switch authsession.VerificationCode(err) {
		case "VERIFICATION_AUTH_REQUIRED":
			outcome = scheduler.OutcomeAuth
		case "VERIFICATION_ACCOUNT_MISMATCH", "VERIFICATION_ACCOUNT_MISSING", "VERIFICATION_FORM_INVALID", "VERIFICATION_PAGE_UNSUPPORTED":
			outcome = scheduler.OutcomeFatal
		case "VERIFICATION_MAINTENANCE", "VERIFICATION_UNAVAILABLE", "VERIFICATION_TIMEOUT":
			outcome = scheduler.OutcomeTransient
		}
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: outcome}, err
	}
	return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
}

func (v *SessionVerifier) VerifySession(ctx context.Context, connectionID string, generation int64, encrypted []byte) (resultErr error) {
	// The scheduler may still finish a canceled task after this call returns.
	// Guard the finite diagnostic fields, and emit only at the caller boundary.
	var result struct {
		sync.Mutex
		phase          string
		hasResponse    bool
		status         int
		kind           acb.PageKind
		formValid      bool
		accountPresent bool
		accountMatch   bool
	}
	result.phase = "RESTORE"
	defer func() {
		code := "VERIFIED"
		if resultErr != nil {
			code = authsession.VerificationCode(resultErr)
			if code == "" {
				code = "VERIFICATION_UNAVAILABLE"
			}
		}
		result.Lock()
		defer result.Unlock()
		if result.hasResponse {
			slog.Info("ACB session verification result", "generation", generation, "phase", result.phase, "code", code, "status", result.status, "kind", result.kind, "form_valid", result.formValid, "account_present", result.accountPresent, "account_match", result.accountMatch)
		} else {
			slog.Info("ACB session verification result", "generation", generation, "phase", result.phase, "code", code)
		}
	}()
	if v == nil || v.sessions == nil || v.client == nil {
		return &authsession.VerificationError{Code: "VERIFICATION_UNAVAILABLE"}
	}

	execFn := func(stepCtx context.Context) error {
		if err := ctx.Err(); err != nil {
			return verificationFailure(err)
		}
		stepCtx, cancel := context.WithCancel(stepCtx)
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		defer cancel()
		if err := v.sessions.RestoreEnvelope(stepCtx, connectionID, generation, encrypted); err != nil {
			return verificationFailure(err)
		}
		result.Lock()
		result.phase = "BOOTSTRAP"
		result.Unlock()
		var expectedAccount string
		response, err := sessionOperation(stepCtx, v.sessions.store, v.sessions, nil, connectionID, generation, true, func() (acb.Response, error) {
			expectedAccount = v.client.SessionAccountNumber()
			return v.client.Bootstrap(stepCtx)
		})
		if response.StatusCode != 0 {
			result.Lock()
			result.hasResponse, result.status, result.kind = true, response.StatusCode, response.Kind
			result.Unlock()
		}
		if err != nil {
			return verificationFailure(err)
		}
		switch response.Kind {
		case acb.AccountDetailPage, acb.HistoryPage:
			if expectedAccount != "" {
				result.Lock()
				result.phase = "ACCOUNT"
				result.Unlock()
				form, err := acb.ExtractHistoryForm(response.Body)
				if err != nil {
					return &authsession.VerificationError{Code: "VERIFICATION_FORM_INVALID"}
				}
				account := form.Fields["AccountNbr"]
				result.Lock()
				result.formValid, result.accountPresent, result.accountMatch = true, account != "", account == expectedAccount
				result.Unlock()
				if account == "" {
					// ACB history can omit the selected account after a direct exact-account
					// query. Do not apply this to account/summary pages or resync GETs.
					if response.Kind != acb.HistoryPage || response.StatusCode != http.StatusOK || response.RequestedAccount != expectedAccount {
						return &authsession.VerificationError{Code: "VERIFICATION_ACCOUNT_MISSING"}
					}
					if _, err := acb.ParseHistoryPage(response.Body); err != nil {
						return &authsession.VerificationError{Code: "VERIFICATION_FORM_INVALID"}
					}
				} else if account != expectedAccount {
					return &authsession.VerificationError{Code: "VERIFICATION_ACCOUNT_MISMATCH"}
				}
			}
			result.Lock()
			result.phase = "COMPLETE"
			result.Unlock()
			return nil
		case acb.LoginPage, acb.OTPChallenge, acb.CaptchaPage:
			return &authsession.VerificationError{Code: "VERIFICATION_AUTH_REQUIRED"}
		case acb.MaintenancePage:
			return &authsession.VerificationError{Code: "VERIFICATION_MAINTENANCE"}
		default:
			return &authsession.VerificationError{Code: "VERIFICATION_PAGE_UNSUPPORTED"}
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
		return verificationFailure(err)
	}

	select {
	case <-ctx.Done():
		v.scheduler.CancelTask(task.ID())
		return verificationFailure(ctx.Err())
	case err := <-task.done:
		return err
	}
}

func verificationFailure(err error) error {
	code := "VERIFICATION_UNAVAILABLE"
	var authFailure *acb.AuthFailure
	switch {
	case errors.Is(err, storage.ErrGenerationFenceMismatch), errors.Is(err, storage.ErrRecoverySuperseded):
		code = "VERIFICATION_SUPERSEDED"
	case errors.As(err, &authFailure):
		code = "VERIFICATION_AUTH_REQUIRED"
	}
	return &authsession.VerificationError{Code: code}
}
