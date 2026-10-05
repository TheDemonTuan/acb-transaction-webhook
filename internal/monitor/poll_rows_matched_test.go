package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
	"github.com/thedemontuan/acb-transaction-webhook/internal/scheduler"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// The real Client validates pinned requests against the stateful bank fixture;
// only history row makeup changes. No production account/session is involved.
type matchedRowsBank struct {
	bank          *protocolRecoveryBank
	rawRows       int
	matchedRows   int
	malformedPage int
	invalidDay    bool
	sourceScoped  bool
	mismatchPage  int
	uniquePerPage bool
}

func (b *matchedRowsBank) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := b.bank.RoundTrip(request)
	if err != nil {
		return response, err
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	markup := string(body)
	if strings.Contains(markup, "<table>") {
		b.bank.mu.Lock()
		token, page, pages := b.bank.token(), b.bank.page, b.bank.pages
		b.bank.mu.Unlock()
		var rows strings.Builder
		rawRows := b.rawRows
		if b.sourceScoped && request.PostForm.Get("activeDatetimeYN") == "Y" && page != b.mismatchPage {
			rawRows = b.matchedRows
		}
		if page != b.malformedPage {
			for i := range rawRows {
				day := "04/10/2026"
				if i >= b.matchedRows && i%2 == 0 {
					day = "03/10/2026"
				}
				if i < b.matchedRows {
					day = protocolRecoveryDate
				}
				if b.invalidDay {
					day = "invalid"
				}
				// Usually repeat identities: matched counts are not insert counts.
				identity := fmt.Sprintf("MIX%d", i)
				if b.uniquePerPage {
					identity = fmt.Sprintf("MIX%d_%d", page, i)
				}
				fmt.Fprintf(&rows, `<tr><td>05/10/2026</td><td>%s</td><td>%s</td><td>0</td><td>100</td></tr>`, day, identity)
			}
			if rawRows == 0 {
				rows.WriteString(`<tr><td colspan="5">Không có giao dịch</td></tr>`)
			}
		}
		navigation := `<span class="disabled">Trang sau</span>`
		if page < pages {
			navigation = `<a href="/acbib/Request" onclick="submitEvent('nextPage')">Trang sau</a>`
		}
		total := rawRows
		if pages > 0 {
			total *= pages
		}
		markup = protocolRecoveryForm(token, "ibkacctDetailProc") + fmt.Sprintf(`<table><tr><th>Ngày hiệu lực</th><th>Ngày giao dịch</th><th>Số GD</th><th>Ghi nợ</th><th>Ghi có</th></tr>%s<tr><td colspan="5">%s</td></tr></table><div>Tổng số dòng: %d</div>`, rows.String(), navigation, total)
	}
	response.Body = io.NopCloser(strings.NewReader(markup))
	return response, nil
}

