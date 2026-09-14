package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// CatchUpTask executes a preemptible multi-day history scan following startup or downtime.
// Invariant: Step executes AT MOST ONE ACB page request per quantum, then yields to allow
// higher-priority realtime polls to run before subsequent catch-up pages.
type CatchUpTask struct {
	m            *Monitor
	id           string
	connectionID string
	generation   int64

	initialized bool
	fromDate    string
	toDate      string
	currentDay  time.Time

	dayPageCount int
	nextAction   string
	nextFields   map[string]string
	dayTxns      []storage.BatchTransactionItem
	cursor       *acb.PaginationCursor
	currentResp  acb.Response
	done         chan error
}

func NewCatchUpTask(m *Monitor, connectionID string, generation int64) *CatchUpTask {
	return &CatchUpTask{
		m:            m,
		id:           fmt.Sprintf("catchup_%d_%d", generation, time.Now().UnixNano()),
		connectionID: connectionID,
		generation:   generation,
	}
}

func (t *CatchUpTask) ID() string                { return t.id }
func (t *CatchUpTask) Kind() string              { return "CATCH_UP" }
func (t *CatchUpTask) Priority() UpstreamPriority { return PriorityCatchUp }
func (t *CatchUpTask) Generation() int64         { return t.generation }
func (t *CatchUpTask) CoalesceKey() string       { return "CATCH_UP" }

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
			"task_gen", t.generation, "current_gen", conn.Generation)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
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
			RequeueAt: time.Now().Add(5 * time.Second),
			Outcome:   scheduler.OutcomeTransient,
		}, nil
	}

	if !t.initialized {
		nowInLoc := time.Now().In(acb.DefaultLocation)
		today := nowInLoc.Format("2006-01-02")
		fromDate := nowInLoc.AddDate(0, 0, -1).Format("2006-01-02") // default yesterday

		if cp, err := t.m.store.GetCheckpoint(ctx, conn.ID); err == nil && cp != nil && cp.CoverageTo != "" {
			fromDate = cp.CoverageTo
		}

		// Clamp maximum automatic catch-up window to 7 days
		sevenDaysAgo := nowInLoc.AddDate(0, 0, -7).Format("2006-01-02")
		if fromDate < sevenDaysAgo {
			fromDate = sevenDaysAgo
		}
		if fromDate > today {
			fromDate = today
		}

		fromT, _ := time.Parse("2006-01-02", fromDate)
		t.currentDay = fromT
		t.toDate = today
		t.fromDate = fromDate
		t.initialized = true
		slog.Info("initialized catch-up task", "from", fromDate, "to", today)
	}

	toT, _ := time.Parse("2006-01-02", t.toDate)

	// Skip covered historical days before today
	for !t.currentDay.After(toT) && t.currentDay.Before(toT) && t.dayPageCount == 0 {
		dayStr := t.currentDay.Format("2006-01-02")
		if covered, err := t.m.store.CheckRangeCoverage(ctx, conn.ID, dayStr, dayStr); err == nil && covered {
			_ = t.m.store.SaveCheckpoint(ctx, storage.Checkpoint{
				ConnectionID: conn.ID,
				ScanID:       "scan_" + strconv.FormatInt(time.Now().Unix(), 10),
				CoverageFrom: t.fromDate,
				CoverageTo:   dayStr,
			})
			t.currentDay = t.currentDay.AddDate(0, 0, 1)
			continue
		}
		break
	}

	// Check if entire catch-up range is completed
	if t.currentDay.After(toT) {
		if s := t.m.SessionLoader(); s != nil {
			_ = s.Persist(ctx, conn.ID, conn.Generation)
		}
		slog.Info("catch-up task completed successfully", "from", t.fromDate, "to", t.toDate)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	dayStr := t.currentDay.Format("2006-01-02")

	// If starting a fresh day, bootstrap if needed to get form tokens
	if t.dayPageCount == 0 && t.currentResp.Body == "" {
		if s := t.m.SessionLoader(); s != nil {
			if err := s.Restore(ctx, conn.ID, conn.Generation); err != nil {
				return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeAuth}, err
			}
		}
		resp, err := t.m.client.Bootstrap(ctx)
		if err != nil {
			return scheduler.TaskStepResult{
				Done:      false,
				RequeueAt: time.Now().Add(5 * time.Second),
				Outcome:   scheduler.OutcomeTransient,
				Error:     err,
			}, nil
		}
		if resp.Kind == acb.LoginPage || resp.Kind == acb.OTPChallenge || resp.Kind == acb.CaptchaPage {
			authErr := errors.New("session expired during catch-up")
			t.finishDone(authErr)
			return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeAuth, Error: authErr}, nil
		}
		if resp.StatusCode == 429 {
			t.m.SetBackoff(60 * time.Second)
			return scheduler.TaskStepResult{
				Done:      false,
				RequeueAt: time.Now().Add(60 * time.Second),
				Outcome:   scheduler.OutcomeTransient,
			}, nil
		}
		if resp.Kind == acb.MaintenancePage {
			t.m.SetBackoff(60 * time.Second)
			return scheduler.TaskStepResult{
				Done:      false,
				RequeueAt: time.Now().Add(60 * time.Second),
				Outcome:   scheduler.OutcomeTransient,
			}, nil
		}
		t.currentResp = resp
	}

	// Prepare form navigation for this day if page 0
	if t.dayPageCount == 0 {
		form, formErr := acb.ExtractHistoryForm(t.currentResp.Body)
		if formErr != nil {
			return scheduler.TaskStepResult{
				Done:      false,
				RequeueAt: time.Now().Add(5 * time.Second),
				Outcome:   scheduler.OutcomeTransient,
				Error:     formErr,
			}, nil
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

	// EXECUTE AT MOST ONE ACB History HTTP Request
	histResp, histErr := t.m.client.History(ctx, t.nextAction, t.nextFields)
	if histErr != nil {
		return scheduler.TaskStepResult{
			Done:      false,
			RequeueAt: time.Now().Add(5 * time.Second),
			Outcome:   scheduler.OutcomeTransient,
			Error:     histErr,
		}, nil
	}
	if histResp.StatusCode == 429 || histResp.Kind == acb.MaintenancePage {
		t.m.SetBackoff(60 * time.Second)
		return scheduler.TaskStepResult{
			Done:      false,
			RequeueAt: time.Now().Add(60 * time.Second),
			Outcome:   scheduler.OutcomeTransient,
		}, nil
	}
	if histResp.Kind == acb.LoginPage || histResp.Kind == acb.OTPChallenge || histResp.Kind == acb.CaptchaPage {
		authErr := errors.New("session expired during catch-up")
		t.finishDone(authErr)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeAuth, Error: authErr}, nil
	}

	t.dayPageCount++
	if _, err := acb.ExtractHistoryForm(histResp.Body); err == nil {
		t.currentResp = histResp
	}

	pageResult, parseErr := acb.ParseHistoryPage(histResp.Body)
	if parseErr != nil {
		return scheduler.TaskStepResult{
			Done:      false,
			RequeueAt: time.Now().Add(5 * time.Second),
			Outcome:   scheduler.OutcomeTransient,
			Error:     parseErr,
		}, nil
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

	// Ingest with CATCH_UP source:
	// New credit transactions enqueue webhooks (no missed payments!), but voice is suppressed!
	res, ingestErr := t.m.store.IngestTransactionsBatchWithSource(ctx, conn.ID, conn.Generation, conn.AccountMasked, pageItems, false, "CATCH_UP")
	if ingestErr != nil {
		return scheduler.TaskStepResult{
			Done:      false,
			RequeueAt: time.Now().Add(5 * time.Second),
			Outcome:   scheduler.OutcomeTransient,
			Error:     ingestErr,
		}, nil
	}
	t.m.notifyNewEvents(res.NewEvents)
	t.dayTxns = append(t.dayTxns, pageItems...)

	if t.cursor == nil {
		t.cursor = acb.NewPaginationCursor(t.nextAction, t.nextFields)
	}
	t.cursor.Step(pageResult, len(pageResult.Transactions))

	// Check if more pages exist for this day within the 20-page budget
	if t.cursor.HasNext && (t.cursor.Action != "" || len(t.cursor.Fields) > 0) && t.dayPageCount < 20 {
		t.nextAction = t.cursor.Action
		t.nextFields = t.cursor.Fields
		if t.nextFields == nil {
			t.nextFields = make(map[string]string)
		}
		t.nextFields["_raw"] = "true"
		// Bounded quantum complete! Yield after 1 page so higher-priority tasks can preempt.
		return scheduler.TaskStepResult{Done: false, Outcome: scheduler.OutcomeSuccess}, nil
	}

	// Fail closed if day pagination was truncated or budget exceeded with remaining pages
	if t.cursor.Truncated || (t.cursor.HasNext && t.dayPageCount >= 20) {
		truncErr := fmt.Errorf("catch-up pagination truncated for day %s: seen %d of %d rows (pages: %d)",
			dayStr, t.cursor.CumulativeRows, t.cursor.TotalRowsSeen, t.dayPageCount)
		slog.Warn("catch-up day truncated, failing closed without advancing coverage", "day", dayStr, "error", truncErr)
		t.finishDone(truncErr)
		return scheduler.TaskStepResult{
			Done:      false,
			RequeueAt: time.Now().Add(30 * time.Second),
			Outcome:   scheduler.OutcomeTransient,
			Error:     truncErr,
		}, nil
	}

	// Day is complete: advance coverage and checkpoint ONLY after the full day completes!
	_ = t.m.store.RecordCoveragePerDay(ctx, conn.ID, map[string]int{dayStr: len(t.dayTxns)})
	_ = t.m.store.SaveCheckpoint(ctx, storage.Checkpoint{
		ConnectionID: conn.ID,
		ScanID:       "scan_" + strconv.FormatInt(time.Now().Unix(), 10),
		CoverageFrom: t.fromDate,
		CoverageTo:   dayStr,
	})

	// Reset day-level state and advance to next day
	t.dayPageCount = 0
	t.dayTxns = nil
	t.nextAction = ""
	t.nextFields = nil
	t.cursor = nil
	t.currentDay = t.currentDay.AddDate(0, 0, 1)

	if t.currentDay.After(toT) {
		if s := t.m.SessionLoader(); s != nil {
			_ = s.Persist(ctx, conn.ID, conn.Generation)
		}
		slog.Info("catch-up task finished all days", "from", t.fromDate, "to", t.toDate)
		t.finishDone(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	// Yield between days
	return scheduler.TaskStepResult{Done: false, Outcome: scheduler.OutcomeSuccess}, nil
}

func (t *CatchUpTask) finishDone(err error) {
	if t.done != nil {
		select {
		case t.done <- err:
		default:
		}
	}
}
