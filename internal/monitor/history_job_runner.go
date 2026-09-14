package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// ACBHistoryClient represents the upstream bank interface required by the history runner.
type ACBHistoryClient interface {
	Bootstrap(ctx context.Context) (acb.Response, error)
	History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error)
}

// HistoryJobRunner coordinates background asynchronous history synchronization jobs.
type HistoryJobRunner struct {
	store          *storage.Store
	client         ACBHistoryClient
	scheduler      *scheduler.Scheduler
	sessions       *SessionLoader
	monitor        *Monitor
	wakeCh         chan struct{}
	staleThreshold time.Duration
	pollInterval   time.Duration
	logger         *slog.Logger
	paused         atomic.Bool

	mu         sync.Mutex
	activeTask *HistoryJobTask
}

// NewHistoryJobRunner creates a new durable history job runner.
func NewHistoryJobRunner(store *storage.Store, client ACBHistoryClient, sched *scheduler.Scheduler, sessions *SessionLoader) *HistoryJobRunner {
	return &HistoryJobRunner{
		store:          store,
		client:         client,
		scheduler:      sched,
		sessions:       sessions,
		wakeCh:         make(chan struct{}, 1),
		staleThreshold: 60 * time.Second,
		pollInterval:   5 * time.Second,
		logger:         slog.Default().With("component", "history_runner"),
	}
}

// WithMonitor sets the associated monitor for backoff/circuit-breaker awareness.
func (r *HistoryJobRunner) WithMonitor(m *Monitor) *HistoryJobRunner {
	r.monitor = m
	return r
}

// WithStaleThreshold sets the heartbeat staleness threshold for crashed job recovery.
func (r *HistoryJobRunner) WithStaleThreshold(d time.Duration) *HistoryJobRunner {
	if d > 0 {
		r.staleThreshold = d
	}
	return r
}

// WithPollInterval sets the background poll interval for queued jobs.
func (r *HistoryJobRunner) WithPollInterval(d time.Duration) *HistoryJobRunner {
	if d > 0 {
		r.pollInterval = d
	}
	return r
}

// Pause halts the processing of new or claimed history sync jobs.
func (r *HistoryJobRunner) Pause() {
	r.paused.Store(true)
}

// Resume unpauses history runner processing and signals wake.
func (r *HistoryJobRunner) Resume() {
	r.paused.Store(false)
	r.Wake()
}

// IsPaused reports whether the history runner is paused.
func (r *HistoryJobRunner) IsPaused() bool {
	return r.paused.Load()
}

// Wake notifies the runner that a new job was enqueued, prompting immediate processing without busy-polling.
func (r *HistoryJobRunner) Wake() {
	select {
	case r.wakeCh <- struct{}{}:
	default:
	}
}

// RecoverStaleJobs recovers jobs left in RUNNING state past their heartbeat deadline.
func (r *HistoryJobRunner) RecoverStaleJobs(ctx context.Context) (int, error) {
	if r.store == nil {
		return 0, errors.New("store not configured")
	}
	staleBefore := time.Now().Add(-r.staleThreshold)
	count, err := r.store.RequeueStaleHistorySyncJobs(ctx, staleBefore)
	if err != nil {
		r.logger.Error("failed to recover stale history jobs", "error", err)
		return 0, err
	}
	if count > 0 {
		r.logger.Info("recovered stale history jobs", "count", count)
	}
	return count, nil
}

// CancelJob cancels the currently active running task if it matches jobID.
func (r *HistoryJobRunner) CancelJob(jobID string) {
	r.mu.Lock()
	task := r.activeTask
	r.mu.Unlock()

	if task != nil && task.job.ID == jobID {
		task.cancel()
		if r.scheduler != nil {
			r.scheduler.CancelTask(task.ID())
		}
	}
}

// Run executes the background scanner and runner loop until the context is canceled.
func (r *HistoryJobRunner) Run(ctx context.Context) {
	r.logger.Info("history runner started")

	// Startup recovery pass: recover any crashed/stale RUNNING jobs
	_, _ = r.RecoverStaleJobs(ctx)

	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("history runner stopping on context done")
			return
		default:
		}

		processed, err := r.ProcessNextJob(ctx)
		if err != nil {
			r.logger.Warn("history runner error processing job", "error", err)
		}

		if processed {
			// Immediately check if more queued jobs are ready
			continue
		}

		select {
		case <-ctx.Done():
			r.logger.Info("history runner stopping on context done")
			return
		case <-r.wakeCh:
		case <-ticker.C:
			_, _ = r.RecoverStaleJobs(ctx)
		}
	}
}

