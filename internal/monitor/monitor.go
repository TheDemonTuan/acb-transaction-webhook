package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
)

type BankClient interface {
	Bootstrap(ctx context.Context) (acb.Response, error)
	History(ctx context.Context, endpoint string, fields map[string]string) (acb.Response, error)
}

var (
	ErrSyncUnavailable   = errors.New("sync unavailable")
	ErrCatchUpIncomplete = errors.New("catch-up history sync incomplete")
)

type syncRequest struct {
	connectionID string
	generation   int64
}

type Monitor struct {
	store           *storage.Store
	client          BankClient
	sessions        *SessionLoader
	pollMinInterval time.Duration
	pollMaxInterval time.Duration
	nextInterval    func(time.Duration, time.Duration) time.Duration
	mu              sync.Mutex
	syncMu          sync.Mutex
	syncReq         *syncRequest
	syncCh          chan struct{}
	settingsCh      chan struct{}
	cachedSettings  storage.MonitorSettings
	lastMode        storage.PollMode
	catchUpPending  bool
	historyGroup    Group
	onNewEvents     func([]storage.EventNotification)
	onPollFinished  func(poll storage.PollRun, insertedCount int)
	backoffUntil    time.Time
}

func (m *Monitor) WithEventNotifier(fn func([]storage.EventNotification)) *Monitor {
	m.onNewEvents = fn
	return m
}

func (m *Monitor) WithPollNotifier(fn func(poll storage.PollRun, insertedCount int)) *Monitor {
	m.onPollFinished = fn
	return m
}

func (m *Monitor) finishPoll(ctx context.Context, poll storage.PollRun, insertedCount int) error {
	err := m.store.FinishPoll(ctx, poll)
	if m.onPollFinished != nil {
		m.onPollFinished(poll, insertedCount)
	}
	return err
}

func (m *Monitor) UpstreamGate() *sync.Mutex {
	return &m.mu
}

func New(store *storage.Store, client BankClient, minInterval, maxInterval time.Duration) *Monitor {
	if minInterval < 2*time.Second {
		minInterval = 5 * time.Second
	}
	if maxInterval < minInterval {
		maxInterval = minInterval
	}
	return &Monitor{
		store:           store,
		client:          client,
		pollMinInterval: minInterval,
		pollMaxInterval: maxInterval,
		nextInterval: func(minimum, maximum time.Duration) time.Duration {
			if maximum <= minimum {
				return minimum
			}
			return minimum + time.Duration(rand.Int64N(int64(maximum-minimum)+1))
		},
		syncCh:         make(chan struct{}, 1),
		settingsCh:     make(chan struct{}, 1),
		cachedSettings: storage.DefaultMonitorSettings,
	}
}

func (m *Monitor) NotifySettingsChanged() {
	select {
	case m.settingsCh <- struct{}{}:
	default:
	}
}

func (m *Monitor) WithSessionLoader(loader *SessionLoader) *Monitor {
	m.sessions = loader
	return m
}

// RequestSync enqueues a bounded manual sync request if the connection is currently in MONITORING state.
func (m *Monitor) RequestSync(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	conn, err := m.store.Connection(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrSyncUnavailable
		}
		return err
	}
	if conn.State != "MONITORING" {
		return ErrSyncUnavailable
	}

	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	if m.syncReq != nil && m.syncReq.connectionID == conn.ID && m.syncReq.generation == conn.Generation {
		return nil
	}

	m.syncReq = &syncRequest{
		connectionID: conn.ID,
		generation:   conn.Generation,
	}
	if m.syncCh == nil {
		m.syncCh = make(chan struct{}, 1)
	}
	select {
	case m.syncCh <- struct{}{}:
	default:
	}
	return nil
}

type fetchHistoryResult struct {
	Transactions []storage.BatchTransactionItem
	PagesFetched int
	Complete     bool
	Truncated    bool
	LastResponse acb.Response
}

