package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// CatchUpTask executes a preemptible multi-day history scan following startup or downtime.
// Invariant: Step processes an entire day atomically up to catchUpMaxPages per quantum,
// then yields between days to allow higher-priority realtime polls to run before subsequent days.
const (
	catchUpMaxDays  = 7
	catchUpMaxPages = 20
)

type CatchUpTask struct {
	m            *Monitor
	id           string
	connectionID string
	generation   int64
	reason       string
	runID        string

	recoveryReady    bool
	explicitRecovery bool
	recoveryPlan     storage.RecoveryRunPlan
	initialized      bool
	fromDate         string
	toDate           string
	currentDay       time.Time

	dayPageCount int
	nextAction   string
	nextFields   map[string]string
	dayTxns      []storage.BatchTransactionItem
	cursor       *acb.PaginationCursor
	currentResp  acb.Response
	poll         storage.PollRun
	done         chan error
}

// NewRecoveryCatchUpTask creates the recovery-only catch-up task.
func NewRecoveryCatchUpTask(m *Monitor, connectionID string, generation int64, reason, runID string) *CatchUpTask {
	task := newCatchUpTask(m, connectionID, generation, reason, runID)
	task.explicitRecovery = true
	return task
}

func newCatchUpTask(m *Monitor, connectionID string, generation int64, reason, runID string) *CatchUpTask {
	return &CatchUpTask{
		m:            m,
		id:           fmt.Sprintf("catchup_%d_%d", generation, time.Now().UnixNano()),
		connectionID: connectionID,
		generation:   generation,
		reason:       reason,
		runID:        runID,
	}
}

func (t *CatchUpTask) ID() string                 { return t.id }
func (t *CatchUpTask) Kind() string               { return "CATCH_UP" }
func (t *CatchUpTask) Priority() UpstreamPriority { return PriorityCatchUp }
func (t *CatchUpTask) Generation() int64          { return t.generation }
func (t *CatchUpTask) CoalesceKey() string {
	if t.runID != "" {
		return fmt.Sprintf("CATCH_UP:%s", t.runID)
	}
	return fmt.Sprintf("CATCH_UP:%s:%d", t.connectionID, t.generation)
}

func (t *CatchUpTask) history(ctx context.Context, endpoint string, fields map[string]string, date string) (acb.Response, error) {
	if client, ok := t.m.client.(realtimeDateBankClient); ok {
		return client.HistoryForDate(ctx, endpoint, fields, date)
	}
	return t.m.client.History(ctx, endpoint, fields)
}

