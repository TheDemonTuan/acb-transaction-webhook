package sepay

import (
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thedemontuan/acb-transaction-webhook/internal/bankdate"
)

const (
	ReasonInvalidTemplate              = "INVALID_TEMPLATE"
	ReasonOutgoing                     = "OUTGOING"
	ReasonAccountMismatch              = "ACCOUNT_MISMATCH"
	ReasonInvalidAmount                = "INVALID_AMOUNT"
	ReasonInvalidTransactionDate       = "INVALID_TRANSACTION_DATE"
	ReasonMissingReference             = "MISSING_REFERENCE"
	ReasonEditedMessage                = "EDITED_MESSAGE"
	maxAmountVND                 int64 = 9007199254740991
	maxTelegramTextLength              = 4096
)

var (
	ErrInvalidTemplate        = errors.New(ReasonInvalidTemplate)
	ErrOutgoing               = errors.New(ReasonOutgoing)
	ErrInvalidAmount          = errors.New(ReasonInvalidAmount)
	ErrInvalidTransactionDate = errors.New(ReasonInvalidTransactionDate)
	ErrMissingReference       = errors.New(ReasonMissingReference)
)

type Notification struct {
	AmountVND     int64
	AccountNumber string
	BankName      string
	Reference     string
	Description   string
	TransactionAt time.Time
}

// ParseNotification accepts only the fixed v1 headers. Everything following
// content= is opaque memo text, including newlines that resemble headers.
// Errors contain only review codes, never raw text or payer information.
func ParseNotification(text string) (Notification, error) {
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxTelegramTextLength {
		return Notification{}, ErrInvalidTemplate
	}
	lines := strings.SplitN(text, "\n", 8)
	if len(lines) != 8 || strings.TrimSuffix(lines[0], "\r") != "SEPAY_STORE_V1" {
		return Notification{}, ErrInvalidTemplate
	}
	prefixes := [...]string{"direction=", "amount=", "account=", "bank=", "time=", "reference="}
	var values [6]string
	for i, prefix := range prefixes {
		line := strings.TrimSuffix(lines[i+1], "\r")
		if !strings.HasPrefix(line, prefix) {
			return Notification{}, ErrInvalidTemplate
		}
		values[i] = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if strings.ContainsAny(values[i], "\r\x00") || strings.Contains(values[i], "{{") || strings.Contains(values[i], "}}") {
			return Notification{}, ErrInvalidTemplate
		}
	}
	if !strings.HasPrefix(lines[7], "content=") {
		return Notification{}, ErrInvalidTemplate
	}
	if values[0] == "ra" {
		return Notification{}, ErrOutgoing
	}
	if values[0] != "vào" || values[2] == "" || values[3] == "" {
		return Notification{}, ErrInvalidTemplate
	}
	amount, err := parseAmountVND(values[1])
	if err != nil {
		return Notification{}, err
	}
	transactionAt, hasTime, err := bankdate.ParseTransactionDate(values[4], bankdate.DefaultLocation)
	if err != nil || !hasTime {
		return Notification{}, ErrInvalidTransactionDate
	}
	if values[5] == "" || utf8.RuneCountInString(values[5]) > 200 {
		return Notification{}, ErrMissingReference
	}
	return Notification{
		AmountVND: amount, AccountNumber: values[2], BankName: values[3],
		Reference: values[5], TransactionAt: transactionAt,
		Description: strings.TrimPrefix(lines[7], "content="),
	}, nil
}

func parseAmountVND(raw string) (int64, error) {
	if raw == "" {
		return 0, ErrInvalidAmount
	}
	separator := byte(0)
	groupLength, groups := 0, 0
	for i := range len(raw) {
		ch := raw[i]
		if ch >= '0' && ch <= '9' {
			groupLength++
			continue
		}
		if ch != ',' && ch != '.' || separator != 0 && separator != ch {
			return 0, ErrInvalidAmount
		}
		if groups == 0 {
			if groupLength < 1 || groupLength > 3 {
				return 0, ErrInvalidAmount
			}
		} else if groupLength != 3 {
			return 0, ErrInvalidAmount
		}
		separator, groupLength, groups = ch, 0, groups+1
	}
	if groups > 0 && groupLength != 3 {
		return 0, ErrInvalidAmount
	}
	if separator != 0 {
		raw = strings.ReplaceAll(raw, string(separator), "")
	}
	amount, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || amount <= 0 || amount > maxAmountVND {
		return 0, ErrInvalidAmount
	}
	return amount, nil
}

func notificationReviewReason(err error) string {
	switch {
	case errors.Is(err, ErrOutgoing):
		return ReasonOutgoing
	case errors.Is(err, ErrInvalidAmount):
		return ReasonInvalidAmount
	case errors.Is(err, ErrInvalidTransactionDate):
		return ReasonInvalidTransactionDate
	case errors.Is(err, ErrMissingReference):
		return ReasonMissingReference
	default:
		return ReasonInvalidTemplate
	}
}