func matchedRowsPoll(t *testing.T, mon *Monitor, conn storage.Connection) storage.PollRun {
	t.Helper()
	task := NewRealtimeTask(mon, PriorityRealtimePoll, conn.ID, conn.Generation)
	for range realtimeMaxPages + 1 {
		result, err := task.Step(context.Background())
		if err != nil {
			t.Fatalf("real Client poll failed: %+v %v", result, err)
		}
		if result.Done {
			polls, err := mon.store.ListPollRuns(context.Background(), 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, poll := range polls {
				if poll.ID == task.poll.ID {
					return poll
				}
			}
			t.Fatalf("finished poll not persisted: %s", task.poll.ID)
		}
	}
	t.Fatal("poll failed to reach bounded completion")
	return storage.PollRun{}
}

func assertMatchedRows(t *testing.T, poll storage.PollRun, status string, raw int, matched *int) {
	t.Helper()
	if poll.Status != status || poll.RowsSeen != raw || (poll.RowsMatched == nil) != (matched == nil) || matched != nil && *poll.RowsMatched != *matched {
		t.Fatalf("poll row metrics: %+v expected status=%s raw=%d matched=%v", poll, status, raw, matched)
	}
}

// Run this test as the real Client + temporary SQLite Store smoke path.
func TestRealtimeTodaySourceQueryRealClientStore(t *testing.T) {
	baselineBank := &matchedRowsBank{bank: &protocolRecoveryBank{sourceMode: "N"}, rawRows: 180, matchedRows: 22, sourceScoped: true}
	_, _, _, baselineClient, _, _ := protocolRecoveryFixture(t, baselineBank)
	snapshot, err := baselineClient.SnapshotSession()
	if err != nil {
		t.Fatal(err)
	}
	fields := snapshot.Fields
	fields["_explicitRange"] = "true"
	fields["FromDate"], fields["ToDate"] = protocolRecoveryDate, protocolRecoveryDate
	baselineResponse, err := baselineClient.History(context.Background(), snapshot.Action, fields)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := acb.ParseHistoryPage(baselineResponse.Body)
	if err != nil || len(baseline.Transactions) != 180 {
		t.Fatalf("historical N source baseline: rows=%d err=%v", len(baseline.Transactions), err)
	}
	todayRows, err := acb.FilterHistoryTransactionDay(baseline.Transactions, protocolRecoveryDate)
	if err != nil || len(todayRows) != 22 {
		t.Fatalf("historical N baseline today identities: rows=%d err=%v", len(todayRows), err)
	}
	if baselineBank.bank.failure != "" {
		t.Fatal(baselineBank.bank.failure)
	}
	bank := &matchedRowsBank{bank: &protocolRecoveryBank{}, rawRows: 180, matchedRows: 22, sourceScoped: true}
	store, conn, _, _, _, mon := protocolRecoveryFixture(t, bank)
	want := 22
	first := matchedRowsPoll(t, mon, conn)
	assertMatchedRows(t, first, "SUCCEEDED", 22, &want)
	if bank.bank.byDateCalls != 1 {
		t.Fatalf("proved one-page bootstrap repeated its source query: %d page-one POSTs", bank.bank.byDateCalls)
	}
	second := matchedRowsPoll(t, mon, conn)
	assertMatchedRows(t, second, "SUCCEEDED", 22, &want)
	if bank.bank.byDateCalls != 2 {
		t.Fatalf("duplicate poll repeated its source query: %d page-one POSTs", bank.bank.byDateCalls)
	}
	if count := protocolRecoveryCount(t, store, "transactions"); count != 22 {
		t.Fatalf("mixed dates or duplicates ingested: %d", count)
	}
	if count := protocolRecoveryCount(t, store, "events"); count != 22 {
		t.Fatalf("deduped credit event count: %d", count)
	}
	transactions, err := store.ListTransactions(context.Background(), 100)
	if err != nil || len(transactions) != len(todayRows) {
		t.Fatalf("Today source lost baseline identities: rows=%d err=%v", len(transactions), err)
	}
	identities := make(map[string]bool, len(todayRows))
	for _, row := range todayRows {
		identities["ACB:"+row.Number] = true
	}
	for _, transaction := range transactions {
		if !identities[transaction.SemanticKey] || transaction.TransactionDay != "2026-10-05" || transaction.EffectiveAt != protocolRecoveryDate || transaction.Credit != 100 {
			t.Fatalf("Today source altered a baseline identity: %+v", transaction)
		}
		delete(identities, transaction.SemanticKey)
	}
	if len(identities) != 0 {
		t.Fatalf("Today source omitted baseline identities: %v", identities)
	}
	var exactEvents int
	if err := store.DB().QueryRow(`SELECT count(*) FROM events e JOIN transactions t ON t.id=e.transaction_id WHERE e.event_type='bank.transaction.credit' AND t.semantic_key LIKE 'ACB:MIX%'`).Scan(&exactEvents); err != nil || exactEvents != 22 {
		t.Fatalf("Today source credit event identities: count=%d err=%v", exactEvents, err)
	}
	// A stale incomplete bootstrap exercises recovery before consumer parsing.
	bank.bank.mu.Lock()
	bank.bank.state++
	bank.bank.incompleteDetail = true
	bank.bank.mu.Unlock()
	recovered := matchedRowsPoll(t, mon, conn)
	assertMatchedRows(t, recovered, "SUCCEEDED", 22, &want)
	if bank.bank.failure != "" {
		t.Fatal(bank.bank.failure)
	}
	if bank.bank.byDateCalls != 3 {
		t.Fatalf("stale recovery repeated its proved source query: %d page-one POSTs", bank.bank.byDateCalls)
	}
	gets := 0
	for _, phase := range bank.bank.events {
		if phase == "get" {
			gets++
		}
	}
	if gets != 1 {
		t.Fatalf("stale recovery must use one bounded form probe: %v", bank.bank.events)
	}
	bank.rawRows, bank.matchedRows = 0, 0
	empty := matchedRowsPoll(t, mon, conn)
	zero := 0
	assertMatchedRows(t, empty, "SUCCEEDED", 0, &zero)
	bank.bank.repeatDetail = true
	unavailable := matchedRowsPoll(t, mon, conn)
	assertMatchedRows(t, unavailable, "PARTIAL", 0, nil)
	if unavailable.Error != "HISTORY_UNAVAILABLE" {
		t.Fatalf("unexpected unavailable boundary: %+v", unavailable)
	}
	final, err := store.Connection(context.Background())
	if err != nil || final.State != "MONITORING" || final.Generation != conn.Generation {
		t.Fatalf("metric changed session/account fence: %+v %v", final, err)
	}
	if protocolRecoveryCount(t, store, "transactions") != 22 || protocolRecoveryCount(t, store, "events") != 22 {
		t.Fatal("empty/unavailable poll changed ingestion")
	}
}

func TestRealtimeRowsMatchedPaginationPartialAndReset(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pages         int
		malformedPage int
		status        string
		raw           int
		matched       int
		known         bool
	}{
		{"foreground complete", 2, 0, "SUCCEEDED", 4, 4, true},
		{"scheduler continuation complete", 6, 0, "SUCCEEDED", 12, 12, true},
		{"foreground partial", 2, 2, "PARTIAL", 2, 2, true},
		{"continuation partial", 6, 6, "PARTIAL", 10, 10, true},
		{"first page unparsed", 1, 1, "PARTIAL", 0, 0, false},
		{"hard page budget", 21, 0, "PARTIAL", 40, 40, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bank := &matchedRowsBank{bank: &protocolRecoveryBank{pages: tc.pages}, rawRows: 2, matchedRows: 2, malformedPage: tc.malformedPage}
			store, conn, _, _, _, mon := protocolRecoveryFixture(t, bank)
			poll := matchedRowsPoll(t, mon, conn)
			var want *int
			if tc.known {
				want = &tc.matched
			}
			assertMatchedRows(t, poll, tc.status, tc.raw, want)
			if bank.bank.failure != "" {
				t.Fatal(bank.bank.failure)
			}
			if tc.known && protocolRecoveryCount(t, store, "transactions") != 2 {
				t.Fatal("matched count was confused with deduped inserts")
			}
			// The following full poll starts from zero rather than carrying partials.
			bank.bank.pages = 1
			bank.malformedPage = 0
			reset := matchedRowsPoll(t, mon, conn)
			two := 2
			assertMatchedRows(t, reset, "SUCCEEDED", 2, &two)
			if protocolRecoveryCount(t, store, "transactions") != 2 {
				t.Fatal("next poll bypassed dedupe")
			}
		})
	}
}