func (m *Monitor) fetchHistoryRange(ctx context.Context, conn *storage.Connection, bootstrapResp acb.Response, fromDateStr, toDateStr string, maxPages int) (fetchHistoryResult, error) {
	if maxPages <= 0 {
		maxPages = 10
	}

	form, formErr := acb.ExtractHistoryForm(bootstrapResp.Body)
	if formErr != nil {
		return fetchHistoryResult{}, fmt.Errorf("extract history form: %w", formErr)
	}
	if form.Fields["AccountNbr"] == "" && conn.AccountMasked != "" {
		form.Fields["AccountNbr"] = conn.AccountMasked
	}

	action := form.Action
	fields := form.Fields
	if fromDateStr != "" && toDateStr != "" {
		fromT, err := time.Parse("2006-01-02", fromDateStr)
		if err != nil {
			return fetchHistoryResult{}, fmt.Errorf("invalid from date: %w", err)
		}
		toT, err := time.Parse("2006-01-02", toDateStr)
		if err != nil {
			return fetchHistoryResult{}, fmt.Errorf("invalid to date: %w", err)
		}
		fields["FromDate"] = fromT.Format("02/01/2006")
		fields["ToDate"] = toT.Format("02/01/2006")
		fields["_explicitRange"] = "true"
	}

	seenTxnNumbers := make(map[string]struct{})
	var allBatchItems []storage.BatchTransactionItem
	pagesFetched := 0
	maxTotalRowsSeen := 0
	complete := false
	truncated := false
	var lastResp acb.Response

	for pagesFetched < maxPages {
		if err := ctx.Err(); err != nil {
			return fetchHistoryResult{Transactions: allBatchItems, PagesFetched: pagesFetched, Complete: false}, err
		}

		histResp, histErr := m.client.History(ctx, action, fields)
		if histErr != nil {
			return fetchHistoryResult{Transactions: allBatchItems, PagesFetched: pagesFetched, Complete: false}, fmt.Errorf("query ACB history: %w", histErr)
		}
		lastResp = histResp
		pagesFetched++

		if histResp.Kind == acb.LoginPage || histResp.Kind == acb.OTPChallenge || histResp.Kind == acb.CaptchaPage {
			return fetchHistoryResult{Transactions: allBatchItems, PagesFetched: pagesFetched, Complete: false}, errors.New("ACB session expired or challenge required during query")
		}
		if histResp.Kind == acb.MaintenancePage {
			m.backoffUntil = time.Now().Add(60 * time.Second)
			return fetchHistoryResult{Transactions: allBatchItems, PagesFetched: pagesFetched, Complete: false}, errors.New("ACB maintenance")
		}

		pageResult, parseErr := acb.ParseHistoryPage(histResp.Body)
		if parseErr != nil {
			return fetchHistoryResult{Transactions: allBatchItems, PagesFetched: pagesFetched, Complete: false}, fmt.Errorf("parse ACB history page %d: %w", pagesFetched, parseErr)
		}

		for _, txn := range pageResult.Transactions {
			if _, seen := seenTxnNumbers[txn.Number]; !seen {
				seenTxnNumbers[txn.Number] = struct{}{}
				allBatchItems = append(allBatchItems, storage.BatchTransactionItem{
					Number:        txn.Number,
					Credit:        txn.Credit,
					Debit:         txn.Debit,
					Balance:       txn.Balance,
					TransactionAt: txn.TransactionAt,
					EffectiveAt:   txn.EffectiveDate,
					Description:   txn.Description,
				})
			}
		}

		if pageResult.TotalRows > maxTotalRowsSeen {
			maxTotalRowsSeen = pageResult.TotalRows
			neededPages := (maxTotalRowsSeen + 9) / 10
			if neededPages > 50 {
				neededPages = 50
			}
			if neededPages > maxPages {
				maxPages = neededPages
			}
		}

		if !pageResult.HasNext {
			if maxTotalRowsSeen > 0 && len(allBatchItems) < maxTotalRowsSeen {
				truncated = true
				complete = false
				slog.Warn("ACB history indicates truncated rows without next navigation", "page", pagesFetched, "cumulative_rows_seen", len(allBatchItems), "total_expected", maxTotalRowsSeen)
			} else {
				complete = true
			}
			break
		}

		if pageResult.NextAction == "" && len(pageResult.NextFields) == 0 {
			slog.Warn("ACB history page indicates more records exist but no navigation available", "page", pagesFetched, "rows_so_far", len(allBatchItems))
			complete = false
			break
		}

		action = pageResult.NextAction
		fields = pageResult.NextFields
		if fields == nil {
			fields = make(map[string]string)
		}
		fields["_raw"] = "true"
	}

	return fetchHistoryResult{
		Transactions: allBatchItems,
		PagesFetched: pagesFetched,
		Complete:     complete,
		Truncated:    truncated,
		LastResponse: lastResp,
	}, nil
}

