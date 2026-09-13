package monitor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type historyTaskResult struct {
	insertedCount int
	err           error
}

// HistorySyncTask executes a historical range query bounded to single-page quanta.
type HistorySyncTask struct {
	m            *Monitor
	id           string
	connectionID string
	generation   int64
	fromDay      string
	toDay        string
	jobID        string

	initialized      bool
	fromT            time.Time
	toT              time.Time
	nextAction       string
	nextFields       map[string]string
	pagesFetched     int
	maxPages         int
	maxTotalRowsSeen int
	seenTxnNumbers   map[string]struct{}
	allBatchItems    []storage.BatchTransactionItem
	complete         bool
	truncated        bool

	done chan historyTaskResult
}

func NewHistorySyncTask(m *Monitor, connectionID string, generation int64, fromDay, toDay, jobID string) *HistorySyncTask {
	return &HistorySyncTask{
		m:              m,
		id:             fmt.Sprintf("hist_%s_%s_%d", fromDay, toDay, time.Now().UnixNano()),
		connectionID:   connectionID,
		generation:     generation,
		fromDay:        fromDay,
		toDay:          toDay,
		jobID:          jobID,
		maxPages:       10,
		seenTxnNumbers: make(map[string]struct{}),
		done:           make(chan historyTaskResult, 1),
	}
}

func (t *HistorySyncTask) ID() string                { return t.id }
func (t *HistorySyncTask) Kind() string              { return "FILTER_HISTORY" }
func (t *HistorySyncTask) Priority() UpstreamPriority { return PriorityFilterHistory }
func (t *HistorySyncTask) Generation() int64         { return t.generation }
func (t *HistorySyncTask) CoalesceKey() string {
	return fmt.Sprintf("hist_%s_%s", t.fromDay, t.toDay)
}

