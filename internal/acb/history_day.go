package acb

import (
	"errors"
	"sort"
)

var ErrHistoryDayMismatch = errors.New("ACB history response contains transactions outside requested day")

// FilterHistoryTransactionDay accepts only a scoped effective-date response and
// returns the transactions whose transaction date matches the requested day.
func FilterHistoryTransactionDay(transactions []Transaction, requested string) ([]Transaction, error) {
	if err := validateHistoryDateRange(requested, requested); err != nil {
		return nil, err
	}
	filtered := make([]Transaction, 0, len(transactions))
	for _, transaction := range transactions {
		day, err := NormalizeDate(transaction.TransactionAt, nil)
		if err != nil {
			return nil, ErrHistoryDayMismatch
		}
		if transaction.EffectiveDate != "" {
			effective, err := NormalizeDate(transaction.EffectiveDate, nil)
			if err != nil || effective.Time.Format(historyDateLayout) != requested {
				return nil, ErrHistoryDayMismatch
			}
		} else if day.Time.Format(historyDateLayout) != requested {
			return nil, ErrHistoryDayMismatch
		}
		if day.Time.Format(historyDateLayout) == requested {
			filtered = append(filtered, transaction)
		}
	}
	return filtered, nil
}

// ValidateHistoryTransactionDay rejects an unscoped or stale response before ingestion.
func ValidateHistoryTransactionDay(transactions []Transaction, requested string) error {
	if err := validateHistoryDateRange(requested, requested); err != nil {
		return err
	}
	for _, transaction := range transactions {
		day, err := NormalizeDate(transaction.TransactionAt, nil)
		if err != nil || day.Time.Format(historyDateLayout) != requested {
			return ErrHistoryDayMismatch
		}
	}
	return nil
}

type HistoryDayCount struct {
	Day  string `json:"day"`
	Rows int    `json:"rows"`
}

// HistoryDayCounts exposes only dates and row counts, never transaction contents.
func HistoryDayCounts(transactions []Transaction) []HistoryDayCount {
	return historyDayCounts(transactions, func(tx Transaction) string { return tx.TransactionAt })
}

func HistoryEffectiveDayCounts(transactions []Transaction) []HistoryDayCount {
	return historyDayCounts(transactions, func(tx Transaction) string { return tx.EffectiveDate })
}

func historyDayCounts(transactions []Transaction, date func(Transaction) string) []HistoryDayCount {
	counts := make(map[string]int)
	for _, transaction := range transactions {
		day, err := NormalizeDate(date(transaction), nil)
		if err != nil {
			counts["INVALID"]++
			continue
		}
		counts[day.TransactionDay]++
	}
	result := make([]HistoryDayCount, 0, len(counts))
	for day, rows := range counts {
		result = append(result, HistoryDayCount{Day: day, Rows: rows})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Day < result[j].Day })
	return result
}

// HistoryFormDateMatches checks only whether the returned form mirrors the requested range.
func HistoryFormDateMatches(markup, requested string) bool {
	form, err := ExtractHistoryForm(markup)
	return err == nil && form.Fields["FromDate"] == requested && form.Fields["ToDate"] == requested
}