// EnsureHistory ensures ACB transaction history for the requested [fromDay, toDay] date range is synchronized.
// It coalesces concurrent requests, checks cached coverage with TTLs, pushes date filters to ACB, and ingests with FILTER_SYNC source.
func (m *Monitor) EnsureHistory(ctx context.Context, fromDay, toDay string) (int, error) {
	fromT, err := time.Parse("2006-01-02", fromDay)
	if err != nil {
		return 0, fmt.Errorf("invalid from date: %w", err)
	}
	toT, err := time.Parse("2006-01-02", toDay)
	if err != nil {
		return 0, fmt.Errorf("invalid to date: %w", err)
	}
	if fromT.After(toT) {
		return 0, errors.New("from date must not be after to date")
	}
	if toT.Sub(fromT) > 31*24*time.Hour {
		return 0, errors.New("range too large (max 31 days)")
	}

	conn, err := m.store.Connection(ctx)
	if err != nil {
		return 0, err
	}
	if conn.State != "MONITORING" {
		return 0, errors.New("bank connection is not in MONITORING state")
	}

	// 1. Fast cache check
	covered, err := m.store.CheckRangeCoverage(ctx, conn.ID, fromDay, toDay)
	if err == nil && covered {
		return 0, nil
	}

	// 2. Coalescing single-flight group
	flightKey := fmt.Sprintf("%s:%s:%s", conn.ID, fromDay, toDay)
	val, err := m.historyGroup.Do(flightKey, func() (any, error) {
		// Re-check coverage under flight
		if cov, _ := m.store.CheckRangeCoverage(ctx, conn.ID, fromDay, toDay); cov {
			return 0, nil
		}

		m.mu.Lock()
		defer m.mu.Unlock()

		if time.Now().Before(m.backoffUntil) {
			return 0, errors.New("circuit breaker backoff active")
		}

		if m.client == nil {
			return 0, errors.New("bank client not configured")
		}
		if m.sessions != nil {
			if err := m.sessions.Restore(ctx, conn.ID, conn.Generation); err != nil {
				return 0, fmt.Errorf("restore ACB session: %w", err)
			}
		}

		resp, err := m.client.Bootstrap(ctx)
		if err != nil {
			return 0, fmt.Errorf("bootstrap ACB session: %w", err)
		}
		if resp.Kind == acb.LoginPage || resp.Kind == acb.OTPChallenge || resp.Kind == acb.CaptchaPage {
			return 0, errors.New("ACB session expired or challenge required")
		}
		if resp.Kind == acb.MaintenancePage {
			m.backoffUntil = time.Now().Add(60 * time.Second)
			return 0, errors.New("ACB maintenance")
		}

		fetchRes, fetchErr := m.fetchHistoryRange(ctx, &conn, resp, fromDay, toDay, 10)
		if fetchErr != nil {
			return 0, fetchErr
		}

		// Ingest with FILTER_SYNC source (suppressing webhooks and voice)
		_, ingestErr := m.store.IngestTransactionsBatchWithSource(ctx, conn.ID, conn.Generation, conn.AccountMasked, fetchRes.Transactions, false, "FILTER_SYNC")
		if ingestErr != nil {
			return 0, fmt.Errorf("ingest history transactions: %w", ingestErr)
		}

		if !fetchRes.Complete {
			return len(fetchRes.Transactions), fmt.Errorf("ACB history range incomplete: fetched %d pages (%d rows) but more rows remain", fetchRes.PagesFetched, len(fetchRes.Transactions))
		}

		dayCounts := make(map[string]int)
		for cur := fromT; !cur.After(toT); cur = cur.AddDate(0, 0, 1) {
			dayCounts[cur.Format("2006-01-02")] = 0
		}
		for _, item := range fetchRes.Transactions {
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
		if err := m.store.RecordCoveragePerDay(ctx, conn.ID, dayCounts); err != nil {
			return len(fetchRes.Transactions), fmt.Errorf("record coverage: %w", err)
		}

		return len(fetchRes.Transactions), nil
	})

	if err != nil {
		return 0, err
	}
	return val.(int), nil
}

// PollOnce executes a single poll cycle if the connection is in MONITORING state.
func (m *Monitor) PollOnce(ctx context.Context) error {
	return m.pollOnce(ctx, nil)
}

func (m *Monitor) pollOnce(ctx context.Context, expected *syncRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	conn, err := m.store.Connection(ctx)
	if err != nil {
		return err
	}
	if conn.State != "MONITORING" {
		return nil // Not in active monitoring state
	}

	if time.Now().Before(m.backoffUntil) {
		slog.Info("skipping poll: circuit breaker backoff active", "until", m.backoffUntil)
		return nil
	}

	hasActiveAttempt, err := m.store.HasActiveAuthAttempt(ctx, conn.ID)
	if err == nil && hasActiveAttempt {
		slog.Info("skipping poll: browser authentication in progress", "connection_id", conn.ID)
		return nil
	}

	if expected != nil {
		if conn.ID != expected.connectionID || conn.Generation != expected.generation {
			return nil // Stale sync skipped
		}
	}
	if m.client == nil {
		return errors.New("bank client not configured")
	}
	if m.sessions != nil {
		if err := m.sessions.Restore(ctx, conn.ID, conn.Generation); err != nil {
			return fmt.Errorf("restore ACB session: %w", err)
		}
	}

	poll, err := m.store.StartPoll(ctx)
	if err != nil {
		return fmt.Errorf("start poll: %w", err)
	}

	// 1. Fetch account detail page to verify session and extract form state
	resp, err := m.client.Bootstrap(ctx)
	if err != nil {
		poll.Status = "FAILED"
		poll.Error = err.Error()
		_ = m.finishPoll(ctx, poll, 0)
		return err
	}

	poll.Classifier = string(resp.Kind)
	poll.HTTPStatus = resp.StatusCode

	if resp.Kind == acb.LoginPage {
		poll.Status = "AUTH_REQUIRED"
		poll.Error = "SESSION_EXPIRED"
		_ = m.finishPoll(ctx, poll, 0)
		slog.Warn("ACB confirmed the session is no longer authenticated; transitioned to AUTH_REQUIRED")
		return nil
	}
	if resp.Kind == acb.OTPChallenge || resp.Kind == acb.CaptchaPage {
		poll.Status = "AUTH_REQUIRED"
		poll.Error = string(resp.Kind)
		_ = m.finishPoll(ctx, poll, 0)
		slog.Warn("ACB requires interactive authentication", "challenge", resp.Kind)
		return nil
	}

	if resp.StatusCode == 429 {
		poll.Status = "FAILED"
		poll.Error = "ACB_RATE_LIMITED"
		m.backoffUntil = time.Now().Add(60 * time.Second)
		telemetry.Default.SetCircuitBreaker(true)
		_ = m.finishPoll(ctx, poll, 0)
		slog.Warn("ACB rate limit detected (429); backoff for 60s")
		return nil
	}

	if resp.Kind == acb.MaintenancePage {
		poll.Status = "FAILED"
		poll.Error = "ACB_MAINTENANCE"
		m.backoffUntil = time.Now().Add(60 * time.Second)
		telemetry.Default.SetCircuitBreaker(true)
		_ = m.finishPoll(ctx, poll, 0)
		slog.Warn("ACB maintenance detected; backoff for 60s")
		return nil
	}

	// If the page is not HistoryPage directly, try extracting form state
	historyMarkup := resp.Body
	if resp.Kind != acb.HistoryPage {
		form, formErr := acb.ExtractHistoryForm(resp.Body)
		if formErr != nil {
			poll.Status = "FAILED"
			poll.Error = formErr.Error()
			_ = m.finishPoll(ctx, poll, 0)
			return formErr
		}

		if form.Fields["AccountNbr"] == "" && conn.AccountMasked != "" {
			form.Fields["AccountNbr"] = conn.AccountMasked
		}
		histResp, histErr := m.client.History(ctx, form.Action, form.Fields)
		if histErr != nil {
			poll.Status = "FAILED"
			poll.Error = histErr.Error()
			_ = m.finishPoll(ctx, poll, 0)
			return histErr
		}
		historyMarkup = histResp.Body
		poll.Classifier = string(histResp.Kind)
		poll.HTTPStatus = histResp.StatusCode

		if histResp.Kind == acb.LoginPage {
			poll.Status = "AUTH_REQUIRED"
			poll.Error = "SESSION_EXPIRED"
			_ = m.finishPoll(ctx, poll, 0)
			return nil
		}
		if histResp.Kind == acb.OTPChallenge || histResp.Kind == acb.CaptchaPage {
			poll.Status = "AUTH_REQUIRED"
			poll.Error = string(histResp.Kind)
			_ = m.finishPoll(ctx, poll, 0)
			return nil
		}
	}

	// Parse transaction history
	pageResult, parseErr := acb.ParseHistoryPage(historyMarkup)
	if parseErr != nil {
		poll.Status = "FAILED"
		poll.Error = parseErr.Error()
		_ = m.finishPoll(ctx, poll, 0)
		return parseErr
	}

	allTxns := pageResult.Transactions
	pagesCount := 1
	var pollErr error
	isPartial := false

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
				nextResp, nextErr := m.client.History(ctx, curAction, curFields)
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
				allTxns = append(allTxns, nextPage.Transactions...)
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
					slog.Warn("realtime poll reached page budget while more pages remain", "pages", pagesCount)
					isPartial = true
					break
				}
			}
		}
	}

	poll.RowsSeen = len(allTxns)
	poll.Pages = pagesCount
	slog.Info("ACB history parsed", "rows_seen", poll.RowsSeen, "pages", poll.Pages, "partial", isPartial)

	// Ingest transactions and emit credit events atomically in a single batch transaction
	batchItems := make([]storage.BatchTransactionItem, len(allTxns))
	for i, txn := range allTxns {
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
	batchRes, err := m.store.IngestTransactionsBatch(ctx, conn.ID, conn.Generation, conn.AccountMasked, batchItems, false)
	telemetry.Default.RecordIngest(time.Since(startIngest))
	telemetry.Default.SetLastACBPollAt(time.Now())
	if err != nil {
		if errors.Is(err, storage.ErrGenerationFenceMismatch) {
			slog.Warn("ingest rejected by generation fence", "error", err)
			poll.Status = "FAILED"
			poll.Error = err.Error()
			_ = m.finishPoll(ctx, poll, 0)
			return err
		}
		slog.Error("batch ingest failed", "error", err)
		poll.Status = "FAILED"
		poll.Error = err.Error()
		_ = m.finishPoll(ctx, poll, 0)
		return err
	}

	if isPartial {
		poll.Status = "PARTIAL"
		if pollErr != nil {
			poll.Error = pollErr.Error()
		} else {
			poll.Error = "PARTIAL_PAGE_BUDGET_REACHED"
		}
		m.catchUpPending = true
	} else {
		poll.Status = "SUCCEEDED"
	}
	m.backoffUntil = time.Time{}
	telemetry.Default.SetCircuitBreaker(false)
	if err := m.finishPoll(ctx, poll, batchRes.InsertedCount); err != nil {
		return err
	}

	if len(batchRes.NewEvents) > 0 && m.onNewEvents != nil {
		m.onNewEvents(batchRes.NewEvents)
	}
	if m.sessions != nil {
		if err := m.sessions.Persist(ctx, conn.ID, conn.Generation); err != nil {
			slog.Warn("could not persist refreshed ACB session", "error", err)
		}
	}
	return nil
}

