package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authsession"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
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

func (v *SessionVerifier) VerifySession(ctx context.Context, connectionID string, generation int64, encrypted []byte) (verified []byte, resultErr error) {
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
		envelope       []byte
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
	if v == nil || v.sessions == nil || v.client == nil || v.sessions.store == nil || v.sessions.keyring == nil || v.sessions.restorer == nil {
		return nil, &authsession.VerificationError{Code: "VERIFICATION_UNAVAILABLE"}
	}

	execFn := func(stepCtx context.Context) error {
		if err := ctx.Err(); err != nil {
			return verificationFailure(err)
		}
		stepCtx, cancel := context.WithCancel(stepCtx)
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		defer cancel()
		var refreshed []byte
		response, err := sessionOperation(stepCtx, v.sessions.store, v.sessions, nil, connectionID, generation, true, func() (acb.Response, error) {
			// Restore, bank navigation, and snapshot are one operation. Another
			// candidate must not replace the state whose account we just verified.
			v.sessions.mu.Lock()
			restoreErr := v.sessions.restoreLocked(connectionID, generation, encrypted)
			v.sessions.mu.Unlock()
			if restoreErr != nil {
				return acb.Response{}, verificationFailure(restoreErr)
			}
			result.Lock()
			result.phase = "BOOTSTRAP"
			result.Unlock()
			expectedAccount := v.client.SessionAccountNumber()
			response, err := v.client.Bootstrap(stepCtx)
			if err != nil {
				return response, verificationFailure(err)
			}
			if err := checkSessionFence(stepCtx, v.sessions.store, connectionID, generation, true); err != nil {
				return acb.Response{}, verificationFailure(err)
			}
			switch response.Kind {
			case acb.AccountDetailPage, acb.HistoryPage:
				if expectedAccount != "" {
					result.Lock()
					result.phase = "ACCOUNT"
					result.Unlock()
					form, err := acb.ExtractHistoryForm(response.Body)
					if err != nil {
						return response, &authsession.VerificationError{Code: "VERIFICATION_FORM_INVALID"}
					}
					account := form.Fields["AccountNbr"]
					result.Lock()
					result.formValid, result.accountPresent, result.accountMatch = true, account != "", account == expectedAccount
					result.Unlock()
					if account == "" {
						// Only parsed history from a direct exact-account POST proves
						// selection without an account echo. GET/detail pages do not.
						if response.Kind != acb.HistoryPage || response.StatusCode != http.StatusOK || response.RequestedAccount != expectedAccount {
							return response, &authsession.VerificationError{Code: "VERIFICATION_ACCOUNT_MISSING"}
						}
						if _, err := acb.ParseHistoryPage(response.Body); err != nil {
							return response, &authsession.VerificationError{Code: "VERIFICATION_FORM_INVALID"}
						}
					} else if account != expectedAccount {
						return response, &authsession.VerificationError{Code: "VERIFICATION_ACCOUNT_MISMATCH"}
					}
				}
			case acb.LoginPage, acb.OTPChallenge, acb.CaptchaPage:
				return response, &authsession.VerificationError{Code: "VERIFICATION_AUTH_REQUIRED"}
			case acb.MaintenancePage:
				return response, &authsession.VerificationError{Code: "VERIFICATION_MAINTENANCE"}
			default:
				return response, &authsession.VerificationError{Code: "VERIFICATION_PAGE_UNSUPPORTED"}
			}
			handoff, err := v.client.SnapshotSession()
			if err != nil {
				return response, verificationFailure(err)
			}
			plaintext, err := authbrowser.EncodeHandoff(handoff, []byte("verified"))
			if err != nil {
				return response, verificationFailure(err)
			}
			envelope, err := v.sessions.keyring.Encrypt([]byte(plaintext), security.SessionAAD(connectionID, generation))
			if err != nil {
				return response, verificationFailure(err)
			}
			refreshed, err = json.Marshal(envelope)
			return response, err
		})
		if response.StatusCode != 0 {
			result.Lock()
			result.hasResponse, result.status, result.kind = true, response.StatusCode, response.Kind
			result.Unlock()
		}
		if err != nil {
			clear(refreshed)
			if authsession.VerificationCode(err) != "" {
				return err
			}
			return verificationFailure(err)
		}
		result.Lock()
		defer result.Unlock()
		if err := ctx.Err(); err != nil {
			clear(refreshed)
			return verificationFailure(err)
		}
		result.phase, result.envelope = "COMPLETE", refreshed
		return nil
	}
	finish := func(err error) ([]byte, error) {
		result.Lock()
		defer result.Unlock()
		if err != nil {
			clear(result.envelope)
			result.envelope = nil
			return nil, err
		}
		envelope := result.envelope
		result.envelope = nil
		return envelope, nil
	}

	if v.scheduler == nil || !v.scheduler.IsRunning() {
		return finish(execFn(ctx))
	}

	task := &verifyTask{
		id:         fmt.Sprintf("verify_%s_%d_%d", connectionID, generation, time.Now().UnixNano()),
		generation: generation,
		stepFn:     execFn,
		done:       make(chan error, 1),
	}

	if err := v.scheduler.Enqueue(task); err != nil {
		return nil, verificationFailure(err)
	}

	select {
	case <-ctx.Done():
		v.scheduler.CancelTask(task.ID())
		return finish(verificationFailure(ctx.Err()))
	case err := <-task.done:
		return finish(err)
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
