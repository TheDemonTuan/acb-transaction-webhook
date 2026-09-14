package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
)

// RealtimeTask executes a realtime adaptive transaction poll or an operator manual sync.
type RealtimeTask struct {
	m            *Monitor
	id           string
	priority     UpstreamPriority
	connectionID string
	generation   int64
	done         chan error
}

func NewRealtimeTask(m *Monitor, priority UpstreamPriority, connectionID string, generation int64) *RealtimeTask {
	kind := "REALTIME_POLL"
	if priority == PriorityManualSync {
		kind = "MANUAL_SYNC"
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
	return "REALTIME_POLL"
}

func (t *RealtimeTask) CoalesceKey() string {
	if t.priority == PriorityManualSync {
		return "MANUAL_SYNC"
	}
	return "REALTIME_POLL"
}

func (t *RealtimeTask) Step(ctx context.Context) (scheduler.TaskStepResult, error) {
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

	// Generation guard: a task queued for an older or mismatched generation is discarded before any ACB call.
	if t.generation > 0 && (conn.ID != t.connectionID || conn.Generation != t.generation) {
		slog.Info("stale realtime/manual task discarded due to generation mismatch",
			"task_gen", t.generation, "current_gen", conn.Generation, "conn_id", conn.ID)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	if t.m.IsBackoffActive() {
		slog.Info("skipping poll: circuit breaker backoff active", "until", t.m.BackoffUntil())
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeTransient}, nil
	}

	hasActiveAttempt, err := t.m.store.HasActiveAuthAttempt(ctx, conn.ID)
	if err != nil {
		slog.Error("poll fail-closed: failed to check active auth attempt", "connection_id", conn.ID, "error", err)
		checkErr := fmt.Errorf("check active auth attempt: %w", err)
		t.finishDone(checkErr)
		return scheduler.TaskStepResult{Done: true, Error: checkErr, Outcome: scheduler.OutcomeFatal}, checkErr
	}
	if hasActiveAttempt {
		slog.Info("skipping poll: browser authentication in progress", "connection_id", conn.ID)
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

	// 1. Fetch account detail page to verify session and extract form state
	resp, err := t.m.client.Bootstrap(ctx)
	if err != nil {
		poll.Status = "FAILED"
		poll.Error = err.Error()
		_ = t.m.finishPoll(ctx, poll, 0)
		t.m.notifyPollWaiters(err)
		t.finishDone(err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeTransient}, err
	}

	poll.Classifier = string(resp.Kind)
	poll.HTTPStatus = resp.StatusCode

	if resp.Kind == acb.LoginPage {
		poll.Status = "AUTH_REQUIRED"
		poll.Error = "SESSION_EXPIRED"
		_ = t.m.finishPoll(ctx, poll, 0)
		slog.Warn("ACB confirmed the session is no longer authenticated; transitioned to AUTH_REQUIRED")
		t.m.notifyPollWaiters(nil)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeAuth}, nil
	}
	if resp.Kind == acb.OTPChallenge || resp.Kind == acb.CaptchaPage {
		poll.Status = "AUTH_REQUIRED"
		poll.Error = string(resp.Kind)
		_ = t.m.finishPoll(ctx, poll, 0)
		slog.Warn("ACB requires interactive authentication", "challenge", resp.Kind)
		t.m.notifyPollWaiters(nil)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeAuth}, nil
	}

	if resp.StatusCode == 429 {
		poll.Status = "FAILED"
		poll.Error = "ACB_RATE_LIMITED"
		t.m.SetBackoff(60 * time.Second)
		_ = t.m.finishPoll(ctx, poll, 0)
		slog.Warn("ACB rate limit detected (429); backoff for 60s")
		t.m.notifyPollWaiters(nil)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeTransient}, nil
	}

	if resp.Kind == acb.MaintenancePage {
		poll.Status = "FAILED"
		poll.Error = "ACB_MAINTENANCE"
		t.m.SetBackoff(60 * time.Second)
		_ = t.m.finishPoll(ctx, poll, 0)
		slog.Warn("ACB maintenance detected; backoff for 60s")
		t.m.notifyPollWaiters(nil)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeTransient}, nil
	}

	// If the page is not HistoryPage directly, try extracting form state
	historyMarkup := resp.Body
	if resp.Kind != acb.HistoryPage {
		form, formErr := acb.ExtractHistoryForm(resp.Body)
		if formErr != nil {
			poll.Status = "FAILED"
			poll.Error = formErr.Error()
			_ = t.m.finishPoll(ctx, poll, 0)
			t.m.notifyPollWaiters(formErr)
			t.finishDone(formErr)
			return scheduler.TaskStepResult{Done: true, Error: formErr, Outcome: scheduler.OutcomeFatal}, formErr
		}

		if form.Fields["AccountNbr"] == "" && conn.AccountMasked != "" {
			form.Fields["AccountNbr"] = conn.AccountMasked
		}
		histResp, histErr := t.m.client.History(ctx, form.Action, form.Fields)
		if histErr != nil {
			poll.Status = "FAILED"
			poll.Error = histErr.Error()
			_ = t.m.finishPoll(ctx, poll, 0)
			t.m.notifyPollWaiters(histErr)
			t.finishDone(histErr)
			return scheduler.TaskStepResult{Done: true, Error: histErr, Outcome: scheduler.OutcomeTransient}, histErr
		}
		historyMarkup = histResp.Body
		poll.Classifier = string(histResp.Kind)
		poll.HTTPStatus = histResp.StatusCode

		if histResp.Kind == acb.LoginPage {
			poll.Status = "AUTH_REQUIRED"
			poll.Error = "SESSION_EXPIRED"
			_ = t.m.finishPoll(ctx, poll, 0)
			t.m.notifyPollWaiters(nil)
			t.finishDone(nil)
			return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeAuth}, nil
		}
		if histResp.Kind == acb.OTPChallenge || histResp.Kind == acb.CaptchaPage {
			poll.Status = "AUTH_REQUIRED"
			poll.Error = string(histResp.Kind)
			_ = t.m.finishPoll(ctx, poll, 0)
			t.m.notifyPollWaiters(nil)
			t.finishDone(nil)
			return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeAuth}, nil
		}
		if histResp.StatusCode == 429 {
			poll.Status = "FAILED"
			poll.Error = "ACB_RATE_LIMITED"
			t.m.SetBackoff(60 * time.Second)
			_ = t.m.finishPoll(ctx, poll, 0)
			t.m.notifyPollWaiters(nil)
			t.finishDone(nil)
			return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeTransient}, nil
		}
		if histResp.Kind == acb.MaintenancePage {
			poll.Status = "FAILED"
			poll.Error = "ACB_MAINTENANCE"
			t.m.SetBackoff(60 * time.Second)
			_ = t.m.finishPoll(ctx, poll, 0)
			t.m.notifyPollWaiters(nil)
			t.finishDone(nil)
			return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeTransient}, nil
		}
	}

	// Parse transaction history
	pageResult, parseErr := acb.ParseHistoryPage(historyMarkup)
	if parseErr != nil {
		poll.Status = "FAILED"
		poll.Error = parseErr.Error()
		_ = t.m.finishPoll(ctx, poll, 0)
		t.m.notifyPollWaiters(parseErr)
		t.finishDone(parseErr)
		return scheduler.TaskStepResult{Done: true, Error: parseErr, Outcome: scheduler.OutcomeFatal}, parseErr
	}

	pagesCount := 1
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
		poll.RowsSeen = rowsSeen
		poll.Pages = pagesCount
		poll.Status = "FAILED"
		if totalInserted > 0 {
			poll.Status = "PARTIAL"
			t.m.ScheduleCatchUp()
		}
		poll.Error = err.Error()
		_ = t.m.finishPoll(ctx, poll, totalInserted)
		t.m.notifyPollWaiters(err)
		t.finishDone(err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
	}

	inserted, err := ingestAndNotify(pageResult.Transactions)
	if err != nil {
		return failIngest(err)
	}
	totalInserted += inserted

	// If page has next, fetch up to 5 pages for realtime poll
	if pageResult.HasNext {
		if pageResult.NextAction == "" && len(pageResult.NextFields) == 0 {
			slog.Warn("realtime poll page indicates next page exists but no navigation available")
			isPartial = true
			pollErr = errors.New("pagination unavailable: next page exists but navigation action and fields are empty")
		} else {
			curAction := pageResult.NextAction
			curFields := pageResult.NextFields
			for pagesCount < 5 {
				if curFields == nil {
					curFields = make(map[string]string)
				}
				curFields["_raw"] = "true"
				nextResp, nextErr := t.m.client.History(ctx, curAction, curFields)
				if nextErr != nil {
					slog.Warn("realtime poll next page fetch error", "page", pagesCount+1, "error", nextErr)
					isPartial = true
					pollErr = nextErr
					break
				}
				pagesCount++
				nextPage, err := acb.ParseHistoryPage(nextResp.Body)
				if err != nil {
					slog.Warn("realtime poll next page parse error", "page", pagesCount, "error", err)
					isPartial = true
					pollErr = err
					break
				}
				rowsSeen += len(nextPage.Transactions)
				nextInserted, nextIngestErr := ingestAndNotify(nextPage.Transactions)
				if nextIngestErr != nil {
					return failIngest(nextIngestErr)
				}
				totalInserted += nextInserted

				if !nextPage.HasNext {
					break
				}
				if nextPage.NextAction == "" && len(nextPage.NextFields) == 0 {
					slog.Warn("realtime poll reached page with HasNext but no navigation available", "page", pagesCount)
					isPartial = true
					pollErr = errors.New("pagination unavailable: next page exists but navigation action and fields are empty")
					break
				}
				curAction = nextPage.NextAction
				curFields = nextPage.NextFields
				if pagesCount >= 5 && nextPage.HasNext {
					slog.Warn("realtime poll reached 5 page limit while more records remain; marking PARTIAL and scheduling catch-up")
					isPartial = true
					pollErr = errors.New("PARTIAL_PAGE_BUDGET_REACHED")
					break
				}
			}
		}
	}

	poll.RowsSeen = rowsSeen
	poll.Pages = pagesCount
	slog.Info("ACB history parsed", "rows_seen", poll.RowsSeen, "pages", poll.Pages, "partial", isPartial)

	if isPartial {
		poll.Status = "PARTIAL"
		if pollErr != nil {
			poll.Error = pollErr.Error()
		} else {
			poll.Error = "PARTIAL_PAGE_BUDGET_REACHED"
		}
		t.m.ScheduleCatchUp()
	} else {
		poll.Status = "SUCCEEDED"
	}
	t.m.ClearBackoff()
	if err := t.m.finishPoll(ctx, poll, totalInserted); err != nil {
		t.m.notifyPollWaiters(err)
		t.finishDone(err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
	}

	if s := t.m.SessionLoader(); s != nil {
		if err := s.Persist(ctx, conn.ID, conn.Generation); err != nil {
			slog.Warn("could not persist refreshed ACB session", "error", err)
		}
	}

	t.m.notifyPollWaiters(nil)
	t.finishDone(nil)
	return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
}

func (t *RealtimeTask) finishDone(err error) {
	if t.done != nil {
		select {
		case t.done <- err:
		default:
		}
	}
}