func (m *Monitor) pollKeepalive(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	conn, err := m.store.Connection(ctx)
	if err != nil {
		return err
	}
	if conn.State != "MONITORING" {
		return nil
	}

	if time.Now().Before(m.backoffUntil) {
		return nil
	}

	if m.client == nil {
		return errors.New("bank client not configured")
	}
	if m.sessions != nil {
		if err := m.sessions.Restore(ctx, conn.ID, conn.Generation); err != nil {
			return fmt.Errorf("restore ACB session: %w", err)
		}
	}

	poll, err := m.store.StartPoll(ctx)
	if err != nil {
		return fmt.Errorf("start poll: %w", err)
	}

	// Bootstrap page only to keep session alive and rotate cookies - never calls History!
	resp, err := m.client.Bootstrap(ctx)
	if err != nil {
		poll.Status = "FAILED"
		poll.Error = err.Error()
		_ = m.finishPoll(ctx, poll, 0)
		return err
	}

	poll.Classifier = string(resp.Kind)
	poll.HTTPStatus = resp.StatusCode

	if resp.Kind == acb.LoginPage || resp.Kind == acb.OTPChallenge || resp.Kind == acb.CaptchaPage {
		poll.Status = "AUTH_REQUIRED"
		poll.Error = "SESSION_EXPIRED"
		_ = m.finishPoll(ctx, poll, 0)
		slog.Warn("ACB session expired during keepalive; transitioned to AUTH_REQUIRED")
		return nil
	}

	if resp.StatusCode == 429 {
		poll.Status = "FAILED"
		poll.Error = "ACB_RATE_LIMITED"
		m.backoffUntil = time.Now().Add(60 * time.Second)
		_ = m.finishPoll(ctx, poll, 0)
		return nil
	}

	if resp.Kind == acb.MaintenancePage {
		poll.Status = "FAILED"
		poll.Error = "ACB_MAINTENANCE"
		m.backoffUntil = time.Now().Add(60 * time.Second)
		_ = m.finishPoll(ctx, poll, 0)
		return nil
	}

	if m.sessions != nil {
		_ = m.sessions.Persist(ctx, conn.ID, conn.Generation)
	}

	poll.Status = "SUCCEEDED"
	poll.Pages = 0
	poll.RowsSeen = 0
	return m.finishPoll(ctx, poll, 0)
}