func TestRealtimeRowsMatchedRequiresValidDayFilter(t *testing.T) {
	bank := &matchedRowsBank{bank: &protocolRecoveryBank{}, rawRows: 2, matchedRows: 1, invalidDay: true}
	store, conn, _, _, _, mon := protocolRecoveryFixture(t, bank)
	poll := matchedRowsPoll(t, mon, conn)
	assertMatchedRows(t, poll, "PARTIAL", 0, nil)
	if protocolRecoveryCount(t, store, "transactions") != 0 {
		t.Fatal("invalid day bypassed ingestion fence")
	}
}

func TestNonRealtimePollRowsMatchedUnknown(t *testing.T) {
	bank := &protocolRecoveryBank{}
	store, conn, _, _, _, mon := protocolRecoveryFixture(t, bank)
	keepalive := NewKeepaliveTask(mon, conn.ID, conn.Generation)
	result, err := keepalive.Step(context.Background())
	if err != nil || !result.Done || result.Outcome != scheduler.OutcomeSuccess {
		t.Fatalf("keepalive: %+v %v", result, err)
	}
	polls, err := store.ListPollRuns(context.Background(), 10)
	if err != nil || len(polls) != 1 || polls[0].RowsMatched != nil {
		t.Fatalf("nonrealtime metric must stay unknown: %+v %v", polls, err)
	}
	if bank.failure != "" {
		t.Fatal(bank.failure)
	}
}