func (t *HistorySyncTask) Step(ctx context.Context) (scheduler.TaskStepResult, error) {
	if err := ctx.Err(); err != nil {
		t.finish(0, err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeTransient}, err
	}

	conn, err := t.m.store.Connection(ctx)
	if err != nil {
		t.finish(0, err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
	}
	if conn.State != "MONITORING" {
		notMonErr := errors.New("bank connection is not in MONITORING state")
		t.finish(0, notMonErr)
		return scheduler.TaskStepResult{Done: true, Error: notMonErr, Outcome: scheduler.OutcomeFatal}, notMonErr
	}

	if t.generation > 0 && (conn.ID != t.connectionID || conn.Generation != t.generation) {
		staleErr := errors.New("history task discarded due to generation mismatch")
		t.finish(0, staleErr)
		return scheduler.TaskStepResult{Done: true, Error: staleErr, Outcome: scheduler.OutcomeSuccess}, nil
	}

	if t.m.IsBackoffActive() {
		err := errors.New("circuit breaker backoff active")
		t.finish(0, err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeTransient}, err
	}

	if t.m.client == nil {
		err := errors.New("bank client not configured")
		t.finish(0, err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
	}

	if !t.initialized {
		fromT, err := time.Parse("2006-01-02", t.fromDay)
		if err != nil {
			t.finish(0, err)
			return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
		}
		toT, err := time.Parse("2006-01-02", t.toDay)
		if err != nil {
			t.finish(0, err)
			return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
		}
		t.fromT = fromT
		t.toT = toT

		if t.m.sessions != nil {
			if err := t.m.sessions.Restore(ctx, conn.ID, conn.Generation); err != nil {
				restoreErr := fmt.Errorf("restore ACB session: %w", err)
				t.finish(0, restoreErr)
				return scheduler.TaskStepResult{Done: true, Error: restoreErr, Outcome: scheduler.OutcomeAuth}, restoreErr
			}
		}

		resp, err := t.m.client.Bootstrap(ctx)
		if err != nil {
			bootErr := fmt.Errorf("bootstrap ACB session: %w", err)
			t.finish(0, bootErr)
			return scheduler.TaskStepResult{Done: true, Error: bootErr, Outcome: scheduler.OutcomeTransient}, bootErr
		}
		if resp.Kind == acb.LoginPage || resp.Kind == acb.OTPChallenge || resp.Kind == acb.CaptchaPage {
			err := errors.New("ACB session expired or challenge required")
			t.finish(0, err)
			return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeAuth}, err
		}
		if resp.Kind == acb.MaintenancePage {
			t.m.SetBackoff(60 * time.Second)
			err := errors.New("ACB maintenance")
			t.finish(0, err)
			return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeTransient}, err
		}

		form, formErr := acb.ExtractHistoryForm(resp.Body)
		if formErr != nil {
			t.finish(0, formErr)
			return scheduler.TaskStepResult{Done: true, Error: formErr, Outcome: scheduler.OutcomeFatal}, formErr
		}
		if form.Fields["AccountNbr"] == "" && conn.AccountMasked != "" {
			form.Fields["AccountNbr"] = conn.AccountMasked
		}
		form.Fields["FromDate"] = t.fromT.Format("02/01/2006")
		form.Fields["ToDate"] = t.toT.Format("02/01/2006")
		form.Fields["_explicitRange"] = "true"

		t.nextAction = form.Action
		t.nextFields = form.Fields
		t.initialized = true
	}

	// EXECUTE AT MOST ONE ACB History request
	histResp, histErr := t.m.client.History(ctx, t.nextAction, t.nextFields)
	if histErr != nil {
		queryErr := fmt.Errorf("query ACB history: %w", histErr)
		t.finish(len(t.allBatchItems), queryErr)
		return scheduler.TaskStepResult{Done: true, Error: queryErr, Outcome: scheduler.OutcomeTransient}, queryErr
	}
	t.pagesFetched++

	if histResp.Kind == acb.LoginPage || histResp.Kind == acb.OTPChallenge || histResp.Kind == acb.CaptchaPage {
		err := errors.New("ACB session expired or challenge required during history fetch")
		t.finish(len(t.allBatchItems), err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeAuth}, err
	}
	if histResp.Kind == acb.MaintenancePage {
		t.m.SetBackoff(60 * time.Second)
		err := errors.New("ACB maintenance during history fetch")
		t.finish(len(t.allBatchItems), err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeTransient}, err
	}

	pageResult, parseErr := acb.ParseHistoryPage(histResp.Body)
	if parseErr != nil {
		parseWrapped := fmt.Errorf("parse ACB history page %d: %w", t.pagesFetched, parseErr)
		t.finish(len(t.allBatchItems), parseWrapped)
		return scheduler.TaskStepResult{Done: true, Error: parseWrapped, Outcome: scheduler.OutcomeFatal}, parseWrapped
	}

	if pageResult.TotalRows > t.maxTotalRowsSeen {
		t.maxTotalRowsSeen = pageResult.TotalRows
	}

	for _, txn := range pageResult.Transactions {
		if _, exists := t.seenTxnNumbers[txn.Number]; exists {
			continue
		}
		t.seenTxnNumbers[txn.Number] = struct{}{}
		t.allBatchItems = append(t.allBatchItems, storage.BatchTransactionItem{
			Number:        txn.Number,
			Credit:        txn.Credit,
			Debit:         txn.Debit,
			Balance:       txn.Balance,
			TransactionAt: txn.TransactionAt,
			EffectiveAt:   txn.EffectiveDate,
			Description:   txn.Description,
		})
	}

	// Check if more pages exist within page budget
	if pageResult.HasNext && t.pagesFetched < t.maxPages {
		if pageResult.NextAction == "" && len(pageResult.NextFields) == 0 {
			t.complete = false
		} else {
			t.nextAction = pageResult.NextAction
			t.nextFields = pageResult.NextFields
			if t.nextFields == nil {
				t.nextFields = make(map[string]string)
			}
			t.nextFields["_raw"] = "true"
			// Yield after single page quantum!
			return scheduler.TaskStepResult{Done: false, Outcome: scheduler.OutcomeSuccess}, nil
		}
	} else if !pageResult.HasNext {
		t.complete = true
	} else {
		// Budget reached
		t.complete = false
		t.truncated = true
	}

	// Final completion processing
	// Ingest with FILTER_SYNC source (suppressing webhooks and voice)
	_, ingestErr := t.m.store.IngestTransactionsBatchWithSource(ctx, conn.ID, conn.Generation, conn.AccountMasked, t.allBatchItems, false, "FILTER_SYNC")
	if ingestErr != nil {
		err := fmt.Errorf("ingest history transactions: %w", ingestErr)
		t.finish(0, err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
	}

	if !t.complete {
		err := fmt.Errorf("ACB history range incomplete: fetched %d pages (%d rows) but more rows remain", t.pagesFetched, len(t.allBatchItems))
		t.finish(len(t.allBatchItems), err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeSuccess}, err
	}

	dayCounts := make(map[string]int)
	for cur := t.fromT; !cur.After(t.toT); cur = cur.AddDate(0, 0, 1) {
		dayCounts[cur.Format("2006-01-02")] = 0
	}
	for _, item := range t.allBatchItems {
		day := item.TransactionAt
		if len(day) >= 10 {
			if t, err := time.Parse("02/01/2006", day[:10]); err == nil {
				day = t.Format("2006-01-02")
			}
		}
		if _, exists := dayCounts[day]; exists {
			dayCounts[day]++
		}
	}

	if err := t.m.store.RecordCoveragePerDay(ctx, conn.ID, dayCounts); err != nil {
		recErr := fmt.Errorf("record coverage: %w", err)
		t.finish(len(t.allBatchItems), recErr)
		return scheduler.TaskStepResult{Done: true, Error: recErr, Outcome: scheduler.OutcomeFatal}, recErr
	}

	if t.jobID != "" && t.m.store != nil {
		_ = t.m.store.CompleteHistorySyncJob(ctx, t.jobID, len(t.allBatchItems), nil)
	}

	t.finish(len(t.allBatchItems), nil)
	return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
}

func (t *HistorySyncTask) finish(insertedCount int, err error) {
	if t.jobID != "" && t.m.store != nil && err != nil {
		_ = t.m.store.CompleteHistorySyncJob(context.Background(), t.jobID, insertedCount, err)
	}
	select {
	case t.done <- historyTaskResult{insertedCount: insertedCount, err: err}:
	default:
	}
}
