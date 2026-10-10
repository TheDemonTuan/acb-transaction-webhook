package sepay

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const fixtureNotification = "SEPAY_STORE_V1\ndirection=vào\namount=50,000\naccount=VA012345\nbank=Fixture Bank\ntime=10/10/2026 10:30:00\nreference=SEPAY_TEST_001\ncontent=PRIVATE PAYER MEMO"

func TestParseNotificationExactContract(t *testing.T) {
	for _, amount := range []string{"50,000", "50000", "50.000"} {
		notification, err := ParseNotification(strings.Replace(fixtureNotification, "50,000", amount, 1))
		if err != nil {
			t.Fatal(err)
		}
		if notification.AmountVND != 50000 || notification.AccountNumber != "VA012345" || notification.BankName != "Fixture Bank" || notification.Reference != "SEPAY_TEST_001" {
			t.Fatalf("unexpected parsed notification: %+v", notification)
		}
		if !notification.TransactionAt.Equal(time.Date(2026, 10, 10, 3, 30, 0, 0, time.UTC)) {
			t.Fatal("bank date did not use Vietnam timezone")
		}
	}
	content := "PRIVATE PAYER MEMO\namount=120000\nreference=NOT_A_HEADER\n<b>not HTML</b>&amp;"
	notification, err := ParseNotification(strings.Replace(fixtureNotification, "PRIVATE PAYER MEMO", content, 1))
	if err != nil || notification.Description != content || notification.AmountVND != 50000 || notification.Reference != "SEPAY_TEST_001" {
		t.Fatal("content suffix was interpreted or changed")
	}
}

func TestParseNotificationRejectsInvalidAmounts(t *testing.T) {
	for _, amount := range []string{"", "0", "000", "50.00", "-50000", "+50000", "5e4", "NaN", "50,000.000", "1.000,000", "1234,567", ",500", "500,", "1,,000", "50 000", "50k", "50000đ", "５００００", "9007199254740992", "9223372036854775808"} {
		t.Run(amount, func(t *testing.T) {
			_, err := ParseNotification(strings.Replace(fixtureNotification, "50,000", amount, 1))
			if !errors.Is(err, ErrInvalidAmount) {
				t.Fatalf("expected invalid amount, got %v", err)
			}
		})
	}
	for _, amount := range []string{"1", "1,000,000", "1.000.000", "9007199254740991", "9,007,199,254,740,991"} {
		if _, err := ParseNotification(strings.Replace(fixtureNotification, "50,000", amount, 1)); err != nil {
			t.Fatalf("valid integer %s rejected: %v", amount, err)
		}
	}
}

func TestParseNotificationReviewReasons(t *testing.T) {
	cases := []struct {
		name string
		text string
		want error
	}{
		{"outgoing", strings.Replace(fixtureNotification, "direction=vào", "direction=ra", 1), ErrOutgoing},
		{"unknown direction", strings.Replace(fixtureNotification, "direction=vào", "direction=credit", 1), ErrInvalidTemplate},
		{"date only", strings.Replace(fixtureNotification, "10/10/2026 10:30:00", "10/10/2026", 1), ErrInvalidTransactionDate},
		{"missing date", strings.Replace(fixtureNotification, "10/10/2026 10:30:00", "", 1), ErrInvalidTransactionDate},
		{"invalid date", strings.Replace(fixtureNotification, "10/10/2026 10:30:00", "31/02/2026 10:30:00", 1), ErrInvalidTransactionDate},
		{"missing reference", strings.Replace(fixtureNotification, "SEPAY_TEST_001", "   ", 1), ErrMissingReference},
		{"long reference", strings.Replace(fixtureNotification, "SEPAY_TEST_001", strings.Repeat("a", 201), 1), ErrMissingReference},
		{"missing header", strings.Replace(fixtureNotification, "bank=Fixture Bank\n", "", 1), ErrInvalidTemplate},
		{"duplicate header", strings.Replace(fixtureNotification, "account=VA012345", "amount=50000", 1), ErrInvalidTemplate},
		{"extra header", strings.Replace(fixtureNotification, "content=", "amount=50000\ncontent=", 1), ErrInvalidTemplate},
		{"reordered header", strings.Replace(fixtureNotification, "account=VA012345\nbank=Fixture Bank", "bank=Fixture Bank\naccount=VA012345", 1), ErrInvalidTemplate},
		{"unrendered variable", strings.Replace(fixtureNotification, "SEPAY_TEST_001", "{{reference_number}}", 1), ErrInvalidTemplate},
		{"missing marker", strings.TrimPrefix(fixtureNotification, "SEPAY_STORE_V1\n"), ErrInvalidTemplate},
		{"leading text", "notification\n" + fixtureNotification, ErrInvalidTemplate},
		{"too long", fixtureNotification + strings.Repeat("a", 4096), ErrInvalidTemplate},
		{"invalid UTF8", fixtureNotification + string([]byte{0xff}), ErrInvalidTemplate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseNotification(tc.text)
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("error disclosed raw notification")
			}
		})
	}
}

func TestParseNotificationTrimsHeadersOnly(t *testing.T) {
	text := strings.Replace(fixtureNotification, "account=VA012345", "account=  VA012345  ", 1)
	text = strings.Replace(text, "bank=Fixture Bank", "bank=  Fixture Bank  ", 1)
	text = strings.Replace(text, "SEPAY_TEST_001", strings.Repeat("Đ", 200), 1)
	text = strings.Replace(text, "PRIVATE PAYER MEMO", "  unchanged  \n", 1)
	notification, err := ParseNotification(text)
	if err != nil || notification.AccountNumber != "VA012345" || notification.BankName != "Fixture Bank" || notification.Description != "  unchanged  \n" {
		t.Fatal("header trimming or character length contract changed")
	}
	crlf := strings.ReplaceAll(fixtureNotification, "\n", "\r\n")
	if _, err := ParseNotification(crlf); err != nil {
		t.Fatal("CRLF template rejected")
	}
}
