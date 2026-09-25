package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
)

// RealtimeTask executes a realtime adaptive transaction poll or an operator manual sync.
const realtimeMaxPages = 20

var errRealtimeDateRollover = errors.New("REALTIME_DATE_ROLLOVER")

type RealtimeTask struct {
	m            *Monitor
	id           string
	priority     UpstreamPriority
	connectionID string
	generation   int64
	done         chan error

	// Continuation state stays on the task so the scheduler can yield after each
	// page without restarting today's query at page one.
	started       bool
	today         string
	poll          storage.PollRun
	pages         int
	rowsSeen      int
	totalInserted int
	nextAction    string
	nextFields    map[string]string
	cursor        *acb.PaginationCursor
	continuation  bool
	finished      bool
}

func (t *RealtimeTask) pinToday(fields map[string]string) map[string]string {
	pinned := make(map[string]string, len(fields)+4)
	for key, value := range fields {
		pinned[key] = value
	}
	pinned["FromDate"] = t.today
	pinned["ToDate"] = t.today
	pinned["activeDatetimeYN"] = "N"
	if pinned["dse_nextEventName"] == "" {
		pinned["dse_nextEventName"] = "byDate"
	}
	delete(pinned, "activeDatetimeByMonth")
	delete(pinned, "MonthCurr")
	delete(pinned, "YearCurr")
	delete(pinned, "_explicitRange")
	pinned["_raw"] = "true"
	return pinned
}

func (t *RealtimeTask) todayStillCurrent() bool {
	return t.today != "" && t.m.now().In(acb.DefaultLocation).Format("02/01/2006") == t.today
}

func (t *RealtimeTask) rolloverResult(ctx context.Context) (scheduler.TaskStepResult, error) {
	return t.finishPoll(ctx, "PARTIAL", "REALTIME_DATE_ROLLOVER")
}

func (t *RealtimeTask) bootstrap(ctx context.Context) (acb.Response, error) {
	if !t.todayStillCurrent() {
		return acb.Response{}, errRealtimeDateRollover
	}
	if client, ok := t.m.client.(realtimeDateBankClient); ok {
		return client.BootstrapForDate(ctx, t.today)
	}
	return t.m.client.Bootstrap(ctx)
}

func (t *RealtimeTask) history(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error) {
	if !t.todayStillCurrent() {
		return acb.Response{}, errRealtimeDateRollover
	}
	if client, ok := t.m.client.(realtimeDateBankClient); ok {
		return client.HistoryForDate(ctx, endpoint, fields, t.today)
	}
	return t.m.client.History(ctx, endpoint, fields)
}

func classifyRealtimeResponse(resp *acb.Response) {
	if resp.Kind == acb.UnknownPage && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		resp.Kind = acb.LoginPage
	}
}

func sameRealtimeCursor(action string, fields map[string]string, nextAction string, nextFields map[string]string) bool {
	return action == nextAction && maps.Equal(fields, nextFields)
}

func (t *RealtimeTask) finishPoll(ctx context.Context, status, pollErr string) (scheduler.TaskStepResult, error) {
	if t.finished {
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}
	pages := t.pages
	if pages == 0 && t.started {
		pages = 1
	}
	t.poll.Status = status
	t.poll.Pages = pages
	t.poll.RowsSeen = t.rowsSeen
	if pollErr != "" {
		t.poll.Error = pollErr
	}
	if err := t.m.finishPoll(ctx, t.poll, t.totalInserted); err != nil {
		return scheduler.TaskStepResult{Done: false, RequeueAt: time.Now().Add(time.Second), Error: err, Outcome: scheduler.OutcomeTransient}, err
	}
	t.finished = true
	if status == "AUTH_REQUIRED" {
		t.m.notifyPollWaiters(nil)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeAuth}, nil
	}
	if status == "PARTIAL" {
		t.m.notifyPollWaiters(nil)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeTransient}, nil
	}
	if pollErr != "" {
		terminalErr := errors.New(pollErr)
		t.m.notifyPollWaiters(terminalErr)
		t.finishDone(terminalErr)
		return scheduler.TaskStepResult{Done: true, Error: terminalErr, Outcome: scheduler.OutcomeTransient}, terminalErr
	}
	t.m.notifyPollWaiters(nil)
	t.finishDone(nil)
	return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
}

