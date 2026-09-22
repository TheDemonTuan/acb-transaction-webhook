package acb

import (
	"testing"
	"time"
)

func TestPrepareHistoryFieldsOverridesStaleMonthFilter(t *testing.T) {
	location := time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)
	fields, err := PrepareHistoryFields(map[string]string{
		"dse_operationName":     "ibkacctDetailProc",
		"dse_processorState":    "acctDetailPage",
		"dse_nextEventName":     "byMonth",
		"activeDatetimeYN":      "Y",
		"activeDatetimeByMonth": "Y",
		"MonthCurr":             "8",
		"YearCurr":              "2026",
		"FromDate":              "13/08/2026",
		"ToDate":                "12/09/2026",
		"CheckRef":              "true",
		"CheckDoiUng":           "true",
	}, time.Date(2026, 9, 12, 0, 30, 0, 0, location), location)
	if err != nil {
		t.Fatal(err)
	}
	if fields["dse_nextEventName"] != "byDate" || fields["activeDatetimeYN"] != "N" {
		t.Fatalf("wrong search mode: %#v", fields)
	}
	if fields["FromDate"] != "12/09/2026" || fields["ToDate"] != "12/09/2026" {
		t.Fatalf("wrong date range: %#v", fields)
	}
	if fields["CheckRef"] != "false" || fields["CheckDoiUng"] != "false" {
		t.Fatalf("unexpected optional filters: %#v", fields)
	}
	for _, name := range []string{"activeDatetimeByMonth", "MonthCurr", "YearCurr"} {
		if _, ok := fields[name]; ok {
			t.Fatalf("month-only field %s was retained", name)
		}
	}
}

func TestPrepareHistoryFieldsUsesVietnamCalendarAcrossUTCDate(t *testing.T) {
	location := time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)
	fields, err := PrepareHistoryFields(map[string]string{
		"dse_operationName":  "ibkacctDetailProc",
		"dse_processorState": "acctDetailPage",
	}, time.Date(2026, 12, 31, 18, 30, 0, 0, time.UTC), location)
	if err != nil {
		t.Fatal(err)
	}
	if fields["FromDate"] != "01/01/2027" || fields["ToDate"] != "01/01/2027" {
		t.Fatalf("wrong Vietnam date range: %#v", fields)
	}
}

func TestPinDateRangePreservingPagination(t *testing.T) {
	original := map[string]string{
		"dse_operationName":  "ibkacctDetailProc",
		"dse_processorState": "server-page-2",
		"dse_sessionId":      "session-2",
		"dse_nextEventName":  "nextPage",
		"_raw":               "true",
		"FromDate":           "01/09/2026",
		"ToDate":             "01/09/2026",
	}
	pinned, err := PinDateRangePreservingPagination(original, "12/09/2026", "12/09/2026")
	if err != nil {
		t.Fatal(err)
	}
	if pinned["FromDate"] != "12/09/2026" || pinned["ToDate"] != "12/09/2026" {
		t.Fatalf("wrong pinned range: %#v", pinned)
	}
	for _, key := range []string{"dse_operationName", "dse_processorState", "dse_sessionId", "dse_nextEventName", "_raw"} {
		if pinned[key] != original[key] {
			t.Fatalf("pagination field %q changed: got %q want %q", key, pinned[key], original[key])
		}
	}
	if original["FromDate"] != "01/09/2026" {
		t.Fatal("helper mutated input")
	}
}

func TestPrepareHistoryFieldsRejectsIncompleteExplicitRange(t *testing.T) {
	location := time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)
	for name, fields := range map[string]map[string]string{
		"missing from": {"_explicitRange": "true", "ToDate": "12/09/2026"},
		"missing to":   {"_explicitRange": "true", "FromDate": "12/09/2026"},
		"invalid from": {"_explicitRange": "true", "FromDate": "31/02/2026", "ToDate": "12/09/2026"},
		"invalid to":   {"_explicitRange": "true", "FromDate": "12/09/2026", "ToDate": "2026-09-12"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := PrepareHistoryFields(fields, time.Date(2026, 9, 12, 0, 0, 0, 0, location), location)
			if err == nil {
				t.Fatal("accepted incomplete or invalid explicit range")
			}
		})
	}
}

