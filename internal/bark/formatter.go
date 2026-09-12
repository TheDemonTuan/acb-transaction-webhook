package bark

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	formattedAmount := FormatVND(creditStr)

	source, _ := eventData["source"].(string)
	isCatchUp := source == "CATCH_UP"

	if isCatchUp {
		title = fmt.Sprintf("🕓 ACB +%s (bù dữ liệu)", formattedAmount)
	} else {
		title = fmt.Sprintf("💰 ACB +%s", formattedAmount)
	}

	accMasked, _ := eventData["account"].(string)
	if accMasked == "" {
		accMasked, _ = eventData["accountMasked"].(string)
	}
	if accMasked != "" {
		subtitle = fmt.Sprintf("Tài khoản %s • Biến động số dư", accMasked)
	} else {
		subtitle = "Biến động số dư tài khoản"
	}

	var bodyLines []string

	if cfg.IncludeDescription {
		desc, _ := eventData["description"].(string)
		desc = strings.TrimSpace(desc)
		if desc != "" {
			masked := maskSensitiveNumbers(desc)
			if len([]rune(masked)) > 300 {
				masked = string([]rune(masked)[:300]) + "..."
			}
			bodyLines = append(bodyLines, fmt.Sprintf("Mô tả: %s", masked))
		}
	}

	if cfg.IncludeBalance {
		balStr, _ := eventData["balance"].(string)
		balStr = strings.TrimSpace(balStr)
		if balStr != "" {
			bodyLines = append(bodyLines, fmt.Sprintf("Số dư: %s", FormatVND(balStr)))
		}
	}

	// Transaction Date/Time
	txDateRaw, _ := eventData["transactionDate"].(string)
	if txDateRaw != "" {
		if t, err := time.Parse(time.RFC3339, txDateRaw); err == nil {
			loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
			if err == nil {
				t = t.In(loc)
			}
			bodyLines = append(bodyLines, fmt.Sprintf("Thời gian: %s", t.Format("02/01/2006 15:04")))
		} else {
			bodyLines = append(bodyLines, fmt.Sprintf("Thời gian: %s", txDateRaw))
		}
	}

	if isCatchUp {
		bodyLines = append(bodyLines, "Giao dịch được phát hiện trong chu kỳ đồng bộ bù.")
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
		group = "ACB"
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
	if icon == "" {
		icon = "https://api.vietqr.io/img/ACB.png"
	}

	if cfg.DashboardLink && publicOrigin != "" {
		txnID, _ := eventData["transactionId"].(string)
		if txnID != "" {
			linkURL = fmt.Sprintf("%s/transactions/%s", strings.TrimRight(publicOrigin, "/"), txnID)
		} else {
			linkURL = fmt.Sprintf("%s/transactions", strings.TrimRight(publicOrigin, "/"))
		}
	}

	return title, subtitle, body, group, sound, level, icon, linkURL
}

func FormatTestNotification(cfg storage.BarkConfig) (title, subtitle, body, group, sound, level, icon string) {
	title = "✅ Bark đã kết nối"
	subtitle = "ACB Transaction Gateway"
	body = "Hệ thống đã kết nối thành công và có thể gửi thông báo biến động số dư tới iPhone này."

	group = cfg.Group
	if group == "" {
		group = "ACB"
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
	if icon == "" {
		icon = "https://api.vietqr.io/img/ACB.png"
	}

	return title, subtitle, body, group, sound, level, icon
}