func (m *Monitor) catchUp(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	conn, err := m.store.Connection(ctx)
	if err != nil {
		return err
	}
	if conn.State != "MONITORING" {
		return nil
	}

	nowInLoc := time.Now().In(acb.DefaultLocation)
	today := nowInLoc.Format("2006-01-02")
	fromDate := nowInLoc.AddDate(0, 0, -1).Format("2006-01-02") // default yesterday

	// Determine catch-up start date from checkpoint or last coverage
	if cp, err := m.store.GetCheckpoint(ctx, conn.ID); err == nil && cp != nil && cp.CoverageTo != "" {
		fromDate = cp.CoverageTo
	}

	// Clamp max auto catch-up window to 7 days
	sevenDaysAgo := nowInLoc.AddDate(0, 0, -7).Format("2006-01-02")
	if fromDate < sevenDaysAgo {
		fromDate = sevenDaysAgo
	}
	if fromDate > today {
		fromDate = today
	}

	slog.Info("running catch-up history sync before resuming realtime", "from", fromDate, "to", today)

	if m.client == nil {
		return errors.New("bank client not configured")
	}
	if m.sessions != nil {
		if err := m.sessions.Restore(ctx, conn.ID, conn.Generation); err != nil {
			return fmt.Errorf("restore ACB session: %w", err)
		}
	}

	resp, err := m.client.Bootstrap(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap for catch-up: %w", err)
	}
	if resp.Kind == acb.LoginPage || resp.Kind == acb.OTPChallenge || resp.Kind == acb.CaptchaPage {
		return errors.New("session expired")
	}

	fromT, _ := time.Parse("2006-01-02", fromDate)
	toT, _ := time.Parse("2006-01-02", today)

	currentResp := resp
	totalInserted := 0
	totalSkipped := 0
	totalEmitted := 0

	for curDay := fromT; !curDay.After(toT); curDay = curDay.AddDate(0, 0, 1) {
		dayStr := curDay.Format("2006-01-02")

		// If historical day (before today) is already covered, we can safely advance checkpoint to dayStr
		if curDay.Before(toT) {
			if covered, err := m.store.CheckRangeCoverage(ctx, conn.ID, dayStr, dayStr); err == nil && covered {
				_ = m.store.SaveCheckpoint(ctx, storage.Checkpoint{
					ConnectionID: conn.ID,
					ScanID:       "scan_" + strconv.FormatInt(time.Now().Unix(), 10),
					CoverageFrom: fromDate,
					CoverageTo:   dayStr,
				})
				continue
			}
		}

		fetchRes, fetchErr := m.fetchHistoryRange(ctx, &conn, currentResp, dayStr, dayStr, 20)
		if fetchErr != nil {
			m.catchUpPending = true
			return fmt.Errorf("catch-up fetch for %s: %w", dayStr, fetchErr)
		}
		if fetchRes.LastResponse.Body != "" {
			if _, err := acb.ExtractHistoryForm(fetchRes.LastResponse.Body); err == nil {
				currentResp = fetchRes.LastResponse
			}
		}

		// Ingest with CATCH_UP source:
		// New credit transactions enqueue webhooks (no missed payments!), but voice is suppressed!
		res, err := m.store.IngestTransactionsBatchWithSource(ctx, conn.ID, conn.Generation, conn.AccountMasked, fetchRes.Transactions, false, "CATCH_UP")
		if err != nil {
			m.catchUpPending = true
			return fmt.Errorf("catch-up ingest for %s: %w", dayStr, err)
		}
		totalInserted += res.InsertedCount
		totalSkipped += res.SkippedCount
		if m.onNewEvents != nil && len(res.NewEvents) > 0 {
			m.onNewEvents(res.NewEvents)
			totalEmitted += len(res.NewEvents)
		}

		if !fetchRes.Complete {
			slog.Warn("catch-up for day incomplete; withholding checkpoint advancement to force retry", "day", dayStr, "pages", fetchRes.PagesFetched, "rows", len(fetchRes.Transactions))
			m.catchUpPending = true
			return ErrCatchUpIncomplete
		}

		// Mark this day's coverage as COMPLETE
		_ = m.store.RecordCoveragePerDay(ctx, conn.ID, map[string]int{dayStr: len(fetchRes.Transactions)})

		// Progressively advance checkpoint to this completed day
		if err := m.store.SaveCheckpoint(ctx, storage.Checkpoint{
			ConnectionID: conn.ID,
			ScanID:       "scan_" + strconv.FormatInt(time.Now().Unix(), 10),
			CoverageFrom: fromDate,
			CoverageTo:   dayStr,
		}); err != nil {
			slog.Warn("failed to save catch-up checkpoint", "day", dayStr, "error", err)
		}
	}

	m.catchUpPending = false
	if m.sessions != nil {
		_ = m.sessions.Persist(ctx, conn.ID, conn.Generation)
	}

	slog.Info("catch-up completed", "from", fromDate, "to", today, "inserted", totalInserted, "skipped", totalSkipped, "emitted", totalEmitted)
	return nil
}

