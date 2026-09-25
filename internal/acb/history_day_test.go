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
	form := `<form action="/acbib/Request"><input name="dse_operationName" value="ibkacctDetailProc"><input name="dse_processorState" value="opaque"><input name="FromDate" value="25/09/2026"><input name="ToDate" value="25/09/2026"></form>`
	if !HistoryFormDateMatches(form, "25/09/2026") || HistoryFormDateMatches(form, "20/09/2026") {
		t.Fatal("response form date comparison failed")
	}
}

func TestValidateHistoryTransactionDay(t *testing.T) {
	for _, test := range []struct {
		name string
		date string
		want error
	}{
		{"same day with time", "25/09/2026 16:00:00", nil},
		{"prior day", "24/09/2026", ErrHistoryDayMismatch},
		{"malformed", "unknown", ErrHistoryDayMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateHistoryTransactionDay([]Transaction{{TransactionAt: test.date}}, "25/09/2026")
			if !errors.Is(err, test.want) || (test.want == nil && err != nil) {
				t.Fatalf("unexpected validation result: %v", err)
			}
		})
	}
}
