package acb

import (
	"errors"
	"testing"
)

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