func TestRealtimeTodaySourceMismatchIsPartial(t *testing.T) {
	for _, tc := range []struct {
		name string
		page int
	}{{"first page", 1}, {"foreground page2", 2}, {"yielded page6", 6}} {
		t.Run(tc.name, func(t *testing.T) {
			bank := &matchedRowsBank{bank: &protocolRecoveryBank{pages: tc.page}, rawRows: 180, matchedRows: 22, sourceScoped: true, mismatchPage: tc.page, uniquePerPage: true}
			store, conn, _, _, _, mon := protocolRecoveryFixture(t, bank)
			if _, err := store.DB().Exec(`INSERT INTO checkpoints(connection_id,scan_id,coverage_from,coverage_to,updated_at) VALUES(?,'prior-scan','2026-10-01','2026-10-04','2026-10-04T00:00:00Z')`, conn.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec(`INSERT INTO history_coverage(id,connection_id,day,status,last_sync_at,rows_seen) VALUES('prior-coverage',?,'2026-10-04','COMPLETE','2026-10-04T00:00:00Z',7)`, conn.ID); err != nil {
				t.Fatal(err)
			}
			poll := matchedRowsPoll(t, mon, conn)
			previous := 22 * (tc.page - 1)
			matched := previous + 22
			assertMatchedRows(t, poll, "PARTIAL", previous+180, &matched)
			if poll.Error != "REALTIME_SOURCE_DATE_MISMATCH" || poll.Pages != tc.page {
				t.Fatalf("source violation boundary: %+v", poll)
			}
			if bank.bank.failure != "" {
				t.Fatal(bank.bank.failure)
			}
			for _, table := range []string{"transactions", "events"} {
				if count := protocolRecoveryCount(t, store, table); count != previous {
					t.Fatalf("violating page changed %s: count=%d previous=%d", table, count, previous)
				}
			}
			var violatingRows int
			if err := store.DB().QueryRow("SELECT count(*) FROM transactions WHERE semantic_key LIKE ?", fmt.Sprintf("ACB:MIX%d_%%", tc.page)).Scan(&violatingRows); err != nil || violatingRows != 0 {
				t.Fatalf("violating page ingested matching rows: count=%d err=%v", violatingRows, err)
			}
			checkpoint, err := store.GetCheckpoint(context.Background(), conn.ID)
			if err != nil || checkpoint == nil || checkpoint.ScanID != "prior-scan" || checkpoint.CoverageTo != "2026-10-04" || checkpoint.UpdatedAt != "2026-10-04T00:00:00Z" {
				t.Fatalf("source mismatch advanced checkpoint: %+v %v", checkpoint, err)
			}
			var coverageDay, coverageStatus, coverageUpdated string
			var coverageRows int
			if protocolRecoveryCount(t, store, "history_coverage") != 1 || protocolRecoveryCount(t, store, "checkpoints") != 1 {
				t.Fatal("source mismatch committed incomplete coverage")
			}
			if err := store.DB().QueryRow("SELECT day,status,last_sync_at,rows_seen FROM history_coverage WHERE connection_id=?", conn.ID).Scan(&coverageDay, &coverageStatus, &coverageUpdated, &coverageRows); err != nil || coverageDay != "2026-10-04" || coverageStatus != "COMPLETE" || coverageUpdated != "2026-10-04T00:00:00Z" || coverageRows != 7 {
				t.Fatalf("source mismatch changed historical coverage: day=%s status=%s rows=%d err=%v", coverageDay, coverageStatus, coverageRows, err)
			}
			connection, err := store.Connection(context.Background())
			if err != nil || connection.State != "MONITORING" || connection.Generation != conn.Generation || mon.IsBackoffActive() {
				t.Fatalf("source mismatch changed auth/backoff: %+v %v", connection, err)
			}
			bank.mismatchPage = 0
			compliant := matchedRowsPoll(t, mon, conn)
			complete := 22 * tc.page
			assertMatchedRows(t, compliant, "SUCCEEDED", complete, &complete)
			for _, table := range []string{"transactions", "events"} {
				if count := protocolRecoveryCount(t, store, table); count != complete {
					t.Fatalf("compliant poll lost preserved identities or bypassed dedupe in %s: %d", table, count)
				}
			}
		})
	}
}
