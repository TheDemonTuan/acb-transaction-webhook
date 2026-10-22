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

func TestNotificationLayoutPrivacyAndProvider(t *testing.T) {
	cfg := storage.BarkConfig{IncludeBalance: true, IncludeDescription: true, DashboardLink: true, Group: "custom", Icon: "https://example.com/icon.png"}
	data := map[string]any{"credit": "150000", "balance": "2350000", "description": "  memo\n\t012345678901\x00end", "provider": "PAYOS", "bank": "VCB", "transactionDate": "2026-10-22T07:35:00Z"}
	title, sub, body, group, _, _, icon, _ := FormatTransactionNotification(data, cfg, "")
	if !strings.HasPrefix(title, "+150.000đ") || sub != "Số dư: 2.350.000đ" || !strings.HasPrefix(body, "ND: memo 012...901 end\n") || !strings.HasSuffix(body, "payOS · VCB · 22/10 14:35") {
		t.Fatalf("layout: %q %q %q", title, sub, body)
	}
	if strings.Contains(body, "012345678901") || strings.Contains(body, "2.350.000") || strings.Contains(title, "ACB") || group != cfg.Group || icon != cfg.Icon {
		t.Fatal("privacy or channel override lost")
	}
	cfg.IncludeBalance, cfg.IncludeDescription = false, false
	_, sub, body, _, _, _, _, _ = FormatTransactionNotification(data, cfg, "")
	if sub != "" || strings.Contains(body, "memo") || strings.Contains(body, "2350000") {
		t.Fatal("disabled fields leaked")
	}
}

func TestTransactionLinksRespectOrigin(t *testing.T) {
	cfg := storage.DefaultBarkConfig()
	cfg.DashboardLink = true
	tests := []struct{ origin, id, want string }{
		{"https://transactions.tuannguyenviet.site", "txn_123", "https://transactions.tuannguyenviet.site/t/txn_123"},
		{"https://transactions.tuannguyenviet.site", "", "https://transactions.tuannguyenviet.site/"},
		{"https://admin.example.com", "txn_123", "https://admin.example.com/transactions/txn_123"},
		{"https://admin.example.com", "", "https://admin.example.com/transactions"},
		{"https://admin.example.com", "a/b", "https://admin.example.com/transactions/a%2Fb"},
		{"", "txn_123", ""},
		{"not a URL", "txn_123", ""},
		{"https://", "txn_123", ""},
	}
	for _, tc := range tests {
		_, _, _, _, _, _, _, got := FormatTransactionNotification(map[string]any{"transactionId": tc.id}, cfg, tc.origin)
		if got != tc.want {
			t.Errorf("origin %q id %q: link = %q, want %q", tc.origin, tc.id, got, tc.want)
		}
	}
	cfg.DashboardLink = false
	_, _, _, _, _, _, _, got := FormatTransactionNotification(map[string]any{"transactionId": "txn_123"}, cfg, "https://transactions.tuannguyenviet.site")
	if got != "" {
		t.Errorf("disabled dashboard link = %q", got)
	}
}

func TestNotificationMissingBalanceAndCatchUp(t *testing.T) {
	cfg := storage.BarkConfig{IncludeBalance: true, IncludeDescription: true}
	data := map[string]any{"credit": "100000", "source": "CATCH_UP", "transactionDate": "invalid-date"}
	title, sub, body, _, _, _, icon, _ := FormatTransactionNotification(data, cfg, "")
	if !strings.HasPrefix(title, "+100.000đ") || !strings.Contains(title, "Giao dịch cũ") || sub != "Số dư: chưa có dữ liệu" || body != "ND: chưa có nội dung" || icon != "" {
		t.Fatalf("missing snapshot: %q %q %q", title, sub, body)
	}
	data["balance"] = "0"
	_, sub, _, _, _, _, _, _ = FormatTransactionNotification(data, cfg, "")
	if sub != "Số dư: 0đ" {
		t.Fatal("zero confused with missing balance")
	}
	data["description"] = strings.Repeat("a", 301)
	_, _, body, _, _, _, _, _ = FormatTransactionNotification(data, cfg, "")
	if body != "ND: "+strings.Repeat("a", 300)+"..." {
		t.Fatal("memo display limit lost")
	}
}

func TestSampleNotificationRespectsPrivacy(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		cfg := storage.BarkConfig{IncludeBalance: enabled, IncludeDescription: enabled}
		title, sub, body, _, _, _, _ := FormatTestNotification(cfg)
		if !strings.HasPrefix(title, "+150.000đ") || !strings.HasSuffix(title, " · THỬ") || !strings.Contains(body, "không phải giao dịch thật") {
			t.Fatal("sample could be confused with real transaction")
		}
		if strings.Contains(body, "THỬ BARK") != enabled || (sub != "") != enabled {
			t.Fatal("sample privacy flags ignored")
		}
	}
}
