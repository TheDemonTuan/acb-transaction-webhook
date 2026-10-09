package bankdate

import (
	"testing"
)

func TestParseTransactionDate(t *testing.T) {
	loc := DefaultLocation

	tests := []struct {
		name          string
		raw           string
		wantDay       string
		wantHasTime   bool
		wantPrecision string
		wantErr       bool
	}{
		{
			name:          "Date only DD/MM/YYYY",
			raw:           "12/09/2026",
			wantDay:       "2026-09-12",
			wantHasTime:   false,
			wantPrecision: "date",
		},
		{
			name:          "Day 1 Month 2 must not be swapped to Month 1 Day 2",
			raw:           "01/02/2026",
			wantDay:       "2026-02-01",
			wantHasTime:   false,
			wantPrecision: "date",
		},
		{
			name:          "End of year",
			raw:           "31/12/2026",
			wantDay:       "2026-12-31",
			wantHasTime:   false,
			wantPrecision: "date",
		},
		{
			name:          "Leap day 29 Feb",
			raw:           "29/02/2024",
			wantDay:       "2024-02-29",
			wantHasTime:   false,
			wantPrecision: "date",
		},
		{
			name:          "Date and hour minute",
			raw:           "12/09/2026 09:15",
			wantDay:       "2026-09-12",
			wantHasTime:   true,
			wantPrecision: "datetime",
		},
		{
			name:          "Date with hour minute second",
			raw:           "12/09/2026 10:32:15",
			wantDay:       "2026-09-12",
			wantHasTime:   true,
			wantPrecision: "datetime",
		},
		{
			name:          "With surrounding whitespace",
			raw:           "  12/09/2026 10:32:15  \n",
			wantDay:       "2026-09-12",
			wantHasTime:   true,
			wantPrecision: "datetime",
		},
		{
			name:          "Defensive ISO format",
			raw:           "2026-09-12T10:32:15+07:00",
			wantDay:       "2026-09-12",
			wantHasTime:   true,
			wantPrecision: "datetime",
		},
		{
			name:          "Defensive ISO date only",
			raw:           "2026-09-12",
			wantDay:       "2026-09-12",
			wantHasTime:   false,
			wantPrecision: "date",
		},
		{
			name:    "Invalid format",
			raw:     "not-a-date",
			wantErr: true,
		},
		{
			name:    "Invalid day 32",
			raw:     "32/01/2026",
			wantErr: true,
		},
		{
			name:    "Empty string",
			raw:     "",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, hasTime, err := ParseTransactionDate(tc.raw, loc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.raw, err)
			}
			if hasTime != tc.wantHasTime {
				t.Errorf("hasTime mismatch: got %v, want %v", hasTime, tc.wantHasTime)
			}
			day := parsed.Format("2006-01-02")
			if day != tc.wantDay {
				t.Errorf("day mismatch: got %q, want %q", day, tc.wantDay)
			}

			norm, err := NormalizeDate(tc.raw, loc)
			if err != nil {
				t.Fatalf("NormalizeDate error: %v", err)
			}
			if norm.TransactionDay != tc.wantDay {
				t.Errorf("norm.TransactionDay mismatch: got %q, want %q", norm.TransactionDay, tc.wantDay)
			}
			if norm.DatePrecision != tc.wantPrecision {
				t.Errorf("norm.DatePrecision mismatch: got %q, want %q", norm.DatePrecision, tc.wantPrecision)
			}
		})
	}
}