func (t *CatchUpTask) Step(ctx context.Context) (scheduler.TaskStepResult, error) {
	if err := ctx.Err(); err != nil {
		t.finishDone(err)
		return scheduler.TaskStepResult{Done: false, Error: err, Outcome: scheduler.OutcomeTransient}, err
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
		slog.Info("stale catch-up task discarded due to generation mismatch",
			"task_gen", t.generation, "current_gen", conn.Generation,
			"reason", t.reason, "run_id", t.runID)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	if t.explicitRecovery && !t.recoveryReady {
		if t.runID == "" {
			claimErr := errors.New("recovery catch-up requires a run ID")
			t.finishDone(claimErr)
			return scheduler.TaskStepResult{Done: true, Error: claimErr, Outcome: scheduler.OutcomeFatal}, claimErr
		}
		claimedRun, err := t.m.store.ClaimRecoveryRun(ctx, t.runID, conn.ID, conn.Generation)
		if err != nil {

			if errors.Is(err, storage.ErrGenerationFenceMismatch) || errors.Is(err, storage.ErrRecoveryRunTerminal) {
				t.finishDone(nil)
				return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
			}
			claimErr := fmt.Errorf("claim recovery catch-up run %s: %w", t.runID, err)
			t.finishDone(claimErr)
			return scheduler.TaskStepResult{Done: true, Error: claimErr, Outcome: scheduler.OutcomeFatal}, claimErr
		}
		t.recoveryReady = true
		t.recoveryPlan = storage.RecoveryRunPlan{Reason: claimedRun.Reason, RangeFrom: claimedRun.RangeFrom, RangeTo: claimedRun.RangeTo, NextDay: claimedRun.NextDay}
		if err := t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusRunning, "", ""); err != nil {

			t.finishDone(err)
			return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
		}
	}

	if t.m.IsBackoffActive() {
		return scheduler.TaskStepResult{
			Done:      false,
			RequeueAt: t.m.BackoffUntil(),
			Outcome:   scheduler.OutcomeTransient,
		}, nil
	}

	hasActiveAttempt, err := t.m.store.HasActiveAuthAttempt(ctx, conn.ID)
	if err != nil {
		checkErr := fmt.Errorf("check active auth attempt: %w", err)
		t.finishDone(checkErr)
		return scheduler.TaskStepResult{Done: true, Error: checkErr, Outcome: scheduler.OutcomeFatal}, checkErr
	}
	if hasActiveAttempt {
		slog.Info("deferring catch-up: browser authentication in progress", "connection_id", conn.ID)
		return scheduler.TaskStepResult{
			Done:      false,
			RequeueAt: t.m.now().Add(5 * time.Second),
			Outcome:   scheduler.OutcomeTransient,
		}, nil
	}

	if !t.initialized {
		nowInLoc := t.m.now().In(acb.DefaultLocation)
		today := nowInLoc.Format("2006-01-02")
		fromDate := nowInLoc.AddDate(0, 0, -1).Format("2006-01-02")
		if t.explicitRecovery && t.recoveryPlan.RangeFrom != "" && t.recoveryPlan.RangeTo != "" {
			fromDate = t.recoveryPlan.NextDay
			if fromDate == "" {
				fromDate = t.recoveryPlan.RangeFrom
			}
			today = t.recoveryPlan.RangeTo
		}

		var cp *storage.Checkpoint
		var err error
		if !t.explicitRecovery || t.recoveryPlan.RangeFrom == "" || t.recoveryPlan.RangeTo == "" {
			cp, err = t.m.store.GetCheckpoint(ctx, conn.ID)
			if err != nil {
				checkpointErr := fmt.Errorf("load catch-up checkpoint: %w", err)
				t.finishDone(checkpointErr)
				return scheduler.TaskStepResult{Done: true, Error: checkpointErr, Outcome: scheduler.OutcomeFatal}, checkpointErr
			}
		}
		if cp != nil {
			var checkpointFrom, checkpointTo time.Time
			if cp.CoverageFrom != "" {
				checkpointFrom, err = time.Parse("2006-01-02", cp.CoverageFrom)
				if err != nil {
					checkpointErr := fmt.Errorf("invalid catch-up checkpoint start date %q: %w", cp.CoverageFrom, err)
					t.finishDone(checkpointErr)
					return scheduler.TaskStepResult{Done: true, Error: checkpointErr, Outcome: scheduler.OutcomeFatal}, checkpointErr
				}
			}
			if cp.CoverageTo != "" {
				checkpointTo, err = time.Parse("2006-01-02", cp.CoverageTo)
				if err != nil {
					checkpointErr := fmt.Errorf("invalid catch-up checkpoint end date %q: %w", cp.CoverageTo, err)
					t.finishDone(checkpointErr)
					return scheduler.TaskStepResult{Done: true, Error: checkpointErr, Outcome: scheduler.OutcomeFatal}, checkpointErr
				}
				if cp.CoverageFrom != "" && checkpointFrom.After(checkpointTo) {
					checkpointErr := fmt.Errorf("invalid catch-up checkpoint range %q..%q", cp.CoverageFrom, cp.CoverageTo)
					t.finishDone(checkpointErr)
					return scheduler.TaskStepResult{Done: true, Error: checkpointErr, Outcome: scheduler.OutcomeFatal}, checkpointErr
				}
				fromDate = cp.CoverageTo
			}
		}

		fromT, err := time.Parse("2006-01-02", fromDate)
		if err != nil {
			checkpointErr := fmt.Errorf("invalid catch-up checkpoint date %q: %w", fromDate, err)
			t.finishDone(checkpointErr)
			return scheduler.TaskStepResult{Done: true, Error: checkpointErr, Outcome: scheduler.OutcomeFatal}, checkpointErr
		}
		toT, err := time.Parse("2006-01-02", today)
		if err != nil {
			dateErr := fmt.Errorf("parse catch-up today %q: %w", today, err)
			t.finishDone(dateErr)
			return scheduler.TaskStepResult{Done: true, Error: dateErr, Outcome: scheduler.OutcomeFatal}, dateErr
		}

		// Seven inclusive calendar days: today-6 through today.
		oldest := toT.AddDate(0, 0, -(catchUpMaxDays - 1))
		if fromT.Before(oldest) {
			fromT = oldest
		}
		if fromT.After(toT) {
			fromT = toT
		}
		t.currentDay = fromT
		t.fromDate = fromT.Format("2006-01-02")
		t.toDate = toT.Format("2006-01-02")
		if t.explicitRecovery && t.runID != "" {
			plan := storage.RecoveryRunPlan{Reason: t.reason, RangeFrom: t.fromDate, RangeTo: t.toDate, NextDay: t.fromDate}
			if t.recoveryPlan.Reason != "" {
				plan.Reason = t.recoveryPlan.Reason
			}
			if t.recoveryPlan.RangeFrom == "" || t.recoveryPlan.RangeTo == "" || t.recoveryPlan.NextDay == "" {
				persisted, planErr := t.m.store.SetRecoveryRunPlan(ctx, t.runID, conn.ID, conn.Generation, plan)
				if planErr != nil {
					t.finishDone(planErr)
					return scheduler.TaskStepResult{Done: true, Error: planErr, Outcome: scheduler.OutcomeFatal}, planErr
				}
				t.recoveryPlan = storage.RecoveryRunPlan{Reason: persisted.Reason, RangeFrom: persisted.RangeFrom, RangeTo: persisted.RangeTo, NextDay: persisted.NextDay}
			}
		}
		t.initialized = true
		slog.Info("initialized recovery catch-up task", "from", t.fromDate, "to", t.toDate,
			"reason", t.reason, "run_id", t.runID, "generation", conn.Generation)
	}

	toT, err := time.Parse("2006-01-02", t.toDate)
	if err != nil {
		dateErr := fmt.Errorf("invalid catch-up end date %q: %w", t.toDate, err)
		t.finishDone(dateErr)
		return scheduler.TaskStepResult{Done: true, Error: dateErr, Outcome: scheduler.OutcomeFatal}, dateErr
	}

	// Skip covered historical days before today.
	for !t.currentDay.After(toT) && t.currentDay.Before(toT) && t.dayPageCount == 0 {
		dayStr := t.currentDay.Format("2006-01-02")
		covered, coverageErr := t.m.store.CheckRangeCoverage(ctx, conn.ID, dayStr, dayStr)
		if coverageErr != nil {
			coverageErr = fmt.Errorf("check catch-up coverage for %s: %w", dayStr, coverageErr)
			t.finishDone(coverageErr)
			return scheduler.TaskStepResult{Done: true, Error: coverageErr, Outcome: scheduler.OutcomeFatal}, coverageErr
		}
		if !covered {
			break
		}
		if t.explicitRecovery && t.runID != "" {
			nextDay := t.currentDay.AddDate(0, 0, 1).Format("2006-01-02")
			if _, err := t.m.store.AdvanceRecoveryDay(ctx, storage.RecoveryDayAdvance{RunID: t.runID, ConnectionID: conn.ID, Generation: conn.Generation, Day: dayStr, NextDay: nextDay, ScanID: t.scanID(), CoverageFrom: t.fromDate}); err != nil {
				checkpointErr := fmt.Errorf("advance covered recovery day %s: %w", dayStr, err)
				t.finishDone(checkpointErr)
				return scheduler.TaskStepResult{Done: true, Error: checkpointErr, Outcome: scheduler.OutcomeFatal}, checkpointErr
			}
		} else if err := t.m.store.SaveCheckpoint(ctx, storage.Checkpoint{
			ConnectionID: conn.ID,
			ScanID:       t.scanID(),
			CoverageFrom: t.fromDate,
			CoverageTo:   dayStr,
		}); err != nil {
			checkpointErr := fmt.Errorf("save catch-up checkpoint for covered day %s: %w", dayStr, err)

			t.finishDone(checkpointErr)
			return scheduler.TaskStepResult{Done: true, Error: checkpointErr, Outcome: scheduler.OutcomeFatal}, checkpointErr
		}
		t.currentDay = t.currentDay.AddDate(0, 0, 1)
		if err := t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusRunning, "", ""); err != nil {
			t.finishDone(err)
			return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
		}
	}

	// Check if entire catch-up range is completed
	if t.currentDay.After(toT) {
		if s := t.m.SessionLoader(); s != nil {
			if err := s.Persist(ctx, conn.ID, conn.Generation); err != nil {
				persistErr := fmt.Errorf("persist session after catch-up: %w", err)
				_ = t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusFailed, "SESSION_PERSIST_FAILED", persistErr.Error())
				t.finishDone(persistErr)
				return scheduler.TaskStepResult{Done: true, Error: persistErr, Outcome: scheduler.OutcomeFatal}, persistErr
			}
		}
		if err := t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusCompleted, "", ""); err != nil {
			t.finishDone(err)
			return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
		}
		slog.Info("catch-up task completed successfully", "from", t.fromDate, "to", t.toDate,
			"reason", t.reason, "run_id", t.runID)

		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	dayStr := t.currentDay.Format("2006-01-02")

	// If starting a fresh day, bootstrap if needed to get form tokens
	if t.dayPageCount == 0 && t.currentResp.Body == "" {
		if s := t.m.SessionLoader(); s != nil {
			if err := s.Restore(ctx, conn.ID, conn.Generation); err != nil {
				authErr := fmt.Errorf("restore recovery session: %w", err)
				_ = t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusFailed, "SESSION_RESTORE_FAILED", authErr.Error())
				t.finishDone(authErr)
				return scheduler.TaskStepResult{Done: true, Error: authErr, Outcome: scheduler.OutcomeAuth}, authErr
			}
		}
		resp, err := t.m.client.Bootstrap(ctx)
			if err != nil {
				var authFail *acb.AuthFailure
				if errors.As(err, &authFail) {
					return t.authRequired(ctx, conn, authFail, resp, "bootstrap")
				}
				until := t.m.RecordNetworkFailure(err)
				slog.Warn("ACB request failed", "phase", "catchup_bootstrap", "generation", conn.Generation, "backoff_until", until, "error", acb.SanitizeTransportError(err))
				return scheduler.TaskStepResult{
					Done:      false,
					RequeueAt: until,
					Outcome:   scheduler.OutcomeTransient,
					Error:     err,
				}, nil
			}
			if resp.Kind == acb.LoginPage || resp.Kind == acb.OTPChallenge || resp.Kind == acb.CaptchaPage {
				inconclusiveErr := errors.New("unconfirmed login-like response during catchup bootstrap")
				slog.Warn("unconfirmed login-like response during catchup bootstrap; keeping MONITORING", "generation", conn.Generation)
				t.finishDone(inconclusiveErr)
				return scheduler.TaskStepResult{Done: true, Error: inconclusiveErr, Outcome: scheduler.OutcomeTransient}, inconclusiveErr
			}

			if resp.StatusCode == 429 {
				t.m.SetBackoff(60 * time.Second)
				return scheduler.TaskStepResult{
					Done:      false,
					RequeueAt: t.m.BackoffUntil(),
					Outcome:   scheduler.OutcomeTransient,
				}, nil
			}
			if resp.Kind == acb.MaintenancePage {
				t.m.SetBackoff(60 * time.Second)
				return scheduler.TaskStepResult{
					Done:      false,
					RequeueAt: t.m.BackoffUntil(),
					Outcome:   scheduler.OutcomeTransient,
				}, nil
			}
			t.m.ClearBackoff()
			t.currentResp = resp
		}

		// Prepare form navigation for this day if page 0
		if t.dayPageCount == 0 {
			form, formErr := acb.ExtractHistoryForm(t.currentResp.Body)
			if formErr != nil {
				formErr = fmt.Errorf("extract recovery catch-up form for %s: %w", dayStr, formErr)
				_ = t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusFailed, "FORM_INVALID", formErr.Error())
				t.finishDone(formErr)
				return scheduler.TaskStepResult{Done: true, Error: formErr, Outcome: scheduler.OutcomeFatal}, formErr
			}
			if form.Fields["AccountNbr"] == "" && conn.AccountMasked != "" {
				form.Fields["AccountNbr"] = conn.AccountMasked
			}
			fromT, _ := time.Parse("2006-01-02", dayStr)
			form.Fields["FromDate"] = fromT.Format("02/01/2006")
			form.Fields["ToDate"] = fromT.Format("02/01/2006")
			form.Fields["_explicitRange"] = "true"
			t.nextAction = form.Action
			t.nextFields = form.Fields
		}

		var dayResets int
		for t.dayPageCount < catchUpMaxPages {
			if err := ctx.Err(); err != nil {
				t.finishDone(err)
				return scheduler.TaskStepResult{Done: false, Error: err, Outcome: scheduler.OutcomeTransient}, err
			}

			histResp, histErr := t.history(ctx, t.nextAction, t.nextFields, t.currentDay.Format("02/01/2006"))
			if histErr != nil {
				var authFail *acb.AuthFailure
				if errors.As(histErr, &authFail) {
					return t.authRequired(ctx, conn, authFail, histResp, "history")
				}
				if errors.Is(histErr, acb.ErrConversationReset) {
					if dayResets == 0 {
						dayResets++
						slog.Info("conversational state reset in catch-up; restarting day from page 1", "day", dayStr)
						t.dayPageCount = 0
						t.dayTxns = nil
						t.cursor = nil
						bootResp, bootErr := t.m.client.Bootstrap(ctx)
						if bootErr != nil {
							var bAuthFail *acb.AuthFailure
							if errors.As(bootErr, &bAuthFail) {
								return t.authRequired(ctx, conn, bAuthFail, bootResp, "bootstrap_resync")
							}
							return scheduler.TaskStepResult{Done: false, Error: bootErr, Outcome: scheduler.OutcomeTransient}, bootErr
						}
						t.currentResp = bootResp
						form, formErr := acb.ExtractHistoryForm(bootResp.Body)
						if formErr != nil {
							return scheduler.TaskStepResult{Done: true, Error: formErr, Outcome: scheduler.OutcomeFatal}, formErr
						}
						if form.Fields["AccountNbr"] == "" && conn.AccountMasked != "" {
							form.Fields["AccountNbr"] = conn.AccountMasked
						}
						fromT, _ := time.Parse("2006-01-02", dayStr)
						form.Fields["FromDate"] = fromT.Format("02/01/2006")
						form.Fields["ToDate"] = fromT.Format("02/01/2006")
						form.Fields["_explicitRange"] = "true"
						t.nextAction = form.Action
						t.nextFields = form.Fields
						continue
					}
					resetErr := fmt.Errorf("conversational token rejected repeatedly for %s: %w", dayStr, histErr)
					t.finishDone(resetErr)
					return scheduler.TaskStepResult{Done: true, Error: resetErr, Outcome: scheduler.OutcomeTransient}, resetErr
				}
				until := t.m.RecordNetworkFailure(histErr)
				slog.Warn("ACB request failed", "phase", "catchup_history", "generation", conn.Generation, "backoff_until", until, "error", acb.SanitizeTransportError(histErr))
				return scheduler.TaskStepResult{
					Done:      false,
					RequeueAt: until,
					Outcome:   scheduler.OutcomeTransient,
					Error:     histErr,
				}, nil
			}
			if histResp.StatusCode == 429 || histResp.Kind == acb.MaintenancePage {
				t.m.SetBackoff(60 * time.Second)
				return scheduler.TaskStepResult{
					Done:      false,
					RequeueAt: t.m.BackoffUntil(),
					Outcome:   scheduler.OutcomeTransient,
				}, nil
			}
			if histResp.Kind == acb.LoginPage || histResp.Kind == acb.OTPChallenge || histResp.Kind == acb.CaptchaPage {
				inconclusiveErr := errors.New("unconfirmed login-like response in catchup history")
				slog.Warn("unconfirmed login-like response in catchup history; keeping MONITORING", "day", dayStr, "kind", histResp.Kind)
				t.finishDone(inconclusiveErr)
				return scheduler.TaskStepResult{Done: true, Error: inconclusiveErr, Outcome: scheduler.OutcomeTransient}, inconclusiveErr
			}

			t.dayPageCount++
			if _, err := acb.ExtractHistoryForm(histResp.Body); err != nil {
				t.currentResp = acb.Response{}
			} else {
				t.currentResp = histResp
			}

			t.m.ClearBackoff()
			pageResult, parseErr := acb.ParseHistoryPage(histResp.Body)
			if parseErr != nil {
				parseErr = fmt.Errorf("parse recovery catch-up history for %s: %w", dayStr, parseErr)
				_ = t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusFailed, "PARSE_FAILED", parseErr.Error())
				t.finishDone(parseErr)
				return scheduler.TaskStepResult{Done: true, Error: parseErr, Outcome: scheduler.OutcomeFatal}, parseErr
			}

			var pageItems []storage.BatchTransactionItem
			for _, txn := range pageResult.Transactions {
				pageItems = append(pageItems, storage.BatchTransactionItem{
					Number:        txn.Number,
					Credit:        txn.Credit,
					Debit:         txn.Debit,
					Balance:       txn.Balance,
					TransactionAt: txn.TransactionAt,
					EffectiveAt:   txn.EffectiveDate,
					Description:   txn.Description,
				})
			}

			res, ingestErr := t.m.store.IngestTransactionsBatchWithSource(ctx, conn.ID, conn.Generation, conn.AccountMasked, pageItems, false, "CATCH_UP")
			if ingestErr != nil {
				if errors.Is(ingestErr, storage.ErrGenerationFenceMismatch) {
					t.finishDone(nil)
					return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
				}
				ingestErr = fmt.Errorf("ingest recovery catch-up page for %s: %w", dayStr, ingestErr)
				_ = t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusFailed, "INGEST_FAILED", ingestErr.Error())
				t.finishDone(ingestErr)
				return scheduler.TaskStepResult{Done: true, Error: ingestErr, Outcome: scheduler.OutcomeFatal}, ingestErr
			}
			t.m.notifyNewEvents(res.NewEvents)
			t.dayTxns = append(t.dayTxns, pageItems...)

			if t.cursor == nil {
				t.cursor = acb.NewPaginationCursor(t.nextAction, t.nextFields)
			}
			t.cursor.Step(pageResult, len(pageResult.Transactions))

			if !t.cursor.HasNext {
				break
			}

			if t.cursor.HasNext && (t.cursor.Action != "" || len(t.cursor.Fields) > 0) && t.dayPageCount < catchUpMaxPages {
				day := t.currentDay.Format("02/01/2006")
				pinned, err := acb.PinDateRangePreservingPagination(t.cursor.Fields, day, day)
				if err != nil {
					pinErr := fmt.Errorf("pin recovery pagination for %s: %w", dayStr, err)
					if progressErr := t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusFailed, "FORM_INVALID", pinErr.Error()); progressErr != nil {
						pinErr = errors.Join(pinErr, progressErr)
					}
					t.finishDone(pinErr)
					return scheduler.TaskStepResult{Done: true, Error: pinErr, Outcome: scheduler.OutcomeFatal}, pinErr
				}
					pinned["_raw"] = "true"
					t.nextAction = t.cursor.Action
					t.nextFields = pinned
			}
		}

		// Fail closed on deterministic pagination truncation. Do not requeue forever.
		if t.cursor != nil && (t.cursor.Truncated || (t.cursor.HasNext && t.dayPageCount >= catchUpMaxPages)) {
			truncErr := fmt.Errorf("catch-up pagination truncated for day %s: seen %d of %d rows (pages: %d)",
				dayStr, t.cursor.CumulativeRows, t.cursor.TotalRowsSeen, t.dayPageCount)
			slog.Warn("catch-up day truncated, failing closed without advancing coverage", "day", dayStr,
				"reason", t.reason, "run_id", t.runID, "error", truncErr)
			if progressErr := t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusFailed, "TRUNCATED_HISTORY", truncErr.Error()); progressErr != nil {
				truncErr = errors.Join(truncErr, progressErr)
			}
			t.finishDone(truncErr)
			return scheduler.TaskStepResult{Done: true, Error: truncErr, Outcome: scheduler.OutcomeFatal}, truncErr
		}

		// Day is complete: commit coverage, checkpoint, and recovery cursor atomically.
	nextDay := t.currentDay.AddDate(0, 0, 1).Format("2006-01-02")
	if t.explicitRecovery && t.runID != "" {
		if _, err := t.m.store.CommitRecoveryDay(ctx, storage.RecoveryDayCommit{RunID: t.runID, ConnectionID: conn.ID, Generation: conn.Generation, Day: dayStr, NextDay: nextDay, RowsSeen: len(t.dayTxns), ScanID: t.scanID(), CoverageFrom: t.fromDate}); err != nil {
			commitErr := fmt.Errorf("commit catch-up day %s: %w", dayStr, err)
			_ = t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusFailed, "DAY_COMMIT_FAILED", commitErr.Error())
			t.finishDone(commitErr)
			return scheduler.TaskStepResult{Done: true, Error: commitErr, Outcome: scheduler.OutcomeFatal}, commitErr
		}
	} else {
		if err := t.m.store.RecordCoveragePerDay(ctx, conn.ID, map[string]int{dayStr: len(t.dayTxns)}); err != nil {
			coverageErr := fmt.Errorf("record catch-up coverage for %s: %w", dayStr, err)
			t.finishDone(coverageErr)
			return scheduler.TaskStepResult{Done: true, Error: coverageErr, Outcome: scheduler.OutcomeFatal}, coverageErr
		}
		if err := t.m.store.SaveCheckpoint(ctx, storage.Checkpoint{ConnectionID: conn.ID, ScanID: t.scanID(), CoverageFrom: t.fromDate, CoverageTo: dayStr}); err != nil {
			checkpointErr := fmt.Errorf("save catch-up checkpoint for %s: %w", dayStr, err)
			t.finishDone(checkpointErr)
			return scheduler.TaskStepResult{Done: true, Error: checkpointErr, Outcome: scheduler.OutcomeFatal}, checkpointErr
		}
	}

	// Reset day-level state and advance to next day
	t.dayPageCount = 0
	t.dayTxns = nil
	t.nextAction = ""
	t.nextFields = nil
	t.cursor = nil
	t.currentDay = t.currentDay.AddDate(0, 0, 1)

	if t.currentDay.After(toT) {
		if s := t.m.SessionLoader(); s != nil {
			if err := s.Persist(ctx, conn.ID, conn.Generation); err != nil {
				persistErr := fmt.Errorf("persist session after catch-up: %w", err)
				_ = t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusFailed, "SESSION_PERSIST_FAILED", persistErr.Error())
				t.finishDone(persistErr)
				return scheduler.TaskStepResult{Done: true, Error: persistErr, Outcome: scheduler.OutcomeFatal}, persistErr
			}
		}
		if err := t.updateRecoveryProgress(ctx, conn, storage.RecoveryRunStatusCompleted, "", ""); err != nil {
			t.finishDone(err)
			return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
		}
		slog.Info("catch-up task finished all days", "from", t.fromDate, "to", t.toDate,
			"reason", t.reason, "run_id", t.runID)

		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	// Yield between days
	return scheduler.TaskStepResult{Done: false, Outcome: scheduler.OutcomeSuccess}, nil
}