// ProcessNextJob attempts to claim and execute the next runnable history sync job.
// Returns true if a job was claimed and executed, false if no job was ready.
func (r *HistoryJobRunner) ProcessNextJob(ctx context.Context) (bool, error) {
	if r.paused.Load() {
		return false, nil
	}
	if r.store == nil {
		return false, errors.New("store not configured")
	}

	conn, err := r.store.Connection(ctx)
	if err != nil {
		return false, fmt.Errorf("lookup connection: %w", err)
	}
	if conn.State != "MONITORING" {
		return false, nil
	}

	if r.monitor != nil && r.monitor.IsBackoffActive() {
		return false, nil
	}

	hasActiveAuth, err := r.store.HasActiveAuthAttempt(ctx, conn.ID)
	if err != nil {
		return false, fmt.Errorf("check active auth attempt: %w", err)
	}
	if hasActiveAuth {
		return false, nil
	}

	job, claimed, err := r.store.ClaimNextHistorySyncJob(ctx, time.Now())
	if err != nil {
		return false, fmt.Errorf("claim next history sync job: %w", err)
	}
	if !claimed {
		return false, nil
	}

	r.logger.Info("claimed history sync job", "job_id", job.ID, "from", job.RangeFrom, "to", job.RangeTo)

	task := NewHistoryJobTask(r, job, conn)
	r.mu.Lock()
	r.activeTask = task
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		if r.activeTask == task {
			r.activeTask = nil
		}
		r.mu.Unlock()
	}()

	if r.scheduler != nil && r.scheduler.IsRunning() {
		if err := r.scheduler.Enqueue(task); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = r.store.RequeueHistorySyncJob(cleanupCtx, job.ID, "SCHEDULER_ENQUEUE_FAILED", err.Error(), time.Now().Add(5*time.Second))
			return false, err
		}

		select {
		case <-ctx.Done():
			task.cancel()
			if r.scheduler != nil {
				r.scheduler.CancelTask(task.ID())
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = r.store.RequeueHistorySyncJob(cleanupCtx, job.ID, "WORKER_SHUTDOWN", "Worker stopped during execution", time.Now().Add(5*time.Second))
			return true, ctx.Err()
		case <-task.done:
			return true, task.stepErr
		}
	}

	// Synchronous execution fallback (e.g. focused unit testing without scheduler goroutine)
	for {
		if err := ctx.Err(); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = r.store.RequeueHistorySyncJob(cleanupCtx, job.ID, "WORKER_SHUTDOWN", "Worker stopped during execution", time.Now().Add(5*time.Second))
			return true, err
		}

		res, stepErr := task.Step(ctx)
		if stepErr != nil {
			return true, stepErr
		}
		if res.Done {
			return true, nil
		}
	}
}

// HistoryJobTask executes a single history sync job bounded to single-page quanta.
type HistoryJobTask struct {
	runner *HistoryJobRunner
	job    storage.HistorySyncJob
	conn   storage.Connection

	initialized  bool
	fromDay      time.Time
	toDay        time.Time
	curDay       time.Time
	dayPageCount int
	maxDayPages  int
	nextAction   string
	nextFields   map[string]string
	dayTxns      []storage.BatchTransactionItem
	pagesDone    int
	rowsSeen     int

	canceled atomicBool
	done     chan struct{}
	stepErr  error
}

type atomicBool struct {
	val sync.Mutex
	b   bool
}

func (a *atomicBool) Set(v bool) {
	a.val.Lock()
	a.b = v
	a.val.Unlock()
}

func (a *atomicBool) Get() bool {
	a.val.Lock()
	defer a.val.Unlock()
	return a.b
}

func NewHistoryJobTask(runner *HistoryJobRunner, job storage.HistorySyncJob, conn storage.Connection) *HistoryJobTask {
	return &HistoryJobTask{
		runner:      runner,
		job:         job,
		conn:        conn,
		maxDayPages: 10,
		pagesDone:   job.PagesDone,
		rowsSeen:    job.RowsSeen,
		done:        make(chan struct{}),
	}
}

func (t *HistoryJobTask) ID() string                          { return "hist_job_" + t.job.ID }
func (t *HistoryJobTask) Kind() string                        { return "FILTER_HISTORY" }
func (t *HistoryJobTask) Priority() scheduler.UpstreamPriority { return scheduler.PriorityFilterHistory }
func (t *HistoryJobTask) Generation() int64                   { return t.job.Generation }
func (t *HistoryJobTask) CoalesceKey() string                 { return "hist_job_" + t.job.ID }

