package acb

import (
	"html"
	"net/url"
	"strings"
)

type PageKind string

const (
	AccountDetailPage PageKind = "ACCOUNT_DETAIL_PAGE"
	HistoryPage       PageKind = "HISTORY_PAGE"
	LoginPage         PageKind = "LOGIN_PAGE"
	OTPChallenge      PageKind = "OTP_CHALLENGE"
	CaptchaPage       PageKind = "CAPTCHA_PAGE"
	MaintenancePage   PageKind = "MAINTENANCE_PAGE"
	UnknownPage       PageKind = "UNKNOWN_PAGE"
)

// ClassifyPage uses independent page signals. It must not turn unknown markup
// into an empty transaction history.
func ClassifyPage(finalURL, body string) PageKind {
	location, _ := url.Parse(finalURL)
	host := strings.ToLower(location.Hostname())
	if host != "" && host != "online.acb.com.vn" {
		return UnknownPage
	}

	page := strings.ToLower(html.UnescapeString(body))
	cleanPage := stripScripts(page)

	if containsAny(cleanPage, "bảo trì", "bao tri", "maintenance") {
		return MaintenancePage
	}

	if strings.Contains(page, "ibkacctdetailproc") && containsAny(page, "accountnbr", "dse_processorstate") {
		if containsAny(page, "sogd", "sogiaodich", "so gd", "số gd") || (containsAny(page, "ghino", "ghi no", "ghi nợ") && containsAny(page, "ghico", "ghi co", "ghi có")) {
			return HistoryPage
		}
		return AccountDetailPage
	}
	if strings.Contains(page, "ibkacctsumproc") && containsAny(page, "accountnbr", "accountnumber", "dse_processorstate") {
		return AccountDetailPage
	}

	loginSignals := 0
	for _, signal := range []string{"username", "tên truy cập", "ten truy cap", "password", "mật khẩu", "mat khau", "obkloginop"} {
		if strings.Contains(cleanPage, signal) {
			loginSignals++
		}
	}
	metaRefresh := strings.Contains(page, "http-equiv=\"refresh\"") || strings.Contains(page, "http-equiv='refresh'") || strings.Contains(page, "http-equiv = \"refresh\"")
	if loginSignals >= 2 || (containsAny(page, "displaypagenotloginop", "obkloginop", "webmbtt") && metaRefresh) {
		return LoginPage
	}

	if containsAny(cleanPage, "nhập mã otp", "nhap ma otp", "nhập mã safekey", "nhap ma safekey", "mã xác thực otp", "ma xac thuc otp", "xác thực otp", "xac thuc otp", "xác nhận otp", "xac nhan otp", "mã otp", "ma otp", `name="otp"`, `id="otp"`, `name="safekey"`, `id="safekey"`, `name="authcode"`, `id="authcode"`) {
		return OTPChallenge
	}
	if containsAny(cleanPage, "mã xác nhận", "ma xac nhan", "mã kiểm tra", "ma kiem tra", `name="captcha"`, `id="captcha"`) {
		return CaptchaPage
	}

	lowerURL := strings.ToLower(finalURL)
	lowerPath := strings.ToLower(location.Path)
	if lowerPath == "/login" || containsAny(lowerURL, "login.jsp", "displaypagenotloginop", "obkloginop", "/acbib/webmbtt") ||
		containsAny(cleanPage, "phiên làm việc đã hết hạn", "phien lam viec da het han", "vui lòng đăng nhập lại", "vui long dang nhap lai") {
		return LoginPage
	}

	return UnknownPage
}

func stripScripts(s string) string {
	for {
		start := strings.Index(s, "<script")
		if start == -1 {
			break
		}
		end := strings.Index(s[start:], "</script>")
		if end == -1 {
			return s[:start]
		}
		s = s[:start] + s[start+end+len("</script>"):]
	}
	return s
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