func TestPrepareHistoryFieldsRawPinsTodayAndPreservesNavigation(t *testing.T) {
	location := time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)
	fields, err := PrepareHistoryFields(map[string]string{
		"_raw":               "true",
		"dse_operationName":  "ibkacctDetailProc",
		"dse_processorState": "server-page-2",
		"dse_sessionId":      "session-2",
		"dse_nextEventName":  "nextPage",
		"FromDate":           "01/09/2026",
		"ToDate":             "01/09/2026",
	}, time.Date(2026, 9, 12, 0, 0, 0, 0, location), location)
	if err != nil {
		t.Fatal(err)
	}
	if fields["FromDate"] != "12/09/2026" || fields["ToDate"] != "12/09/2026" || fields["_raw"] != "" {
		t.Fatalf("raw request not pinned/stripped: %#v", fields)
	}
	if fields["dse_processorState"] != "server-page-2" || fields["dse_sessionId"] != "session-2" || fields["dse_nextEventName"] != "nextPage" {
		t.Fatalf("raw pagination state changed: %#v", fields)
	}
}

func TestPrepareHistoryFieldsPreservesExplicitRange(t *testing.T) {
	location := time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)
	fields, err := PrepareHistoryFields(map[string]string{
		"dse_operationName":  "ibkacctDetailProc",
		"dse_processorState": "acctDetailPage",
		"_explicitRange":     "true",
		"FromDate":           "01/09/2026",
		"ToDate":             "12/09/2026",
	}, time.Date(2026, 9, 30, 0, 0, 0, 0, location), location)
	if err != nil {
		t.Fatal(err)
	}
	if fields["FromDate"] != "01/09/2026" || fields["ToDate"] != "12/09/2026" || fields["_explicitRange"] != "" {
		t.Fatalf("explicit range changed: %#v", fields)
	}
}

func TestExtractHistoryForm(t *testing.T) {
	form, err := ExtractHistoryForm(`<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="fresh-state"><input name="dse_sessionId" value="rotated"></form>`)
	if err != nil {
		t.Fatal(err)
	}
	if form.Action != "/acbib/Request" || form.Fields["dse_sessionId"] != "rotated" {
		t.Fatalf("unexpected form %#v", form)
	}
}

func TestExtractHistoryFormRejectsMissingState(t *testing.T) {
	if _, err := ExtractHistoryForm(`<form action="/acbib/Request"><input name="dse_operationName" value="x"></form>`); err == nil {
		t.Fatal("accepted incomplete state")
	}
}

func TestExtractHistoryFormMultipleFormsAndSelect(t *testing.T) {
	html := `<html><body>
		<form id="headerSearch" action="/search"><input name="q" value="test"></form>
		<form id="mainAccount" action="/acbib/Request">
			<input name="dse_operationName" value="ibkacctDetailProc">
			<input name="dse_processorState" value="accountState">
			<select name="AccountNbr">
				<option value="11111111">Acc 1</option>
				<option value="40478827" selected>Acc 2 (Primary)</option>
			</select>
			<input name="FromDate" value="01/08/2026">
			<input name="ToDate" value="11/09/2026">
		</form>
		<form id="logoutForm" action="/logout"><button>Thoát</button></form>
	</body></html>`

	form, err := ExtractHistoryForm(html)
	if err != nil {
		t.Fatalf("ExtractHistoryForm failed: %v", err)
	}
	if form.Action != "/acbib/Request" {
		t.Fatalf("expected action /acbib/Request, got %q", form.Action)
	}
	if form.Fields["AccountNbr"] != "40478827" {
		t.Fatalf("expected AccountNbr 40478827, got %q", form.Fields["AccountNbr"])
	}
	if form.Fields["q"] != "" {
		t.Fatalf("expected q to be isolated, but got %q", form.Fields["q"])
	}
}