func (t *HistoryJobTask) Done() <-chan struct{} { return t.done }

func (t *HistoryJobTask) cancel() {
	t.canceled.Set(true)
}

func (t *HistoryJobTask) finish(err error) {
	t.stepErr = err
	select {
	case <-t.done:
	default:
		close(t.done)
	}
}

// Step executes at most one ACB history page request, ingests with FILTER_SYNC source,
// updates durable progress/coverage, and yields to the scheduler.
func (t *HistoryJobTask) Step(ctx context.Context) (scheduler.TaskStepResult, error) {
	// 1. Worker shutdown / context cancellation check: use independent bounded context to requeue
	if err := ctx.Err(); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = t.runner.store.RequeueHistorySyncJob(cleanupCtx, t.job.ID, "WORKER_SHUTDOWN", "Worker stopped during execution", time.Now().Add(5*time.Second))
		t.finish(err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeTransient}, err
	}

	// 2. Cancellation check
	if t.canceled.Get() {
		t.finish(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	dbJob, err := t.runner.store.GetHistorySyncJob(ctx, t.job.ID)
	if err != nil {
		t.finish(err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
	}
	if dbJob.Status == storage.HistoryJobStatusCanceled {
		t.finish(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	// 3. Generation fencing check
	conn, err := t.runner.store.Connection(ctx)
	if err != nil {
		t.finish(err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
	}
	if conn.State != "MONITORING" {
		notMonErr := errors.New("connection not in MONITORING state")
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = t.runner.store.RequeueHistorySyncJob(cleanupCtx, t.job.ID, "NOT_MONITORING", notMonErr.Error(), time.Now().Add(10*time.Second))
		t.finish(notMonErr)
		return scheduler.TaskStepResult{Done: true, Error: notMonErr, Outcome: scheduler.OutcomeTransient}, notMonErr
	}
	if conn.ID != t.job.ConnectionID || conn.Generation != t.job.Generation {
		staleErr := fmt.Errorf("%w: job gen %d != conn gen %d", storage.ErrGenerationFenceMismatch, t.job.Generation, conn.Generation)
		_ = t.runner.store.CancelHistorySyncJob(ctx, t.job.ID)
		t.finish(staleErr)
		return scheduler.TaskStepResult{Done: true, Error: staleErr, Outcome: scheduler.OutcomeSuccess}, staleErr
	}

	// 4. Circuit breaker / backoff check
	if t.runner.monitor != nil && t.runner.monitor.IsBackoffActive() {
		backoffUntil := t.runner.monitor.BackoffUntil()
		return scheduler.TaskStepResult{
			Done:      false,
			RequeueAt: backoffUntil,
			Outcome:   scheduler.OutcomeTransient,
		}, nil
	}

	// 5. Initialize date range bounds
	if !t.initialized {
		fromDayT, err := time.Parse("2006-01-02", t.job.RangeFrom)
		if err != nil {
			_ = t.runner.store.FailHistorySyncJob(ctx, t.job.ID, "INVALID_DATE", err.Error())
			t.finish(err)
			return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
		}
		toDayT, err := time.Parse("2006-01-02", t.job.RangeTo)
		if err != nil {
			_ = t.runner.store.FailHistorySyncJob(ctx, t.job.ID, "INVALID_DATE", err.Error())
			t.finish(err)
			return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
		}
		t.fromDay = fromDayT
		t.toDay = toDayT

		// Resume from current day if valid, otherwise start from range_from
		startDay := fromDayT
		if t.job.CurrentDay != "" {
			if parsedCur, err := time.Parse("2006-01-02", t.job.CurrentDay); err == nil && !parsedCur.Before(fromDayT) && !parsedCur.After(toDayT) {
				startDay = parsedCur
			}
		}
		t.curDay = startDay
		t.initialized = true
	}

	// 6. Fast-forward past fully covered days
	for !t.curDay.After(t.toDay) && t.dayPageCount == 0 {
		dayStr := t.curDay.Format("2006-01-02")
		covered, err := t.runner.store.CheckRangeCoverage(ctx, conn.ID, dayStr, dayStr)
		if err == nil && covered {
			t.curDay = t.curDay.AddDate(0, 0, 1)
			continue
		}
		break
	}

	// Check if entire requested range is complete
	if t.curDay.After(t.toDay) {
		if err := t.runner.store.CompleteHistorySyncJob(ctx, t.job.ID, t.pagesDone, t.rowsSeen); err != nil {
			t.finish(err)
			return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
		}
		t.finish(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	dayStr := t.curDay.Format("2006-01-02")

	// 7. Fresh day: bootstrap form tokens if not yet prepared
	if t.dayPageCount == 0 && t.nextAction == "" {
		if t.runner.sessions != nil {
			if err := t.runner.sessions.Restore(ctx, conn.ID, conn.Generation); err != nil {
				restoreErr := fmt.Errorf("restore ACB session: %w", err)
				_ = t.runner.store.FailHistorySyncJob(ctx, t.job.ID, "AUTH_REQUIRED", restoreErr.Error())
				t.finish(restoreErr)
				return scheduler.TaskStepResult{Done: true, Error: restoreErr, Outcome: scheduler.OutcomeAuth}, restoreErr
			}
		}

		resp, err := t.runner.client.Bootstrap(ctx)
		if err != nil {
			bootErr := fmt.Errorf("bootstrap ACB session: %w", err)
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = t.runner.store.RequeueHistorySyncJob(cleanupCtx, t.job.ID, "TRANSIENT_ERROR", bootErr.Error(), time.Now().Add(5*time.Second))
			t.finish(bootErr)
			return scheduler.TaskStepResult{Done: true, Error: bootErr, Outcome: scheduler.OutcomeTransient}, bootErr
		}

		if resp.Kind == acb.LoginPage || resp.Kind == acb.OTPChallenge || resp.Kind == acb.CaptchaPage {
			authErr := errors.New("ACB session expired or challenge required")
			_ = t.runner.store.FailHistorySyncJob(ctx, t.job.ID, "AUTH_REQUIRED", authErr.Error())
			t.finish(authErr)
			return scheduler.TaskStepResult{Done: true, Error: authErr, Outcome: scheduler.OutcomeAuth}, authErr
		}
		if resp.Kind == acb.MaintenancePage || resp.StatusCode == 429 {
			if t.runner.monitor != nil {
				t.runner.monitor.SetBackoff(60 * time.Second)
			}
			maintErr := errors.New("ACB maintenance or rate limit active")
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = t.runner.store.RequeueHistorySyncJob(cleanupCtx, t.job.ID, "TRANSIENT_ERROR", maintErr.Error(), time.Now().Add(60*time.Second))
			t.finish(maintErr)
			return scheduler.TaskStepResult{Done: true, Error: maintErr, Outcome: scheduler.OutcomeTransient}, maintErr
		}

		form, formErr := acb.ExtractHistoryForm(resp.Body)
		if formErr != nil {
			_ = t.runner.store.FailHistorySyncJob(ctx, t.job.ID, "FORM_EXTRACT_ERROR", formErr.Error())
			t.finish(formErr)
			return scheduler.TaskStepResult{Done: true, Error: formErr, Outcome: scheduler.OutcomeFatal}, formErr
		}

		if form.Fields["AccountNbr"] == "" && conn.AccountMasked != "" {
			form.Fields["AccountNbr"] = conn.AccountMasked
		}
		form.Fields["FromDate"] = t.curDay.Format("02/01/2006")
		form.Fields["ToDate"] = t.curDay.Format("02/01/2006")
		form.Fields["_explicitRange"] = "true"

		t.nextAction = form.Action
		t.nextFields = form.Fields
		t.dayTxns = nil
	}

	// 8. EXECUTE AT MOST ONE ACB History Page Request
	histResp, histErr := t.runner.client.History(ctx, t.nextAction, t.nextFields)
	if histErr != nil {
		qErr := fmt.Errorf("query ACB history for %s: %w", dayStr, histErr)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = t.runner.store.RequeueHistorySyncJob(cleanupCtx, t.job.ID, "TRANSIENT_ERROR", qErr.Error(), time.Now().Add(5*time.Second))
		t.finish(qErr)
		return scheduler.TaskStepResult{Done: true, Error: qErr, Outcome: scheduler.OutcomeTransient}, qErr
	}

	if histResp.Kind == acb.LoginPage || histResp.Kind == acb.OTPChallenge || histResp.Kind == acb.CaptchaPage {
		authErr := errors.New("ACB session expired or challenge required during history fetch")
		_ = t.runner.store.FailHistorySyncJob(ctx, t.job.ID, "AUTH_REQUIRED", authErr.Error())
		t.finish(authErr)
		return scheduler.TaskStepResult{Done: true, Error: authErr, Outcome: scheduler.OutcomeAuth}, authErr
	}
	if histResp.Kind == acb.MaintenancePage || histResp.StatusCode == 429 {
		if t.runner.monitor != nil {
			t.runner.monitor.SetBackoff(60 * time.Second)
		}
		maintErr := errors.New("ACB maintenance or rate limit active during history fetch")
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = t.runner.store.RequeueHistorySyncJob(cleanupCtx, t.job.ID, "TRANSIENT_ERROR", maintErr.Error(), time.Now().Add(60*time.Second))
		t.finish(maintErr)
		return scheduler.TaskStepResult{Done: true, Error: maintErr, Outcome: scheduler.OutcomeTransient}, maintErr
	}

	pageResult, parseErr := acb.ParseHistoryPage(histResp.Body)
	if parseErr != nil {
		pErr := fmt.Errorf("parse ACB history page for %s: %w", dayStr, parseErr)
		_ = t.runner.store.FailHistorySyncJob(ctx, t.job.ID, "PARSE_ERROR", pErr.Error())
		t.finish(pErr)
		return scheduler.TaskStepResult{Done: true, Error: pErr, Outcome: scheduler.OutcomeFatal}, pErr
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

	// 9. Ingest with FILTER_SYNC source: suppresses webhooks, Bark deliveries, and browser voice
	_, ingestErr := t.runner.store.IngestTransactionsBatchWithSource(ctx, conn.ID, conn.Generation, conn.AccountMasked, pageItems, false, "FILTER_SYNC")
	if ingestErr != nil {
		iErr := fmt.Errorf("ingest history page for %s: %w", dayStr, ingestErr)
		_ = t.runner.store.FailHistorySyncJob(ctx, t.job.ID, "INGEST_ERROR", iErr.Error())
		t.finish(iErr)
		return scheduler.TaskStepResult{Done: true, Error: iErr, Outcome: scheduler.OutcomeFatal}, iErr
	}

	t.pagesDone++
	t.rowsSeen += len(pageItems)
	t.dayPageCount++
	t.dayTxns = append(t.dayTxns, pageItems...)

	// 10. Check if more pages exist for this day
	if pageResult.HasNext && t.dayPageCount < t.maxDayPages && (pageResult.NextAction != "" || len(pageResult.NextFields) > 0) {
		t.nextAction = pageResult.NextAction
		t.nextFields = pageResult.NextFields
		if t.nextFields == nil {
			t.nextFields = make(map[string]string)
		}
		t.nextFields["_raw"] = "true"

		// Record intermediate progress & heartbeat
		_ = t.runner.store.RecordHistoryJobProgress(ctx, t.job.ID, dayStr, t.pagesDone, t.rowsSeen, false, 0)

		// Bounded quantum complete! Yield after 1 page so higher-priority tasks can preempt.
		return scheduler.TaskStepResult{Done: false, Outcome: scheduler.OutcomeSuccess}, nil
	}

	// 11. Day is complete! Mark day coverage and advance to next day
	dayRows := len(t.dayTxns)
	if err := t.runner.store.RecordHistoryJobProgress(ctx, t.job.ID, dayStr, t.pagesDone, t.rowsSeen, true, dayRows); err != nil {
		t.finish(err)
		return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
	}

	t.curDay = t.curDay.AddDate(0, 0, 1)
	t.dayPageCount = 0
	t.nextAction = ""
	t.nextFields = nil
	t.dayTxns = nil

	if t.curDay.After(t.toDay) {
		// Entire date range complete!
		if err := t.runner.store.CompleteHistorySyncJob(ctx, t.job.ID, t.pagesDone, t.rowsSeen); err != nil {
			t.finish(err)
			return scheduler.TaskStepResult{Done: true, Error: err, Outcome: scheduler.OutcomeFatal}, err
		}
		t.finish(nil)
		return scheduler.TaskStepResult{Done: true, Outcome: scheduler.OutcomeSuccess}, nil
	}

	// Still more days in range: advance current_day in storage and yield to scheduler
	nextDayStr := t.curDay.Format("2006-01-02")
	_ = t.runner.store.RecordHistoryJobProgress(ctx, t.job.ID, nextDayStr, t.pagesDone, t.rowsSeen, false, 0)
	return scheduler.TaskStepResult{Done: false, Outcome: scheduler.OutcomeSuccess}, nil
}
