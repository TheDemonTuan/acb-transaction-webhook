package bark

import (
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestFormatVND(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"0", "0đ"},
		{"50000", "50.000đ"},
		{"1250000", "1.250.000đ"},
		{"100000000", "100.000.000đ"},
		{"-5000", "-5.000đ"},
	}

	for _, tc := range tests {
		got := FormatVND(tc.input)
		if got != tc.expected {
			t.Errorf("FormatVND(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestFormatTransactionNotificationRealtime(t *testing.T) {
	eventData := map[string]any{
		"transactionId":     "txn_123",
		"transactionNumber": "987654",
		"credit":            "2500000",
		"debit":             "0",
		"source":            "REALTIME",
		"description":       "CT tu 012345678901 Nguyen Van A",
		"balance":           "15000000",
		"transactionDate":   "2026-09-13T15:30:00Z",
	}

	cfg := storage.DefaultBarkConfig()
	cfg.IncludeBalance = true
	cfg.IncludeDescription = true
	cfg.DashboardLink = true

	title, subtitle, body, group, sound, level, icon, linkURL := FormatTransactionNotification(eventData, cfg, "https://bank.example.com")

	if title != "💰 ACB +2.500.000đ" {
		t.Fatalf("unexpected title: %s", title)
	}
	if !strings.Contains(subtitle, "Biến động số dư") {
		t.Fatalf("unexpected subtitle: %s", subtitle)
	}
	if !strings.Contains(body, "Mô tả:") {
		t.Fatalf("expected description in body, got: %s", body)
	}
	// Verify account number is masked
	if strings.Contains(body, "012345678901") {
		t.Fatalf("account number was not masked in body: %s", body)
	}
	if !strings.Contains(body, "Số dư: 15.000.000đ") {
		t.Fatalf("expected balance in body, got: %s", body)
	}
	if group != "ACB" || sound != "shake" || level != "timeSensitive" {
		t.Fatalf("unexpected group/sound/level: %s %s %s", group, sound, level)
	}
	if icon != "https://api.vietqr.io/img/ACB.png" {
		t.Fatalf("unexpected icon: %s", icon)
	}
	if linkURL != "https://bank.example.com/transactions/txn_123" {
		t.Fatalf("unexpected linkURL: %s", linkURL)
	}
}

func TestFormatTransactionNotificationCatchUp(t *testing.T) {
	eventData := map[string]any{
		"transactionId":     "txn_cu_1",
		"transactionNumber": "112233",
		"credit":            "100000",
		"source":            "CATCH_UP",
		"description":       "Tien thuong",
		"transactionDate":   "2026-09-10T08:00:00Z",
	}

	cfg := storage.DefaultBarkConfig()
	cfg.IncludeBalance = false
	cfg.IncludeDescription = true

	title, _, body, _, _, _, _, _ := FormatTransactionNotification(eventData, cfg, "")

	if !strings.Contains(title, "(bù dữ liệu)") {
		t.Fatalf("expected catch-up marker in title, got: %s", title)
	}
	if !strings.Contains(body, "đồng bộ bù") {
		t.Fatalf("expected catch-up explanation in body, got: %s", body)
	}
}
