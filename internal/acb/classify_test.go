package acb

import "testing"

func TestClassifyPage(t *testing.T) {
	cases := []struct {
		name, url, body string
		want            PageKind
	}{
		{"login HTTP 200", "https://online.acb.com.vn/acbib/Request", `<input name="username"><input type="password" name="password">`, LoginPage},
		{"login with captcha", "https://online.acb.com.vn/acbib/Request", `<input name="username"><input type="password" name="password"><input name="captcha" placeholder="Mã xác nhận">`, LoginPage},
		{"login redirect errorPage", "https://online.acb.com.vn/acbib/Request?dse_errorPage=login.jsp", `some text`, LoginPage},
		{"login redirect displayPageNotLoginOp", "https://online.acb.com.vn/acbib/Request?dse_operationName=displayPageNotLoginOp", `some text`, LoginPage},
		{"login webmbtt redirect", "https://online.acb.com.vn/acbib/webmbtt", `some text`, LoginPage},
		{"login meta refresh to webmbtt", "https://online.acb.com.vn/acbib/", `<meta HTTP-EQUIV="REFRESH" content="0; url=https://online.acb.com.vn/acbib/webmbtt">`, LoginPage},
		{"login session timeout message", "https://online.acb.com.vn/acbib/Request", `Phiên làm việc đã hết hạn. Vui lòng đăng nhập lại.`, LoginPage},
		{"otp", "https://online.acb.com.vn/acbib/Request", `Nhập mã OTP SafeKey`, OTPChallenge},
		{"captcha", "https://online.acb.com.vn/acbib/Request", `captcha Mã xác nhận`, CaptchaPage},
		{"maintenance", "https://online.acb.com.vn/acbib/Request", `Hệ thống đang bảo trì`, MaintenancePage},
		{"account", "https://online.acb.com.vn/acbib/Request", `ibkacctDetailProc dse_processorState AccountNbr`, AccountDetailPage},
		{"account query form", "https://online.acb.com.vn/acbib/Request", `ibkacctDetailProc dse_processorState AccountNbr FromDate ToDate Ngày giao dịch`, AccountDetailPage},
		{"account with safekey menu and motphan", "https://online.acb.com.vn/acbib/Request", `<li><a>Đăng ký ACB SafeKey</a></li> <a href="ibktraNoTruocHanMotPhanProc"> ibkacctDetailProc dse_processorState AccountNbr`, AccountDetailPage},
		{"account with logout script", "https://online.acb.com.vn/acbib/Request", `<script>function logout(){submit('displayPageNotLoginOp')}</script> ibkacctDetailProc dse_processorState AccountNbr`, AccountDetailPage},
		{"history", "https://online.acb.com.vn/acbib/Request", `ibkacctDetailProc dse_processorState AccountNbr Số GD Ghi nợ Ghi có`, HistoryPage},
		{"history with safekey menu", "https://online.acb.com.vn/acbib/Request", `<li><a>Đăng ký ACB SafeKey</a></li> <a href="ibktraNoTruocHanMotPhanProc"> ibkacctDetailProc dse_processorState AccountNbr Số GD Ghi nợ Ghi có`, HistoryPage},
		{"summary page", "https://online.acb.com.vn/acbib/Request", `ibkacctSumProc dse_processorState AccountNumber <li><a>Đăng ký ACB SafeKey</a></li>`, AccountDetailPage},
		{"untrusted host", "https://example.test/", `ibkacctDetailProc AccountNbr`, UnknownPage},
		{"history with dse_errorPage=login.jsp in query", "https://online.acb.com.vn/acbib/Request?dse_operationName=ibkacctDetailOp&dse_errorPage=login.jsp", `ibkacctDetailProc dse_processorState AccountNbr Số GD Ghi nợ Ghi có`, HistoryPage},
		{"account with dse_errorPage=login.jsp in query", "https://online.acb.com.vn/acbib/Request?dse_operationName=ibkacctDetailOp&dse_errorPage=login.jsp", `ibkacctDetailProc dse_processorState AccountNbr`, AccountDetailPage},
		{"summary with dse_errorPage=login.jsp in query", "https://online.acb.com.vn/acbib/Request?dse_operationName=ibkacctSumOp&dse_errorPage=login.jsp", `ibkacctSumProc dse_processorState AccountNumber`, AccountDetailPage},
		{"actual login path /acbib/login.jsp", "https://online.acb.com.vn/acbib/login.jsp", `some markup`, LoginPage},
		{"actual login path /login", "https://online.acb.com.vn/login", `some markup`, LoginPage},
		{"actual login op obkLoginOp", "https://online.acb.com.vn/acbib/Request?op=obkLoginOp", `some markup`, LoginPage},
		{"actual login op displayPageNotLoginOp", "https://online.acb.com.vn/acbib/Request?dse_operationName=displayPageNotLoginOp", `some markup`, LoginPage},
		{"actual login form username and password inputs", "https://online.acb.com.vn/acbib/Request", `<form><input name="username"><input type="password" name="password"></form>`, LoginPage},
		{"actual login expired message phien lam viec da het han", "https://online.acb.com.vn/acbib/Request", `Phiên làm việc đã hết hạn. Vui lòng đăng nhập lại.`, LoginPage},
		{"actual login expired message vui long dang nhap lai", "https://online.acb.com.vn/acbib/Request", `Vui lòng đăng nhập lại để tiếp tục.`, LoginPage},
		{"maintenance safe with dse_errorPage in url", "https://online.acb.com.vn/acbib/Request?dse_errorPage=login.jsp", `Hệ thống đang bảo trì`, MaintenancePage},
		{"otp safe with dse_errorPage in url", "https://online.acb.com.vn/acbib/Request?dse_errorPage=login.jsp", `Nhập mã OTP SafeKey`, OTPChallenge},
		{"captcha safe with dse_errorPage in url", "https://online.acb.com.vn/acbib/Request?dse_errorPage=login.jsp", `Mã xác nhận captcha`, CaptchaPage},
		{"embedded script with credentials is not login", "https://online.acb.com.vn/acbib/Request", `<script>var username = 'test'; var password = 'secret';</script><div>neutral page</div>`, UnknownPage},
		{"embedded script with otp text is not challenge", "https://online.acb.com.vn/acbib/Request", `<script>var s = "nhập mã otp";</script><div>neutral page</div>`, UnknownPage},
		{"embedded script with captcha text is not challenge", "https://online.acb.com.vn/acbib/Request", `<script>var c = "mã xác nhận";</script><div>neutral page</div>`, UnknownPage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyPage(tc.url, tc.body); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