func NewRealtimeTask(m *Monitor, priority UpstreamPriority, connectionID string, generation int64) *RealtimeTask {
	kind := "REALTIME_POLL"
	if priority == PriorityManualSync {
		kind = "MANUAL_SYNC"
	} else if priority == PriorityRealtimeContinuation {
		kind = "REALTIME_CONTINUATION"
	}
	return &RealtimeTask{
		m:            m,
		id:           fmt.Sprintf("%s_%d_%d", kind, generation, time.Now().UnixNano()),
		priority:     priority,
		connectionID: connectionID,
		generation:   generation,
	}
}

func (t *RealtimeTask) ID() string                 { return t.id }
func (t *RealtimeTask) Priority() UpstreamPriority { return t.priority }
func (t *RealtimeTask) Generation() int64          { return t.generation }

func (t *RealtimeTask) Kind() string {
	if t.priority == PriorityManualSync {
		return "MANUAL_SYNC"
	}
	if t.priority == PriorityRealtimeContinuation {
		return "REALTIME_CONTINUATION"
	}
	return "REALTIME_POLL"
}

func (t *RealtimeTask) CoalesceKey() string {
	return fmt.Sprintf("REALTIME:%s:%d", t.connectionID, t.generation)
}

func (t *RealtimeTask) Step(ctx context.Context) (scheduler.TaskStepResult, error) {
	if err := ctx.Err(); err != nil {
		if t.started && t.poll.ID != "" && !t.finished {
			return t.finishPoll(ctx, "PARTIAL", "REALTIME_CANCELED")
		}
		t.m.clearSyncRequest(t.connectionID, t.generation)
		t.finishDone(err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeTransient}, err
	}

	conn, err := t.m.store.Connection(ctx)
	if err != nil {
		t.m.clearSyncRequest(t.connectionID, t.generation)
		t.finishDone(err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
	}
	if conn.State != "MONITORING" {
		if t.started && t.poll.ID != "" && !t.finished {
			return t.finishPoll(ctx, "PARTIAL", "REALTIME_CONNECTION_NOT_MONITORING")
		}
		t.m.clearSyncRequest(t.connectionID, t.generation)
		t.m.notifyPollWaiters(nil)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	// Generation guard: a task queued for an older or mismatched generation is discarded before any ACB call.
	if t.generation > 0 && (conn.ID != t.connectionID || conn.Generation != t.generation) {
		slog.Info("stale realtime/manual task discarded due to generation mismatch",
			"task_gen", t.generation, "current_gen", conn.Generation, "conn_id", conn.ID)
		if t.started && t.poll.ID != "" && !t.finished {
			return t.finishPoll(ctx, "PARTIAL", "REALTIME_GENERATION_CHANGED")
		}
		t.m.clearSyncRequest(t.connectionID, t.generation)
		t.m.notifyPollWaiters(nil)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	if t.m.IsBackoffActive() {
		until := t.m.BackoffUntil()
		slog.Info("delaying poll: upstream backoff active", "until", until)
		return scheduler.TaskStepResult{Done: false, RequeueAt: until, Outcome: scheduler.OutcomeTransient}, nil
	}

	hasActiveAttempt, err := t.m.store.HasActiveAuthAttempt(ctx, conn.ID)
	if err != nil {
		slog.Error("poll fail-closed: failed to check active auth attempt", "connection_id", conn.ID, "error", err)
		checkErr := fmt.Errorf("check active auth attempt: %w", err)
		t.m.clearSyncRequest(t.connectionID, t.generation)
		t.m.notifyPollWaiters(checkErr)
		t.finishDone(checkErr)
		return scheduler.TaskStepResult{Done: true, Error: checkErr, Outcome: scheduler.OutcomeFatal}, checkErr
	}
	if hasActiveAttempt {
		slog.Info("skipping poll: browser authentication in progress", "connection_id", conn.ID)
		if t.started && t.poll.ID != "" && !t.finished {
			return t.finishPoll(ctx, "PARTIAL", "AUTH_IN_PROGRESS")
		}
		t.m.clearSyncRequest(t.connectionID, t.generation)
		t.m.notifyPollWaiters(nil)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	openRun, err := t.m.openRecoveryRun(ctx, conn.ID, conn.Generation)
	if err != nil {
		slog.Warn("check open recovery in realtime task failed", "connection_id", conn.ID, "error", err)
		return scheduler.TaskStepResult{Done: false, RequeueAt: time.Now().Add(2 * time.Second), Outcome: scheduler.OutcomeTransient}, nil
	}
	if openRun != nil {
		if openRun.Reason == storage.RecoveryReasonInitialAuth {
			slog.Info("suppressing realtime task: initial bootstrap in progress", "connection_id", conn.ID, "generation", conn.Generation)
			if t.started && t.poll.ID != "" && !t.finished {
				return t.finishPoll(ctx, "PARTIAL", "INITIAL_BOOTSTRAP_IN_PROGRESS")
			}
			t.m.clearSyncRequest(t.connectionID, t.generation)
			t.m.notifyPollWaiters(nil)
			t.finishDone(nil)
			return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
		}
		curBoost := t.m.PaymentBoostStatus()
		if !curBoost.Active && t.priority == PriorityRealtimePoll {
			slog.Info("suppressing normal realtime poll: recovery catch-up in progress", "connection_id", conn.ID, "generation", conn.Generation)
			if t.started && t.poll.ID != "" && !t.finished {
				return t.finishPoll(ctx, "PARTIAL", "RECOVERY_IN_PROGRESS")
			}
			t.m.clearSyncRequest(t.connectionID, t.generation)
			t.m.notifyPollWaiters(nil)
			t.finishDone(nil)
			return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
		}
	}

	if t.m.client == nil {
		clientErr := errors.New("bank client not configured")
		t.m.clearSyncRequest(t.connectionID, t.generation)
		t.m.notifyPollWaiters(clientErr)
		t.finishDone(clientErr)
		return scheduler.TaskStepResult{Done: true, Error: clientErr, Outcome: scheduler.OutcomeFatal}, clientErr
	}
	if t.started {
		return t.stepContinuation(ctx, conn)
	}
	t.today = t.m.now().In(acb.DefaultLocation).Format("02/01/2006")
	if s := t.m.SessionLoader(); s != nil {
		if err := s.Restore(ctx, conn.ID, conn.Generation); err != nil {
			restoreErr := fmt.Errorf("restore ACB session: %w", err)
			t.m.clearSyncRequest(t.connectionID, t.generation)
			t.m.notifyPollWaiters(restoreErr)
			t.finishDone(restoreErr)
			return scheduler.TaskStepResult{Done: true, Error: restoreErr, Outcome: scheduler.OutcomeAuth}, restoreErr
		}
	}

	poll, err := t.m.store.StartPoll(ctx)
	if err != nil {
		startErr := fmt.Errorf("start poll: %w", err)
		t.m.clearSyncRequest(t.connectionID, t.generation)
		t.m.notifyPollWaiters(startErr)
		t.finishDone(startErr)
		return scheduler.TaskStepResult{Done: true, Error: startErr, Outcome: scheduler.OutcomeFatal}, startErr
	}
	t.started = true
	t.poll = poll
	t.pages = 0
	t.rowsSeen = 0
	t.totalInserted = 0
	t.continuation = false

	// 1. Fetch account detail page to verify session and extract form state.
	if !t.todayStillCurrent() {
		return t.rolloverResult(ctx)
	}
	slog.Debug("ACB realtime request", "task", t.Kind(), "task_id", t.id, "reason", "REALTIME", "connection_id", conn.ID, "generation", conn.Generation, "from_date", t.today, "to_date", t.today, "page", 1, "phase", "bootstrap")
	resp, err := t.bootstrap(ctx)

	if err != nil {
		var authFail *acb.AuthFailure
		if errors.As(err, &authFail) {
			t.poll.Classifier = string(authFail.Kind)
			t.poll.AuthConfirmed = true
			if s := t.m.SessionLoader(); s != nil {
				_ = s.Persist(ctx, conn.ID, conn.Generation)
			}
			slog.Warn("ACB confirmed the session is no longer authenticated; transitioned to AUTH_REQUIRED", "phase", "bootstrap", "generation", conn.Generation, "status", resp.StatusCode, "classifier_reason", resp.ClassifierReason, "path", acb.SafePath(resp.URL))
			return t.finishPoll(ctx, "AUTH_REQUIRED", authFail.Reason)
		}
		if errors.Is(err, errRealtimeDateRollover) {
			return t.rolloverResult(ctx)
		}
		sanitized := acb.SanitizeTransportError(err)
		until := t.m.RecordNetworkFailure(err)
		slog.Warn("ACB request failed", "phase", "bootstrap", "generation", conn.Generation, "elapsed_backoff_until", until, "error", sanitized)
		return t.finishPoll(ctx, "FAILED", sanitized)
	}

	classifyRealtimeResponse(&resp)
	t.poll.Classifier = string(resp.Kind)
	t.poll.HTTPStatus = resp.StatusCode

	if resp.Kind == acb.LoginPage || resp.Kind == acb.OTPChallenge || resp.Kind == acb.CaptchaPage {
		slog.Warn("unconfirmed login-like page in bootstrap; keeping MONITORING", "phase", "bootstrap", "generation", conn.Generation, "status", resp.StatusCode, "classifier_reason", resp.ClassifierReason, "path", acb.SafePath(resp.URL))
		return t.finishPoll(ctx, "FAILED", "AUTH_INCONCLUSIVE")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		t.m.SetBackoff(60 * time.Second)
		return t.finishPoll(ctx, "FAILED", "ACB_RATE_LIMITED")
	}
	if resp.Kind == acb.MaintenancePage {
		t.m.SetBackoff(60 * time.Second)
		return t.finishPoll(ctx, "FAILED", "ACB_MAINTENANCE")
	}

	form, formErr := acb.ExtractHistoryForm(resp.Body)
	if formErr != nil {
		diag := acb.DiagnosePageStructure(resp.Body)
		slog.Warn("ACB history form extraction failed", "task", t.Kind(), "task_id", t.id, "connection_id", conn.ID, "generation", conn.Generation, "today", t.today, "status", t.poll.HTTPStatus, "diagnostic", diag, "error", formErr)
		return t.finishPoll(ctx, "PARTIAL", formErr.Error())
	}
	historyMarkup := resp.Body
	if form.Fields["AccountNbr"] == "" && conn.AccountMasked != "" {
		form.Fields["AccountNbr"] = conn.AccountMasked
	}
	form.Fields["FromDate"] = t.today
	form.Fields["ToDate"] = t.today
	form.Fields["dse_nextEventName"] = "byDate"
	form.Fields["activeDatetimeYN"] = "N"
	slog.Debug("ACB realtime request", "task", t.Kind(), "task_id", t.id, "reason", "REALTIME", "connection_id", conn.ID, "generation", conn.Generation, "from_date", t.today, "to_date", t.today, "page", 1, "phase", "history")
	histResp, histErr := t.history(ctx, form.Action, form.Fields)

	if histErr != nil {
		var authFail *acb.AuthFailure
		if errors.As(histErr, &authFail) {
			t.poll.Classifier = string(authFail.Kind)
			t.poll.AuthConfirmed = true
			slog.Warn("ACB session expired during history fetch; transitioned to AUTH_REQUIRED", "phase", "history", "generation", conn.Generation, "status", histResp.StatusCode, "classifier_reason", histResp.ClassifierReason, "path", acb.SafePath(histResp.URL))
			return t.finishPoll(ctx, "AUTH_REQUIRED", authFail.Reason)
		}
		if errors.Is(histErr, acb.ErrConversationReset) {
			return t.finishPoll(ctx, "PARTIAL", "CONVERSATION_RESET")
		}
		if errors.Is(histErr, errRealtimeDateRollover) {
			return t.rolloverResult(ctx)
		}
		sanitized := acb.SanitizeTransportError(histErr)
		until := t.m.RecordNetworkFailure(histErr)
		slog.Warn("ACB request failed", "phase", "history", "generation", conn.Generation, "elapsed_backoff_until", until, "error", sanitized)
		return t.finishPoll(ctx, "FAILED", sanitized)
	}
	classifyRealtimeResponse(&histResp)
	historyMarkup = histResp.Body
	t.poll.Classifier = string(histResp.Kind)
	t.poll.HTTPStatus = histResp.StatusCode

	if histResp.Kind == acb.LoginPage || histResp.Kind == acb.OTPChallenge || histResp.Kind == acb.CaptchaPage {
		slog.Warn("unconfirmed login-like page during history fetch; keeping MONITORING", "phase", "history", "generation", conn.Generation, "status", histResp.StatusCode, "classifier_reason", histResp.ClassifierReason, "path", acb.SafePath(histResp.URL))
		return t.finishPoll(ctx, "FAILED", "AUTH_INCONCLUSIVE")
	}
	if histResp.StatusCode == http.StatusTooManyRequests {
		t.m.SetBackoff(60 * time.Second)
		return t.finishPoll(ctx, "FAILED", "ACB_RATE_LIMITED")
	}
	if histResp.Kind == acb.MaintenancePage {
		t.m.SetBackoff(60 * time.Second)
		return t.finishPoll(ctx, "FAILED", "ACB_MAINTENANCE")
	}

	// Any complete non-authenticated response proves the transport recovered.
	t.m.ClearBackoff()

	pagesCount := 1
	t.pages = pagesCount

	// Parse transaction history
	pageResult, parseErr := acb.ParseHistoryPage(historyMarkup)
	if parseErr != nil {
		diag := acb.DiagnosePageStructure(historyMarkup)
		slog.Warn("ACB history parse failed", "phase", "realtime_history_parse", "connection_id", conn.ID, "generation", conn.Generation, "today", t.today, "status", t.poll.HTTPStatus, "diagnostic", diag, "error", parseErr)
		return t.finishPoll(ctx, "PARTIAL", parseErr.Error())
	}

	todayTransactions, err := acb.FilterHistoryTransactionDay(pageResult.Transactions, t.today)
	if err != nil {
		slog.Warn("ACB history day mismatch", "phase", "realtime_history", "requested_day", t.today, "response_days", acb.HistoryDayCounts(pageResult.Transactions), "response_rows", len(pageResult.Transactions), "bootstrap_form_date_matches", form.Fields["FromDate"] == t.today && form.Fields["ToDate"] == t.today, "response_form_date_matches", acb.HistoryFormDateMatches(historyMarkup, t.today), "response_structure", acb.DiagnosePageStructure(historyMarkup))
		return t.finishPoll(ctx, "PARTIAL", err.Error())
	}
	rowsSeen := len(pageResult.Transactions)
	totalInserted := 0
	var pollErr error
	isPartial := false

	ingestAndNotify := func(txns []acb.Transaction) (int, error) {
		batchItems := make([]storage.BatchTransactionItem, len(txns))
		for i, txn := range txns {
			batchItems[i] = storage.BatchTransactionItem{
				Number:        txn.Number,
				Credit:        txn.Credit,
				Debit:         txn.Debit,
				Balance:       txn.Balance,
				TransactionAt: txn.TransactionAt,
				EffectiveAt:   txn.EffectiveDate,
				Description:   txn.Description,
			}
		}

		startIngest := time.Now()
		batchRes, err := t.m.store.IngestTransactionsBatch(ctx, conn.ID, conn.Generation, conn.AccountMasked, batchItems, false)
		telemetry.Default.RecordIngest(time.Since(startIngest))
		telemetry.Default.SetLastACBPollAt(time.Now())
		if err != nil {
			return 0, err
		}
		t.m.notifyNewEvents(batchRes.NewEvents)
		return batchRes.InsertedCount, nil
	}

	failIngest := func(err error) (scheduler.TaskStepResult, error) {
		if errors.Is(err, storage.ErrGenerationFenceMismatch) {
			slog.Warn("ingest rejected by generation fence", "error", err)
		} else {
			slog.Error("batch ingest failed", "error", err)
		}
		status := "FAILED"
		if totalInserted > 0 {
			status = "PARTIAL"
		}
		t.pages = pagesCount
		t.rowsSeen = rowsSeen
		t.totalInserted = totalInserted
		result, finishErr := t.finishPoll(ctx, status, err.Error())
		if finishErr != nil {
			return result, finishErr
		}
		return result, err
	}

	inserted, err := ingestAndNotify(todayTransactions)
	if err != nil {
		return failIngest(err)
	}
	totalInserted += inserted

	// If page has next, fetch up to 5 pages for realtime poll.
	t.cursor = acb.NewPaginationCursor("", nil)
	t.cursor.Step(pageResult, len(pageResult.Transactions))
	curAction := t.cursor.Action
	curFields := t.cursor.Fields
	lastHasNext := t.cursor.HasNext
	if t.cursor.Truncated {
		isPartial = true
		pollErr = errors.New("REALTIME_PAGINATION_TRUNCATED")
	}
	if t.cursor.HasNext {
		if curAction == "" || len(curFields) == 0 || curFields["dse_operationName"] == "" || curFields["dse_processorState"] == "" {
			slog.Warn("realtime poll page indicates next page exists but navigation form is incomplete")
			isPartial = true
			pollErr = errors.New("pagination unavailable: next page exists but navigation action and fields are empty")
		} else {
			for pagesCount < 5 {
				if !t.todayStillCurrent() {
					t.pages = pagesCount
					t.rowsSeen = rowsSeen
					t.totalInserted = totalInserted
					return t.rolloverResult(ctx)
				}
				if curFields == nil {
					curFields = make(map[string]string)
				}
				pinnedFields, pinErr := acb.PinDateRangePreservingPagination(curFields, t.today, t.today)
				if pinErr != nil {
					isPartial = true
					pollErr = pinErr
					break
				}
				pinnedFields["_raw"] = "true"
				slog.Debug("ACB realtime request", "task", t.Kind(), "task_id", t.id, "reason", "REALTIME", "connection_id", conn.ID, "generation", conn.Generation, "from_date", t.today, "to_date", t.today, "page", pagesCount+1, "phase", "foreground_continuation")
				nextResp, nextErr := t.history(ctx, curAction, pinnedFields)
				if nextErr != nil {
					var authFail *acb.AuthFailure
					if errors.As(nextErr, &authFail) {
						t.poll.Classifier = string(authFail.Kind)
						t.poll.AuthConfirmed = true
						return t.finishPoll(ctx, "AUTH_REQUIRED", authFail.Reason)
					}
					if errors.Is(nextErr, acb.ErrConversationReset) {
						isPartial = true
						pollErr = nextErr
						break
					}
					if errors.Is(nextErr, errRealtimeDateRollover) {
						return t.rolloverResult(ctx)
					}
					until := t.m.RecordNetworkFailure(nextErr)
					slog.Warn("realtime poll next page fetch error", "page", pagesCount+1, "backoff_until", until, "error", acb.SanitizeTransportError(nextErr))
					isPartial = true
					pollErr = errors.New(acb.SanitizeTransportError(nextErr))
					break
				}
				classifyRealtimeResponse(&nextResp)
				if nextResp.Kind == acb.LoginPage || nextResp.Kind == acb.OTPChallenge || nextResp.Kind == acb.CaptchaPage {
					isPartial = true
					pollErr = errors.New("AUTH_INCONCLUSIVE")
					break
				}
				if nextResp.StatusCode == http.StatusTooManyRequests {
					t.m.SetBackoff(60 * time.Second)
					return t.finishPoll(ctx, "PARTIAL", "ACB_RATE_LIMITED")
				}
				if nextResp.Kind == acb.MaintenancePage {
					t.m.SetBackoff(60 * time.Second)
					return t.finishPoll(ctx, "PARTIAL", "ACB_MAINTENANCE")
				}
				pagesCount++
				t.pages = pagesCount
				nextPage, err := acb.ParseHistoryPage(nextResp.Body)
				if err != nil {
					slog.Warn("realtime poll next page parse error", "page", pagesCount, "error", err)
					isPartial = true
					pollErr = err
					break
				}
				todayTransactions, err := acb.FilterHistoryTransactionDay(nextPage.Transactions, t.today)
				if err != nil {
					isPartial = true
					pollErr = err
					break
				}
				rowsSeen += len(nextPage.Transactions)
				nextInserted, nextIngestErr := ingestAndNotify(todayTransactions)
				if nextIngestErr != nil {
					return failIngest(nextIngestErr)
				}
				totalInserted += nextInserted

				previousAction := t.cursor.Action
				previousFields := t.cursor.Fields
				t.cursor.Step(nextPage, len(nextPage.Transactions))
				lastHasNext = t.cursor.HasNext
				if lastHasNext && sameRealtimeCursor(previousAction, previousFields, t.cursor.Action, t.cursor.Fields) {
					isPartial = true
					pollErr = errors.New("pagination unavailable: cursor did not progress")
					break
				}
				if !t.cursor.HasNext {
					break
				}
				if t.cursor.Action == "" || len(t.cursor.Fields) == 0 || t.cursor.Fields["dse_operationName"] == "" || t.cursor.Fields["dse_processorState"] == "" {
					slog.Warn("realtime poll reached page with incomplete navigation form", "page", pagesCount)
					isPartial = true
					pollErr = errors.New("pagination unavailable: next page exists but navigation action and fields are empty")
					break
				}
				curAction = t.cursor.Action
				curFields = t.cursor.Fields
				if pagesCount >= 5 && nextPage.HasNext {
					break
				}
			}
		}
	}

	if lastHasNext && pagesCount >= 5 && curAction != "" && len(curFields) > 0 {
		// Keep the poll open; the scheduler will resume this same cursor.
		t.pages = pagesCount
		t.rowsSeen = rowsSeen
		t.totalInserted = totalInserted
		t.nextAction = curAction
		t.nextFields = curFields
		t.continuation = true
		if t.priority != PriorityManualSync {
			t.priority = PriorityRealtimeContinuation
		}
		return scheduler.TaskStepResult{Done: false, Outcome: scheduler.OutcomeSuccess}, nil
	}

	t.pages = pagesCount
	t.rowsSeen = rowsSeen
	t.totalInserted = totalInserted
	slog.Info("ACB history parsed", "rows_seen", rowsSeen, "pages", pagesCount, "partial", isPartial)

	if isPartial {
		if pollErr != nil {
			return t.finishPoll(ctx, "PARTIAL", pollErr.Error())
		}
		return t.finishPoll(ctx, "PARTIAL", "PARTIAL_PAGE_BUDGET_REACHED")
	}
	t.m.ClearBackoff()
	if result, err := t.finishPoll(ctx, "SUCCEEDED", ""); err != nil {
		return result, err
	}
	if s := t.m.SessionLoader(); s != nil {
		if err := s.Persist(ctx, conn.ID, conn.Generation); err != nil {
			slog.Warn("could not persist refreshed ACB session", "error", err)
		}
	}
	return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
}

func (t *RealtimeTask) stepContinuation(ctx context.Context, conn storage.Connection) (scheduler.TaskStepResult, error) {
	if !t.todayStillCurrent() {
		return t.rolloverResult(ctx)
	}
	if t.cursor == nil {
		t.cursor = acb.NewPaginationCursor(t.nextAction, t.nextFields)
	}
	if t.cursor.Action == "" && t.nextAction != "" {
		t.cursor.Action = t.nextAction
		t.cursor.Fields = cloneRealtimeFields(t.nextFields)
	}
	if t.cursor.Action == "" || len(t.cursor.Fields) == 0 ||
		t.cursor.Fields["dse_operationName"] == "" || t.cursor.Fields["dse_processorState"] == "" {
		return t.finishPoll(ctx, "PARTIAL", "REALTIME_PAGINATION_UNAVAILABLE")
	}
	if t.pages >= realtimeMaxPages {
		return t.finishPoll(ctx, "PARTIAL", "PARTIAL_PAGE_BUDGET_REACHED")
	}

	previousAction := t.cursor.Action
	previousFields := cloneRealtimeFields(t.cursor.Fields)
	fields := t.pinToday(t.cursor.Fields)
	slog.Debug("ACB realtime request", "task", t.Kind(), "task_id", t.id, "reason", "REALTIME", "connection_id", conn.ID, "generation", conn.Generation, "from_date", t.today, "to_date", t.today, "page", t.pages+1, "phase", "continuation")
	resp, err := t.history(ctx, t.cursor.Action, fields)
	if err != nil {
		var authFail *acb.AuthFailure
		if errors.As(err, &authFail) {
			t.poll.Classifier = string(authFail.Kind)
			t.poll.AuthConfirmed = true
			return t.finishPoll(ctx, "AUTH_REQUIRED", authFail.Reason)
		}
		if errors.Is(err, acb.ErrConversationReset) {
			return t.finishPoll(ctx, "PARTIAL", "CONVERSATION_RESET")
		}
		if errors.Is(err, errRealtimeDateRollover) {
			return t.rolloverResult(ctx)
		}
		t.m.RecordNetworkFailure(err)
		return t.finishPoll(ctx, "PARTIAL", acb.SanitizeTransportError(err))
	}
	classifyRealtimeResponse(&resp)
	if resp.Kind == acb.LoginPage || resp.Kind == acb.OTPChallenge || resp.Kind == acb.CaptchaPage {
		return t.finishPoll(ctx, "PARTIAL", "AUTH_INCONCLUSIVE")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		t.m.SetBackoff(60 * time.Second)
		return t.finishPoll(ctx, "PARTIAL", "ACB_RATE_LIMITED")
	}
	if resp.Kind == acb.MaintenancePage {
		t.m.SetBackoff(60 * time.Second)
		return t.finishPoll(ctx, "PARTIAL", "ACB_MAINTENANCE")
	}
	t.pages++
	page, err := acb.ParseHistoryPage(resp.Body)
	if err != nil {
		return t.finishPoll(ctx, "PARTIAL", err.Error())
	}

	todayTransactions, err := acb.FilterHistoryTransactionDay(page.Transactions, t.today)
	if err != nil {
		return t.finishPoll(ctx, "PARTIAL", err.Error())
	}
	items := make([]storage.BatchTransactionItem, 0, len(todayTransactions))
	for _, txn := range todayTransactions {
		items = append(items, storage.BatchTransactionItem{
			Number: txn.Number, Credit: txn.Credit, Debit: txn.Debit, Balance: txn.Balance,
			TransactionAt: txn.TransactionAt, EffectiveAt: txn.EffectiveDate, Description: txn.Description,
		})
	}
	res, err := t.m.store.IngestTransactionsBatch(ctx, conn.ID, conn.Generation, conn.AccountMasked, items, false)
	if err != nil {
		status := "FAILED"
		if t.totalInserted > 0 {
			status = "PARTIAL"
		}
		return t.finishPoll(ctx, status, err.Error())
	}
	t.m.notifyNewEvents(res.NewEvents)
	t.totalInserted += res.InsertedCount
	t.rowsSeen += len(page.Transactions)
	t.cursor.Step(page, len(page.Transactions))
	t.pages = t.cursor.PageNumber
	if t.cursor.Truncated {
		return t.finishPoll(ctx, "PARTIAL", "REALTIME_PAGINATION_TRUNCATED")
	}
	if !t.cursor.HasNext {
		if s := t.m.SessionLoader(); s != nil {
			_ = s.Persist(ctx, conn.ID, conn.Generation)
		}
		return t.finishPoll(ctx, "SUCCEEDED", "")
	}
	if sameRealtimeCursor(previousAction, previousFields, t.cursor.Action, t.cursor.Fields) {
		return t.finishPoll(ctx, "PARTIAL", "REALTIME_CURSOR_NO_PROGRESS")
	}
	if t.cursor.Action == "" || len(t.cursor.Fields) == 0 ||
		t.cursor.Fields["dse_operationName"] == "" || t.cursor.Fields["dse_processorState"] == "" {
		return t.finishPoll(ctx, "PARTIAL", "REALTIME_PAGINATION_UNAVAILABLE")
	}
	t.nextAction = t.cursor.Action
	t.nextFields = t.cursor.Fields
	if t.pages >= realtimeMaxPages {
		return t.finishPoll(ctx, "PARTIAL", "PARTIAL_PAGE_BUDGET_REACHED")
	}
	return scheduler.TaskStepResult{Done: false, Outcome: scheduler.OutcomeSuccess}, nil
}

func cloneRealtimeFields(fields map[string]string) map[string]string {
	if fields == nil {
		return nil
	}
	clone := make(map[string]string, len(fields))
	for key, value := range fields {
		clone[key] = value
	}
	return clone
}

func (t *RealtimeTask) finishDone(err error) {
	if t.done != nil {
		select {
		case t.done <- err:
		default:
		}
	}
}
