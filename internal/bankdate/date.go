package bankdate

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultLocation is Asia/Ho_Chi_Minh (+07:00).
var DefaultLocation = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err == nil {
		return loc
	}
	return time.FixedZone("Asia/Ho_Chi_Minh", 7*3600)
}()

type CanonicalDate struct {
	TransactionDay   string    `json:"transactionDay"`
	TransactionAtISO string    `json:"transactionDate"`
	DatePrecision    string    `json:"datePrecision"`
	Time             time.Time `json:"-"`
	HasTime          bool      `json:"-"`
}

var ErrInvalidDate = errors.New("invalid transaction date format")

// ParseTransactionDate parses bank date formats (DD/MM/YYYY [HH:MM[:SS]])
// and defensive ISO variants into time.Time and whether time components were present.
func ParseTransactionDate(raw string, loc *time.Location) (time.Time, bool, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, false, ErrInvalidDate
	}
	if loc == nil {
		loc = DefaultLocation
	}

	layoutsWithTime := []string{
		"02/01/2006 15:04:05",
		"02/01/2006 15:04",
		"02/01/2006T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		time.RFC3339,
		time.RFC3339Nano,
	}

	for _, layout := range layoutsWithTime {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.In(loc), true, nil
		}
	}

	layoutsDateOnly := []string{
		"02/01/2006",
		"2006-01-02",
	}

	for _, layout := range layoutsDateOnly {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.In(loc), false, nil
		}
	}

	return time.Time{}, false, fmt.Errorf("%w: %q", ErrInvalidDate, raw)
}

// NormalizeDate converts any valid raw date into canonical day, ISO string and precision.
func NormalizeDate(raw string, loc *time.Location) (CanonicalDate, error) {
	t, hasTime, err := ParseTransactionDate(raw, loc)
	if err != nil {
		return CanonicalDate{}, err
	}

	day := t.Format("2006-01-02")
	if hasTime {
		return CanonicalDate{
			TransactionDay:   day,
			TransactionAtISO: t.Format(time.RFC3339),
			DatePrecision:    "datetime",
			Time:             t,
			HasTime:          true,
		}, nil
	}

	return CanonicalDate{
		TransactionDay:   day,
		TransactionAtISO: day,
		DatePrecision:    "date",
		Time:             t,
		HasTime:          false,
	}, nil
}
