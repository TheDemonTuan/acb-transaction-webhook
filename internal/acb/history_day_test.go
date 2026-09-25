package acb

import (
	"errors"
	"testing"
)

func TestHistoryDayDiagnosticsDoNotIncludeTransactionContents(t *testing.T) {
	counts := HistoryDayCounts([]Transaction{
		{TransactionAt: "25/09/2026 16:00:00", Number: "SENSITIVE", Description: "private"},
		{TransactionAt: "24/09/2026"},
		{TransactionAt: "25/09/2026"},
		{TransactionAt: "invalid"},
	})
	if len(counts) != 3 || counts[0] != (HistoryDayCount{Day: "2026-09-24", Rows: 1}) || counts[1] != (HistoryDayCount{Day: "2026-09-25", Rows: 2}) || counts[2] != (HistoryDayCount{Day: "INVALID", Rows: 1}) {
		t.Fatalf("unexpected safe day counts: %+v", counts)
	}
	effectiveCounts := HistoryEffectiveDayCounts([]Transaction{{EffectiveDate: "25/09/2026", Number: "SENSITIVE", Description: "private"}, {EffectiveDate: "invalid"}})
	if len(effectiveCounts) != 2 || effectiveCounts[0] != (HistoryDayCount{Day: "2026-09-25", Rows: 1}) || effectiveCounts[1] != (HistoryDayCount{Day: "INVALID", Rows: 1}) {
		t.Fatalf("unexpected safe effective day counts: %+v", effectiveCounts)
	}
	form := `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="opaque"><input name="FromDate" value="25/09/2026"><input name="ToDate" value="25/09/2026"></form>`
	if !HistoryFormDateMatches(form, "25/09/2026") || HistoryFormDateMatches(form, "20/09/2026") {
		t.Fatal("response form date comparison failed")
	}
}

func TestFilterHistoryTransactionDay(t *testing.T) {
	transactions := []Transaction{
		{TransactionAt: "24/09/2026 23:00:00", EffectiveDate: "25/09/2026", Number: "prior_txn"},
		{TransactionAt: "25/09/2026 08:00:00", EffectiveDate: "25/09/2026", Number: "today_1"},
		{TransactionAt: "25/09/2026 19:30:00", EffectiveDate: "26/09/2026", Number: "today_next_effective"},
	}
	filtered, err := FilterHistoryTransactionDay(transactions, "25/09/2026")
	if err != nil {
		t.Fatalf("unexpected filter error: %v", err)
	}
	if len(filtered) != 2 || filtered[0].Number != "today_1" || filtered[1].Number != "today_next_effective" {
		t.Fatalf("expected both today transactions regardless of effective day: %+v", filtered)
	}
	for _, invalid := range []Transaction{
		{TransactionAt: "25/09/2026", EffectiveDate: "invalid"},
		{TransactionAt: "invalid", EffectiveDate: "25/09/2026"},
	} {
		if _, err := FilterHistoryTransactionDay([]Transaction{invalid}, "25/09/2026"); !errors.Is(err, ErrHistoryDayMismatch) {
			t.Fatalf("expected malformed date rejection: %v", err)
		}
	}
}
