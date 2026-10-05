package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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
		if page != b.malformedPage {
			for i := range b.rawRows {
				day := "04/10/2026"
				if i < b.matchedRows {
					day = protocolRecoveryDate
				}
				if b.invalidDay {
					day = "invalid"
				}
				// Repeat identities across pages/polls: matched count is not inserts.
				fmt.Fprintf(&rows, `<tr><td>05/10/2026</td><td>%s</td><td>MIX%d</td><td>0</td><td>100</td></tr>`, day, i)
			}
			if b.rawRows == 0 {
				rows.WriteString(`<tr><td colspan="5">Không có giao dịch</td></tr>`)
			}
		}
		navigation := `<span class="disabled">Trang sau</span>`
		if page < pages {
			navigation = `<a href="/acbib/Request" onclick="submitEvent('nextPage')">Trang sau</a>`
		}
		total := b.rawRows
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
func TestRealtimeRowsMatchedRealClientStoreSmoke(t *testing.T) {
	bank := &matchedRowsBank{bank: &protocolRecoveryBank{}, rawRows: 180, matchedRows: 22}
	store, conn, _, _, _, mon := protocolRecoveryFixture(t, bank)
	want := 22
	first := matchedRowsPoll(t, mon, conn)
	assertMatchedRows(t, first, "SUCCEEDED", 180, &want)
	second := matchedRowsPoll(t, mon, conn)
	assertMatchedRows(t, second, "SUCCEEDED", 180, &want)
	if count := protocolRecoveryCount(t, store, "transactions"); count != 22 {
		t.Fatalf("mixed dates or duplicates ingested: %d", count)
	}
	if count := protocolRecoveryCount(t, store, "events"); count != 22 {
		t.Fatalf("deduped credit event count: %d", count)
	}
	// A stale incomplete bootstrap exercises recovery before consumer parsing.
	bank.bank.mu.Lock()
	bank.bank.state++
	bank.bank.incompleteDetail = true
	bank.bank.mu.Unlock()
	recovered := matchedRowsPoll(t, mon, conn)
	assertMatchedRows(t, recovered, "SUCCEEDED", 180, &want)
	protocolRecoveryEvents(t, bank.bank, "post,post,post,post,stale,get,post,post")
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
		{"foreground complete", 2, 0, "SUCCEEDED", 4, 2, true},
		{"scheduler continuation complete", 6, 0, "SUCCEEDED", 12, 6, true},
		{"foreground partial", 2, 2, "PARTIAL", 2, 1, true},
		{"continuation partial", 6, 6, "PARTIAL", 10, 5, true},
		{"first page unparsed", 1, 1, "PARTIAL", 0, 0, false},
		{"hard page budget", 21, 0, "PARTIAL", 40, 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bank := &matchedRowsBank{bank: &protocolRecoveryBank{pages: tc.pages}, rawRows: 2, matchedRows: 1, malformedPage: tc.malformedPage}
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
			if tc.known && protocolRecoveryCount(t, store, "transactions") != 1 {
				t.Fatal("matched count was confused with deduped inserts")
			}
			// The following full poll starts from zero rather than carrying partials.
			bank.bank.pages = 1
			bank.malformedPage = 0
			reset := matchedRowsPoll(t, mon, conn)
			one := 1
			assertMatchedRows(t, reset, "SUCCEEDED", 2, &one)
			if protocolRecoveryCount(t, store, "transactions") != 1 {
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
	bank := &protocolRecoveryBank{requestDate: time.Now().In(acb.DefaultLocation).Format("02/01/2006")}
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
