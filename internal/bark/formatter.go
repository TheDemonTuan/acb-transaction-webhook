package bark

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

var accountNumRegex = regexp.MustCompile(`\b\d{8,20}\b`)

func FormatVND(amountStr string) string {
	n, err := strconv.ParseInt(strings.TrimSpace(amountStr), 10, 64)
	if err != nil {
		return amountStr + "đ"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	l := len(s)
	for i, r := range s {
		if i > 0 && (l-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(r)
	}
	res := b.String() + "đ"
	if neg {
		res = "-" + res
	}
	return res
}

func maskSensitiveNumbers(desc string) string {
	return accountNumRegex.ReplaceAllStringFunc(desc, func(s string) string {
		if len(s) <= 6 {
			return s
		}
		return s[:3] + "..." + s[len(s)-3:]
	})
}

func FormatTransactionNotification(eventData map[string]any, cfg storage.BarkConfig, publicOrigin string) (title, subtitle, body, group, sound, level, icon, linkURL string) {
	creditStr, _ := eventData["credit"].(string)
	if creditStr == "" {
		creditStr = "0"
	}
	title = "+" + FormatVND(creditStr)
	if eventData["source"] == "CATCH_UP" {
		title += " · Giao dịch cũ"
	}
	if cfg.IncludeBalance {
		subtitle = "Số dư: chưa có dữ liệu"
		balance, _ := eventData["balance"].(string)
		if _, err := strconv.ParseInt(strings.TrimSpace(balance), 10, 64); err == nil {
			subtitle = "Số dư: " + FormatVND(balance)
		}
	}
	var bodyLines []string
	if cfg.IncludeDescription {
		desc, _ := eventData["description"].(string)
		desc = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, desc)
		desc = maskSensitiveNumbers(strings.Join(strings.Fields(desc), " "))
		if runes := []rune(desc); len(runes) > 300 {
			desc = string(runes[:300]) + "..."
		}
		if desc == "" {
			desc = "chưa có nội dung"
		}
		bodyLines = append(bodyLines, "ND: "+desc)
	}
	provider, _ := eventData["provider"].(string)
	switch strings.ToUpper(provider) {
	case "SEPAY":
		provider = "SePay"
	case "PAYOS":
		provider = "payOS"
	}
	bank, _ := eventData["bank"].(string)
	var context []string
	for _, part := range []string{provider, bank} {
		if part = strings.TrimSpace(part); part != "" {
			context = append(context, part)
		}
	}
	txDateRaw, _ := eventData["transactionDate"].(string)
	if t, err := time.Parse(time.RFC3339, txDateRaw); err == nil {
		loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
		if err == nil {
			t = t.In(loc)
		}
		context = append(context, t.Format("02/01 15:04"))
	}
	if len(context) > 0 {
		bodyLines = append(bodyLines, strings.Join(context, " · "))
	}

	body = strings.Join(bodyLines, "\n")
	if len([]rune(body)) > 1800 {
		body = string([]rune(body)[:1800])
	}
	if len([]rune(title)) > 120 {
		title = string([]rune(title)[:120])
	}

	group = cfg.Group
	if group == "" {
		group = "Giao dịch"
	}

	sound = cfg.Sound
	if sound == "" {
		sound = "shake"
	}

	level = cfg.Level
	if level == "" {
		level = "timeSensitive"
	}

	icon = cfg.Icon

	if cfg.DashboardLink && publicOrigin != "" {
		origin, err := url.Parse(publicOrigin)
		if err == nil && (origin.Scheme == "https" || origin.Scheme == "http") && origin.Hostname() != "" && origin.User == nil {
			base := origin.Scheme + "://" + origin.Host
			list, detail := "/transactions", "/transactions/"
			if strings.EqualFold(origin.Hostname(), "transactions.tuannguyenviet.site") {
				list, detail = "/", "/t/"
			}
			txnID, _ := eventData["transactionId"].(string)
			if txnID != "" {
				linkURL = base + detail + url.PathEscape(txnID)
			} else {
				linkURL = base + list
			}
		}
	}

	return title, subtitle, body, group, sound, level, icon, linkURL
}

func FormatTestNotification(cfg storage.BarkConfig) (title, subtitle, body, group, sound, level, icon string) {
	title, subtitle, body, group, sound, level, icon, _ = FormatTransactionNotification(map[string]any{
		"credit": "150000", "balance": "2350000", "description": "THỬ BARK - kiểm tra hiển thị", "provider": "SEPAY", "bank": "VCB",
	}, cfg, "")
	title += " · THỬ"
	body += "\nDữ liệu mẫu, không phải giao dịch thật."
	return
}