func (t *CatchUpTask) scanID() string {
	if t.runID != "" {
		return t.runID
	}
	return t.id
}

func (t *CatchUpTask) updateRecoveryProgress(ctx context.Context, conn storage.Connection, status storage.RecoveryRunStatus, code, message string) error {
	if !t.explicitRecovery || t.runID == "" {
		return nil
	}
	progress := map[string]string{
		"from":        t.fromDate,
		"to":          t.toDate,
		"current_day": t.currentDay.Format("2006-01-02"),
	}
	encoded, err := json.Marshal(progress)
	if err != nil {
		return fmt.Errorf("encode recovery catch-up progress: %w", err)
	}
	if _, err := t.m.store.UpdateRecoveryRunProgress(ctx, t.runID, conn.ID, conn.Generation, status, string(encoded), code, message); err != nil {
		if errors.Is(err, storage.ErrGenerationFenceMismatch) {
			return nil
		}
		return fmt.Errorf("update recovery catch-up progress: %w", err)
	}
	return nil
}

func (t *CatchUpTask) authRequired(ctx context.Context, conn storage.Connection, authFail *acb.AuthFailure, resp acb.Response, phase string) (scheduler.TaskStepResult, error) {
	authErr := fmt.Errorf("session expired during recovery catch-up (%s)", phase)
	if authFail != nil {
		authErr = fmt.Errorf("session expired during recovery catch-up (%s): %w", phase, authFail)
	}
	if t.explicitRecovery && t.runID != "" {
		progress := map[string]string{"phase": phase, "reason": t.reason}
		progressJSON, err := json.Marshal(progress)
		if err != nil {
			finishErr := errors.Join(authErr, fmt.Errorf("encode recovery auth progress: %w", err))
			t.finishDone(finishErr)
			return scheduler.TaskStepResult{Done: true, Error: finishErr, Outcome: scheduler.OutcomeFatal}, finishErr
		}
		if _, err := t.m.store.UpdateRecoveryRunProgress(ctx, t.runID, conn.ID, conn.Generation,
			storage.RecoveryRunStatusFailed, string(progressJSON), "AUTH_REQUIRED", authErr.Error()); err != nil {
			if errors.Is(err, storage.ErrGenerationFenceMismatch) {
				t.finishDone(nil)
				return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
			}
			finishErr := errors.Join(authErr, fmt.Errorf("record recovery auth loss: %w", err))
			t.finishDone(finishErr)
			return scheduler.TaskStepResult{Done: true, Error: finishErr, Outcome: scheduler.OutcomeFatal}, finishErr
		}
	}
	if t.poll.ID == "" {
		poll, err := t.m.store.StartPoll(ctx)
		if err != nil {
			finishErr := errors.Join(authErr, fmt.Errorf("start recovery auth poll: %w", err))
			t.finishDone(finishErr)
			return scheduler.TaskStepResult{Done: true, Error: finishErr, Outcome: scheduler.OutcomeFatal}, finishErr
		}
		t.poll = poll
	}
	if t.poll.ID != "" {
		t.poll.Status = "AUTH_REQUIRED"
		t.poll.AuthConfirmed = true
		if authFail != nil {
			t.poll.Classifier = string(authFail.Kind)
			t.poll.Error = authFail.Reason
		} else {
			t.poll.Classifier = string(resp.Kind)
			t.poll.Error = "SESSION_EXPIRED"
		}
		t.poll.HTTPStatus = resp.StatusCode
		if err := t.m.finishPoll(ctx, t.poll, 0); err != nil {
			if errors.Is(err, storage.ErrGenerationFenceMismatch) {
				t.finishDone(nil)
				return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
			}
			finishErr := errors.Join(authErr, fmt.Errorf("finish recovery auth poll: %w", err))
			t.finishDone(finishErr)
			return scheduler.TaskStepResult{Done: true, Error: finishErr, Outcome: scheduler.OutcomeFatal}, finishErr
		}
	}
	slog.Warn("ACB session expired during recovery catch-up", "phase", phase, "generation", conn.Generation,
		"reason", t.reason, "run_id", t.runID, "status", resp.StatusCode,
		"classifier_reason", resp.ClassifierReason, "path", acb.SafePath(resp.URL))
	t.finishDone(authErr)
	return scheduler.TaskStepResult{Done: true, Error: authErr, Outcome: scheduler.OutcomeAuth}, nil
}

func (t *CatchUpTask) finishDone(err error) {
	if t.done != nil {
		select {
		case t.done <- err:
		default:
		}
	}
}
