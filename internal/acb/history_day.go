package acb

import "errors"

var ErrHistoryDayMismatch = errors.New("ACB history response contains transactions outside requested day")

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