func (m *Monitor) Run(ctx context.Context) {
	m.syncMu.Lock()
	if m.syncCh == nil {
		m.syncCh = make(chan struct{}, 1)
	}
	syncCh := m.syncCh
	m.syncMu.Unlock()

	// Initial load of settings from database
	if initSettings, err := m.store.GetMonitorSettings(ctx); err == nil {
		m.cachedSettings = initSettings
	}

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()

	for {
		schedule := storage.ResolveSchedule(time.Now(), &m.cachedSettings)

		// Transition check: transitioning into REALTIME (including startup when lastMode is empty) triggers catch-up!
		if (m.lastMode == storage.ModeKeepaliveOnly || m.lastMode == storage.ModePaused || m.lastMode == "") && schedule.Mode == storage.ModeRealtime {
			m.catchUpPending = true
		}
		m.lastMode = schedule.Mode

		wait := m.nextInterval(schedule.MinInterval, schedule.MaxInterval)
		if schedule.Mode == storage.ModePaused {
			wait = 10 * time.Minute
		}
		untilTrans := time.Until(schedule.NextTransition)
		if untilTrans > 0 && untilTrans < wait {
			wait = untilTrans
		}
		timer.Reset(wait)

		select {
		case <-ctx.Done():
			return

		case <-m.settingsCh:
			// Hot reload settings
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if s, err := m.store.GetMonitorSettings(ctx); err == nil {
				m.cachedSettings = s
				slog.Info("monitor schedule hot-reloaded", "revision", s.Revision, "enabled", s.Enabled)
			}
			continue

		case <-syncCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			m.syncMu.Lock()
			req := m.syncReq
			m.syncReq = nil
			m.syncMu.Unlock()
			if req != nil {
				if err := m.pollOnce(ctx, req); err != nil {
					slog.Warn("monitor sync poll completed with error", "error", err)
				}
			}

		case <-timer.C:
			if m.catchUpPending && schedule.Mode == storage.ModeRealtime {
				if err := m.catchUp(ctx); err != nil {
					if errors.Is(err, ErrCatchUpIncomplete) {
						slog.Warn("catch-up sync incomplete; retaining pending status for next cycle")
					} else {
						slog.Warn("catch-up sync failed; retrying before realtime polling", "error", err)
					}
					continue
				}
				m.catchUpPending = false
			}

			switch schedule.Mode {
			case storage.ModeRealtime:
				if err := m.pollOnce(ctx, nil); err != nil {
					slog.Warn("monitor poll cycle completed with error", "error", err)
				}
			case storage.ModeKeepaliveOnly:
				if err := m.pollKeepalive(ctx); err != nil {
					slog.Warn("monitor keepalive cycle completed with error", "error", err)
				}
			case storage.ModePaused:
				// No upstream request
			}
		}
	}
}
