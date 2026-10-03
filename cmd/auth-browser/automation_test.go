package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
)

func TestBrowserAutomationInitialCaptchaAndOTP(t *testing.T) {
	if os.Getenv("ACB_BROWSER_INTEGRATION") != "1" {
		t.Skip("opt in with ACB_BROWSER_INTEGRATION=1")
	}
	browser := findDefaultBrowser()
	if _, err := exec.LookPath(browser); err != nil {
		t.Fatal("opt-in browser unavailable: set BROWSER_BIN to an installed Chromium")
	}
	var mu sync.Mutex
	loginCount, otpCount := 0, 0
	credentialsOK, cookieOK, otpOK := false, false, false
	readFixture := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join("testdata", "recovery", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	initial, otp, auth := readFixture("initial-captcha.html"), readFixture("otp.html"), readFixture("authenticated.html")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == "GET" {
			http.SetCookie(w, &http.Cookie{Name: "fixture_session", Value: "synthetic-cookie", Path: "/", HttpOnly: true})
			_, _ = w.Write(initial)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		cookie, err := r.Cookie("fixture_session")
		cookieOK = err == nil && cookie.Value == "synthetic-cookie"
		switch r.URL.Query().Get("stage") {
		case "login":
			loginCount++
			credentialsOK = r.Form.Get("username") == "fixture-user" && r.Form.Get("password") == "fixture-password" && r.Form.Get("captcha") == "AB12CD"
			_, _ = w.Write(otp)
		case "otp":
			otpCount++
			otpOK = r.Form.Get("otp") == "001234"
			_, _ = w.Write(auth)
		default:
			w.WriteHeader(400)
		}
	}))
	defer upstream.Close()
	s := &server{profiles: t.TempDir(), browserExec: browser, extraFlags: []string{"--headless=new"}, loginURL: upstream.URL + "/acbib/Request", fixtureDOM: true, internalToken: "fixture-internal", internalAuthRequired: true}
	mux := http.NewServeMux()
	mux.Handle("POST /sessions", s.requireInternal(http.HandlerFunc(s.start)))
	mux.Handle("DELETE /sessions/{attemptID}", s.requireInternal(http.HandlerFunc(s.cancel)))
	mux.Handle("POST /sessions/{attemptID}/handoff", s.requireInternal(http.HandlerFunc(s.handoff)))
	s.registerAutomation(mux)
	api := httptest.NewServer(mux)
	defer api.Close()
	defer s.stopCurrent()
	client := authbrowser.NewClient(api.URL, "fixture-internal")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const attempt = "automation-fixture"
	if _, err := client.Start(ctx, attempt); err != nil {
		t.Fatalf("start actual server: %v", err)
	}
	observation, err := client.Observe(ctx, attempt)
	if err != nil || observation.State != authbrowser.LoginForm || !observation.CaptchaRequired {
		t.Fatalf("initial CAPTCHA state: %+v %v", observation, err)
	}
	again, err := client.Observe(ctx, attempt)
	if err != nil || again.Revision != observation.Revision {
		t.Fatal("ordinary Observe changed the revision")
	}
	crop, err := client.CaptureCaptcha(ctx, attempt, observation.Revision)
	if err != nil {
		t.Fatal(err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(crop))
	if err != nil || config.Width != 160 || config.Height != 50 {
		t.Fatal("CAPTCHA response was not an exact 160x50 node crop")
	}
	input := authbrowser.LoginInput{Revision: observation.Revision, Username: "fixture-user", Password: "fixture-password", AccountNumber: "222222222", Captcha: "AB12CD"}
	observation, err = client.SubmitLogin(ctx, attempt, input)
	if err != nil || observation.State != authbrowser.OTPRequired || observation.OTPLength != 0 {
		t.Fatalf("login did not reach OTP: %+v %v", observation, err)
	}
	if _, err = client.SubmitLogin(ctx, attempt, input); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("duplicate revision was accepted")
	}
	observation, err = client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: observation.Revision, Value: "001234"})
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != authbrowser.Authenticated && observation.ReasonCode != "ACCOUNT_SELECTION_PENDING" {
		t.Fatalf("exact account navigation was classified as a terminal unsupported page: %+v", observation)
	}
	// Exact multi-account selection may need one additional observation after onchange.
	for range 5 {
		if observation.State == authbrowser.Authenticated {
			break
		}
		observation, err = client.Observe(ctx, attempt)
		if err != nil {
			t.Fatal(err)
		}
	}
	if observation.State != authbrowser.Authenticated {
		t.Fatalf("not authenticated after exact account selection: %+v", observation)
	}
	handoff, err := client.Handoff(ctx, attempt)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := authbrowser.DecodeHandoff(handoff)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Fields["AccountNbr"] != "222222222" || decoded.Fields["dse_operationName"] != "ibkacctDetailProc" {
		t.Fatal("handoff selected the wrong account/history form")
	}
	mu.Lock()
	countsOK := loginCount == 1 && otpCount == 1 && credentialsOK && cookieOK && otpOK
	mu.Unlock()
	if !countsOK {
		t.Fatal("actual browser did not preserve one attempt/cookie, initial CAPTCHA and leading-zero OTP across single submits")
	}

	// Navigate the existing production browser through adversarial fixture DOMs.
	navigateHTML := func(html string) {
		s.opMu.Lock()
		defer s.opMu.Unlock()
		err := s.withAutomationTab(ctx, s.session, func(tab context.Context, _ target.ID) error {
			return chromedp.Run(tab, chromedp.Evaluate("document.open();document.write("+jsString(html)+");document.close();", nil))
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name, html string
		state      authbrowser.AuthPageState
	}{
		{"hidden password", strings.Replace(string(initial), "type=\"password\"", "type=\"password\" style=\"display:none\"", 1), authbrowser.Unknown},
		{"multiple usernames", strings.Replace(string(initial), "<input name=\"username\"", "<input name=\"user\"><input name=\"username\"", 1), authbrowser.Unknown},
		{"wrong form origin", strings.Replace(string(initial), "action=\"/acbib/Request?stage=login\"", "action=\"https://example.invalid/login\"", 1), authbrowser.Unknown},
		{"frame", strings.Replace(string(initial), "<form ", "<iframe src=\"https://example.invalid/\"></iframe><form ", 1), authbrowser.Unknown},
		{"unsupported", string(readFixture("unsupported.html")), authbrowser.UnsupportedChallenge},
		{"rejection fixture", string(readFixture("rejection.html")), authbrowser.LoginRejected},
		{"maintenance fixture", string(readFixture("maintenance.html")), authbrowser.Maintenance},
		{"invalid CAPTCHA new revision", string(readFixture("invalid-captcha.html")), authbrowser.CaptchaRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			navigateHTML(tc.html)
			o, e := client.Observe(ctx, attempt)
			if e != nil || o.State != tc.state {
				t.Fatalf("fixture state: %+v %v", o, e)
			}
			if tc.state == authbrowser.Unknown {
				if _, e = client.SubmitLogin(ctx, attempt, authbrowser.LoginInput{Revision: o.Revision, Username: "fixture-user", Password: "fixture-password", AccountNumber: "222222222"}); !authbrowser.IsHTTPStatus(e, 422) {
					t.Fatal("unsafe controls did not fail closed")
				}
			}
		})
	}
	navigateHTML(string(otp))
	observation, err = client.Observe(ctx, attempt)
	if err != nil {
		t.Fatal(err)
	}
	// Browser enforces stricter fixture maxlength/pattern, not merely broker ASCII limits.
	if _, err = client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: observation.Revision, Value: "12345"}); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("OTP violating live form pattern was submitted")
	}
	if _, err = client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: observation.Revision, Value: "001234"}); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("consumed ambiguous action was replayed")
	}
	navigateHTML(strings.Replace(string(auth), "222222222", "333333333", 1))
	observation, err = client.Observe(ctx, attempt)
	if err != nil || observation.State == authbrowser.Authenticated || observation.ReasonCode != "ACCOUNT_SELECTION_REQUIRED" {
		t.Fatal("missing exact account did not fail closed")
	}
	if _, err = client.Handoff(ctx, attempt); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("stale authenticated handoff remained accessible")
	}
	// Production must not infer codes from synthetic data attributes.
	s.opMu.Lock()
	s.fixtureDOM = false
	s.opMu.Unlock()
	navigateHTML(string(readFixture("rejection.html")))
	observation, err = client.Observe(ctx, attempt)
	if err != nil || observation.State != authbrowser.Unknown {
		t.Fatal("production adapter trusted a fixture rejection code")
	}
}

func TestBrowserAutomationObservedACBLoginContract(t *testing.T) {
	if os.Getenv("ACB_BROWSER_INTEGRATION") != "1" {
		t.Skip("opt in with ACB_BROWSER_INTEGRATION=1")
	}
	browser := findDefaultBrowser()
	if _, err := exec.LookPath(browser); err != nil {
		t.Fatal("opt-in browser unavailable: set BROWSER_BIN to an installed Chromium")
	}
	readFixture := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join("testdata", "recovery", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	initial, confirmation, otp, authenticated := readFixture("bank-login.html"), readFixture("observed-otp-request.html"), readFixture("observed-otp-entry.html"), readFixture("authenticated.html")
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 160, 50))); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	loginCount, requestCount, otpCount, unexpectedPostCount := 0, 0, 0, 0
	credentialsOK, requestOK := false, false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Render the observed promotional frame without reaching any real bank.
		w.Header().Set("Content-Security-Policy", "frame-src 'none'")
		if r.URL.Path == "/acbib/Captcha.jpg" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(imageBytes.Bytes())
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == http.MethodGet {
			http.SetCookie(w, &http.Cookie{Name: "fixture_session", Value: "synthetic-cookie", Path: "/", HttpOnly: true})
			_, _ = w.Write(initial)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Form.Get("dse_operationName") == "detectLoginNewDeviceProc" && r.Form.Get("dse_processorState") == "otpPage" {
			otpCount++
			if r.Form.Get("EdtOtp") != "001234" || r.Form.Get("dse_nextEventName") != "ok" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write(authenticated)
			return
		}
		if r.Form.Get("dse_operationName") == "detectLoginNewDeviceProc" {
			requestCount++
			requestOK = r.Form.Get("dse_processorState") == "confirmPage" && r.Form.Get("dse_nextEventName") == "ok" && r.Form.Get("AuthTyp") == "synthetic-selected-method"
			_, _ = w.Write(otp)
			return
		}
		if r.Form.Get("UserName") != "" {
			loginCount++
			credentialsOK = r.Form.Get("UserName") == "fixture-user" && r.Form.Get("PassWord") == "fixture-password" && r.Form.Get("SecurityCode") == "AB12CD"
			_, _ = w.Write(confirmation)
			return
		}
		unexpectedPostCount++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()
	s := &server{profiles: t.TempDir(), browserExec: browser, extraFlags: []string{"--headless=new"}, loginURL: upstream.URL + "/acbib/Request", internalToken: "fixture-internal", internalAuthRequired: true}
	mux := http.NewServeMux()
	mux.Handle("DELETE /sessions/{attemptID}", s.requireInternal(http.HandlerFunc(s.cancel)))
	mux.Handle("POST /sessions", s.requireInternal(http.HandlerFunc(s.start)))
	mux.Handle("POST /sessions/{attemptID}/handoff", s.requireInternal(http.HandlerFunc(s.handoff)))
	s.registerAutomation(mux)
	api := httptest.NewServer(mux)
	defer api.Close()
	defer s.stopCurrent()
	client := authbrowser.NewClient(api.URL, "fixture-internal")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const attempt = "observed-bank-fixture"
	if _, err := client.Start(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	observe := func() authbrowser.AuthObservation {
		t.Helper()
		for range 20 {
			o, err := client.Observe(ctx, attempt)
			if authbrowser.IsHTTPStatus(err, http.StatusServiceUnavailable) {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if o.ReasonCode != "CAPTCHA_LOADING" {
				return o
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("fixture CAPTCHA did not load")
		return authbrowser.AuthObservation{}
	}
	o := observe()
	if o.State != authbrowser.LoginForm || !o.CaptchaRequired {
		t.Fatalf("observed ACB form not recognized: %+v", o)
	}
	if again := observe(); again.Revision != o.Revision {
		t.Fatal("unchanged bank CAPTCHA advanced the challenge revision")
	}
	crop, err := client.CaptureCaptcha(ctx, attempt, o.Revision)
	if err != nil {
		t.Fatal(err)
	}
	dimensions, err := png.DecodeConfig(bytes.NewReader(crop))
	if err != nil || dimensions.Width != 160 || dimensions.Height != 50 {
		t.Fatal("bank CAPTCHA returned anything other than its exact image crop")
	}
	input := authbrowser.LoginInput{Revision: o.Revision, Username: "fixture-user", Password: "fixture-password", AccountNumber: "222222222", Captcha: "AB12CD"}
	o, err = client.SubmitLogin(ctx, attempt, input)
	if err != nil || o.State != authbrowser.OTPRequestRequired {
		t.Fatalf("known bank anchor did not advance to observed confirmation: %+v %v", o, err)
	}
	if _, err := client.SubmitLogin(ctx, attempt, input); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("bank anchor allowed replaying credentials")
	}
	requestRevision := o.Revision
	if _, err := client.RequestOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: requestRevision, Value: "001234"}); !authbrowser.IsHTTPStatus(err, 400) {
		t.Fatal("request action accepted an answer")
	}
	if _, err := client.RequestOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: "old-revision"}); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("request action accepted an old revision")
	}
	if _, err := client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: requestRevision, Value: "001234"}); !authbrowser.IsHTTPStatus(err, 422) {
		t.Fatal("method radio was treated as an OTP input")
	}
	o, err = client.RequestOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: requestRevision})
	if err != nil || o.State != authbrowser.OTPRequired || o.OTPLength != 6 {
		t.Fatalf("observed confirmation did not reach the observed six-digit OTP form: %+v %v", o, err)
	}
	if _, err := client.RequestOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: requestRevision}); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("confirmation revision was replayed after navigation")
	}
	if _, err := client.RequestOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: o.Revision}); !authbrowser.IsHTTPStatus(err, 422) {
		t.Fatal("request action was accepted outside the exact confirmation state")
	}
	otpRevision := o.Revision
	for _, value := range []string{"12345", "1234567", "１２３４５６", "12345x"} {
		if _, err := client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: otpRevision, Value: value}); !authbrowser.IsHTTPStatus(err, 400) {
			t.Fatal("invalid split OTP did not fail before consumption")
		}
		if again := observe(); again.State != authbrowser.OTPRequired || again.Revision != otpRevision {
			t.Fatal("invalid split OTP consumed or changed the live revision")
		}
	}
	mu.Lock()
	beforeReplyOK := otpCount == 0 && requestCount == 1
	mu.Unlock()
	if !beforeReplyOK {
		t.Fatal("OTP submitted before a valid owner answer")
	}
	o, err = client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: o.Revision, Value: "001234"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: otpRevision, Value: "001234"}); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("OTP was submitted more than once")
	}
	for range 5 {
		if o.State == authbrowser.Authenticated {
			break
		}
		o = observe()
	}
	handoff, err := client.Handoff(ctx, attempt)
	if err != nil || o.State != authbrowser.Authenticated {
		t.Fatal("bank login did not reach verified exact-account handoff")
	}
	decoded, err := authbrowser.DecodeHandoff(handoff)
	if err != nil || decoded.Fields["AccountNbr"] != "222222222" {
		t.Fatal("bank fixture selected an account other than the requested one")
	}
	mu.Lock()
	countsOK := loginCount == 1 && requestCount == 1 && otpCount == 1 && unexpectedPostCount == 0 && credentialsOK && requestOK
	mu.Unlock()
	if !countsOK {
		t.Fatal("observed login/confirmation/split OTP did not preserve exact single native submissions and leading zero")
	}
	for _, tc := range []struct{ name, html, reason string }{
		{"authentication frame", strings.Replace(string(initial), "https://acb.com.vn/acbo-tin-tuc-acbonline", "https://example.invalid/challenge", 1), "FRAME_UNSUPPORTED"},
		{"frame inside form", strings.Replace(string(initial), "</form>", `<iframe id="iframe-banner" src="https://acb.com.vn/acbo-tin-tuc-acbonline"></iframe></form>`, 1), "FRAME_UNSUPPORTED"},
		{"unknown anchor handler", strings.Replace(string(initial), "submitFormLogin();sendInsider('ins_login_start');", "otherLogin();", 1), "AMBIGUOUS_SUBMIT"},
		{"ambiguous submit", strings.Replace(string(initial), "</form>", "<button type=\"submit\">Other</button></form>", 1), "AMBIGUOUS_SUBMIT"},
		{"unsafe CAPTCHA", strings.Replace(string(initial), "width=\"160\"", "width=\"2000\"", 1), "UNSAFE_CAPTCHA_CROP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.opMu.Lock()
			err := s.withAutomationTab(ctx, s.session, func(tab context.Context, _ target.ID) error {
				return chromedp.Run(tab, chromedp.Evaluate("document.open();document.write("+jsString(tc.html)+");document.close();", nil))
			})
			s.opMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			o := observe()
			if o.State != authbrowser.Unknown || o.ReasonCode != tc.reason {
				t.Fatalf("unsafe bank page accepted: %+v", o)
			}
			if _, err := client.CaptureCaptcha(ctx, attempt, o.Revision); !authbrowser.IsHTTPStatus(err, 422) {
				t.Fatal("unsafe bank page exposed a CAPTCHA capture")
			}
			if _, err := client.SubmitLogin(ctx, attempt, authbrowser.LoginInput{Revision: o.Revision, Username: "fixture-user", Password: "fixture-password", AccountNumber: "222222222", Captcha: "AB12CD"}); !authbrowser.IsHTTPStatus(err, 422) {
				t.Fatal("unsafe bank page submitted credentials")
			}
		})
	}
	navigateConfirmation := func(html string) {
		t.Helper()
		s.opMu.Lock()
		defer s.opMu.Unlock()
		if err := s.withAutomationTab(ctx, s.session, func(tab context.Context, _ target.ID) error {
			return chromedp.Run(tab, chromedp.Evaluate("document.open();document.write("+jsString(html)+");document.close();", nil))
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertNoExtraPosts := func() {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if loginCount != 1 || requestCount != 1 || otpCount != 1 || unexpectedPostCount != 0 {
			t.Fatal("guarded challenge produced an extra bank-fixture request")
		}
	}
	for _, tc := range []struct{ name, html, reason string }{
		{"wrong form name", strings.Replace(string(otp), `name="form"`, `name="other"`, 1), "AMBIGUOUS_CONTROLS"},
		{"wrong operation", strings.Replace(string(otp), `value="detectLoginNewDeviceProc"`, `value="otherProc"`, 1), "UNRECOGNIZED_PAGE"},
		{"duplicate operation", strings.Replace(string(otp), "</form>", `<input type="hidden" name="dse_operationName" value="detectLoginNewDeviceProc"></form>`, 1), "UNRECOGNIZED_PAGE"},
		{"wrong state", strings.Replace(string(otp), `value="otpPage"`, `value="confirmPage"`, 1), "UNRECOGNIZED_PAGE"},
		{"wrong origin", strings.Replace(string(otp), `action="/acbib/Request"`, `action="https://example.invalid/acbib/Request"`, 1), "WRONG_FORM_ORIGIN"},
		{"wrong method", strings.Replace(string(otp), `method="post"`, `method="get"`, 1), "UNRECOGNIZED_PAGE"},
		{"wrong path", strings.Replace(string(otp), `action="/acbib/Request"`, `action="/acbib/Other"`, 1), "UNRECOGNIZED_PAGE"},
		{"request query", strings.Replace(string(otp), `action="/acbib/Request"`, `action="/acbib/Request?other=1"`, 1), "UNRECOGNIZED_PAGE"},
		{"duplicate form", strings.Replace(string(otp), "</body>", `<form name="form"></form></body>`, 1), "AMBIGUOUS_CONTROLS"},
		{"unknown digit", strings.Replace(string(otp), `id="digit-6"`, `id="digit-7"`, 1), "AMBIGUOUS_CONTROLS"},
		{"duplicate digit", strings.Replace(string(otp), "</form>", `<input type="text" id="digit-1" maxlength="1"></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"missing digit", strings.Replace(string(otp), `<input type="text" id="digit-6" maxlength="1">`, "", 1), "AMBIGUOUS_CONTROLS"},
		{"named digit", strings.Replace(string(otp), `id="digit-1"`, `id="digit-1" name="otp"`, 1), "AMBIGUOUS_CONTROLS"},
		{"readonly digit", strings.Replace(string(otp), `id="digit-1"`, `id="digit-1" readonly`, 1), "AMBIGUOUS_CONTROLS"},
		{"disabled digit", strings.Replace(string(otp), `id="digit-1"`, `id="digit-1" disabled`, 1), "AMBIGUOUS_CONTROLS"},
		{"hidden digit", strings.Replace(string(otp), `id="digit-1"`, `id="digit-1" style="display:none"`, 1), "AMBIGUOUS_CONTROLS"},
		{"detached digit", strings.Replace(string(otp), `id="digit-1"`, `id="digit-1" form="missing"`, 1), "AMBIGUOUS_CONTROLS"},
		{"wrong digit type", strings.Replace(string(otp), `type="text" id="digit-1"`, `type="number" id="digit-1"`, 1), "AMBIGUOUS_CONTROLS"},
		{"wrong digit length", strings.Replace(string(otp), `maxlength="1"`, `maxlength="2"`, 1), "AMBIGUOUS_CONTROLS"},
		{"duplicate hidden code", strings.Replace(string(otp), "</form>", `<input type="hidden" name="EdtOtp" id="EdtOtp"></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"missing hidden code", strings.Replace(string(otp), `<input type="hidden" name="EdtOtp" id="EdtOtp" value="synthetic-unfilled-code">`, "", 1), "AMBIGUOUS_CONTROLS"},
		{"detached hidden code", strings.Replace(string(otp), `id="EdtOtp"`, `id="EdtOtp" form="missing"`, 1), "AMBIGUOUS_CONTROLS"},
		{"visible hidden code", strings.Replace(string(otp), `type="hidden" name="EdtOtp"`, `type="text" name="EdtOtp"`, 1), "AMBIGUOUS_CONTROLS"},
		{"visible signature", strings.Replace(string(otp), `id="Signature" style="display:none"`, `id="Signature"`, 1), "AMBIGUOUS_CONTROLS"},
		{"visible disabled signature", strings.Replace(string(otp), `id="Signature" style="display:none"`, `id="Signature" disabled`, 1), "AMBIGUOUS_CONTROLS"},
		{"unknown hidden field", strings.Replace(string(otp), "</form>", `<input type="hidden" name="other"></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"unknown unnamed hidden field", strings.Replace(string(otp), "</form>", `<input type="hidden" id="other"></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"duplicate resend node", strings.Replace(string(otp), "</form>", `<input type="hidden" id="resend-otp"></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"named resend spoof", strings.Replace(string(otp), `id="resend-otp"`, `id="resend-otp" name="resend-otp"`, 1), "AMBIGUOUS_CONTROLS"},
		{"visible resend node", strings.Replace(string(otp), `type="hidden" id="resend-otp"`, `type="text" id="resend-otp"`, 1), "AMBIGUOUS_CONTROLS"},
		{"detached resend node", strings.Replace(string(otp), `id="resend-otp"`, `id="resend-otp" form="missing"`, 1), "AMBIGUOUS_CONTROLS"},
		{"extra input", strings.Replace(string(otp), "</form>", `<input name="other"></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"extra submit", strings.Replace(string(otp), "</form>", `<button type="submit">Other</button></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"extra handler", strings.Replace(string(otp), "</form>", `<a onclick="submitForm('ok');">Other</a></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"digit handler", strings.Replace(string(otp), `id="digit-1"`, `id="digit-1" onclick="otherSubmit();"`, 1), "AMBIGUOUS_CONTROLS"},
		{"hidden field handler", strings.Replace(string(otp), `id="resend-otp"`, `id="resend-otp" onchange="otherSubmit();"`, 1), "AMBIGUOUS_CONTROLS"},
		{"form handler", strings.Replace(string(otp), `<form name="form"`, `<form onsubmit="otherSubmit();" name="form"`, 1), "AMBIGUOUS_CONTROLS"},
		{"altered confirm handler", strings.Replace(string(otp), "submitForm('ok');", "otherSubmit();", 1), "AMBIGUOUS_SUBMIT"},
		{"altered cancel handler", strings.Replace(string(otp), "submitForm('close');", "otherSubmit();", 1), "AMBIGUOUS_SUBMIT"},
		{"detached confirm", strings.Replace(string(otp), `id="button"`, `id="button" form="missing"`, 1), "AMBIGUOUS_SUBMIT"},
		{"duplicate cancel", strings.Replace(string(otp), "</form>", `<input type="button" id="button2" name="button2" value="Hủy" onclick="submitForm('close');"></form>`, 1), "AMBIGUOUS_SUBMIT"},
		{"outside code", strings.Replace(string(otp), "</body>", `<form><input name="otp"></form></body>`, 1), "AMBIGUOUS_CONTROLS"},
		{"outside password", strings.Replace(string(otp), "</body>", `<form><input type="password"></form></body>`, 1), "AMBIGUOUS_CONTROLS"},
		{"associated outside input", strings.Replace(string(otp), `<form name="form"`, `<form id="otp-form" name="form"`, 1) + `<input name="other" form="otp-form">`, "AMBIGUOUS_CONTROLS"},
		{"hidden frame", strings.Replace(string(otp), "</body>", `<iframe style="display:none" src="about:blank"></iframe></body>`, 1), "FRAME_UNSUPPORTED"},
	} {
		t.Run("split OTP/"+tc.name, func(t *testing.T) {
			navigateConfirmation(tc.html)
			o := observe()
			if o.State != authbrowser.Unknown || o.ReasonCode != tc.reason || o.OTPLength != 0 {
				t.Fatalf("unsafe split OTP admitted: %+v", o)
			}
			if _, err := client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: o.Revision, Value: "001234"}); !authbrowser.IsHTTPStatus(err, 422) {
				t.Fatal("unsafe split OTP allowed submission")
			}
			assertNoExtraPosts()
		})
	}
	evaluateOTP := func(script string, output any) {
		t.Helper()
		s.opMu.Lock()
		defer s.opMu.Unlock()
		if err := s.withAutomationTab(ctx, s.session, func(tab context.Context, _ target.ID) error {
			return chromedp.Run(tab, chromedp.Evaluate(script, output))
		}); err != nil {
			t.Fatal(err)
		}
	}
	navigateConfirmation(string(otp))
	o = observe()
	if o.State != authbrowser.OTPRequired || o.OTPLength != 6 || observe().Revision != o.Revision {
		t.Fatal("observed split form with unrelated navigation was not stable")
	}
	var splitFingerprint, changedPrivateFingerprint string
	evaluateOTP(automationDOMLibrary+`;inspectRecovery(false).fingerprint`, &splitFingerprint)
	for _, value := range []string{"synthetic-private-session", "synthetic-unfilled-code", "synthetic-resend", "synthetic-signature", "Xác nhận", "detectLoginNewDeviceProc", "otpPage"} {
		if strings.Contains(splitFingerprint, value) {
			t.Fatal("split OTP fingerprint included field values")
		}
	}
	evaluateOTP(automationDOMLibrary+`;
document.getElementById('digit-1').value='8';
document.getElementById('EdtOtp').value='different-hidden-code';
document.getElementById('resend-otp').value='different-resend-token';
document.querySelector('[name="dse_sessionId"]').value='different-private-session';
document.getElementById('Signature').value='different-private-signature';
inspectRecovery(false).fingerprint`, &changedPrivateFingerprint)
	if changedPrivateFingerprint != splitFingerprint || observe().Revision != o.Revision {
		t.Fatal("digit or hidden private values changed structural OTP identity")
	}
	// A synthetic no-navigation handler proves that the adapter sets only the six
	// current digit nodes, emits ordinary events, and clicks confirm exactly once.
	evaluateOTP(`window.submitForm=event=>{
 window.fixtureNativeClicks=(window.fixtureNativeClicks||0)+1;
 window.fixtureSplitFilled=event==='ok' && Array.from({length:6},(_,i)=>document.getElementById('digit-'+(i+1)).value).join('')==='001234';
 window.fixtureHiddenUntouched=document.getElementById('EdtOtp').value==='different-hidden-code' && document.getElementById('resend-otp').value==='different-resend-token';
};`, nil)
	consumedOTPRevision := o.Revision
	o, err = client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: consumedOTPRevision, Value: "001234"})
	if err != nil || o.State != authbrowser.Unknown || o.ReasonCode != "ACTION_OUTCOME_UNKNOWN" || o.Revision != consumedOTPRevision {
		t.Fatalf("unknown split OTP native outcome was not consumed: %+v %v", o, err)
	}
	if _, err := client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: consumedOTPRevision, Value: "001234"}); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("unknown split OTP outcome permitted replay")
	}
	var splitClickedOnce bool
	evaluateOTP(`window.fixtureNativeClicks===1 && window.fixtureSplitFilled && window.fixtureHiddenUntouched && !window.fixtureCancelClicks && Object.values(window.fixtureDigitEvents).every(events=>['input','change','keydown','keypress','keyup'].every(type=>events[type]))`, &splitClickedOnce)
	if !splitClickedOnce {
		t.Fatal("split OTP did not fill digits through normal events and click only native confirm")
	}
	assertNoExtraPosts()
	for _, mutation := range []struct{ name, script string }{
		{"confirm handler", `document.getElementById('digit-6').addEventListener('change',()=>document.getElementById('button').setAttribute('onclick','otherSubmit();'));`},
		{"cancel handler", `document.getElementById('digit-6').addEventListener('change',()=>document.getElementById('button2').setAttribute('onclick','otherSubmit();'));`},
		{"replacement digit", `document.getElementById('digit-6').addEventListener('change',()=>{const el=document.getElementById('digit-1');el.replaceWith(el.cloneNode());});`},
		{"detached digit", `document.getElementById('digit-6').addEventListener('change',()=>document.getElementById('digit-1').setAttribute('form','missing'));`},
	} {
		t.Run("split OTP pre-click/"+mutation.name, func(t *testing.T) {
			navigateConfirmation(string(otp))
			o := observe()
			evaluateOTP(mutation.script, nil)
			if _, err := client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: o.Revision, Value: "001234"}); !authbrowser.IsHTTPStatus(err, 409) {
				t.Fatal("changed split OTP contract was clicked")
			}
			if _, err := client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: o.Revision, Value: "001234"}); !authbrowser.IsHTTPStatus(err, 409) {
				t.Fatal("changed split OTP contract permitted replay")
			}
			assertNoExtraPosts()
		})
	}
	for _, tc := range []struct{ name, html, reason string }{
		{"wrong form name", strings.Replace(string(confirmation), `name="form"`, `name="other"`, 1), "AMBIGUOUS_CONTROLS"},
		{"wrong method", strings.Replace(string(confirmation), `method="post"`, `method="get"`, 1), "UNRECOGNIZED_PAGE"},
		{"wrong operation", strings.Replace(string(confirmation), `value="detectLoginNewDeviceProc"`, `value="otherProc"`, 1), "UNRECOGNIZED_PAGE"},
		{"wrong processor state", strings.Replace(string(confirmation), `value="confirmPage"`, `value="otherPage"`, 1), "UNRECOGNIZED_PAGE"},
		{"wrong request origin", strings.Replace(string(confirmation), `action="/acbib/Request"`, `action="https://example.invalid/acbib/Request"`, 1), "WRONG_FORM_ORIGIN"},
		{"wrong request path", strings.Replace(string(confirmation), `action="/acbib/Request"`, `action="/acbib/Other"`, 1), "UNRECOGNIZED_PAGE"},
		{"unexpected request query", strings.Replace(string(confirmation), `action="/acbib/Request"`, `action="/acbib/Request?other=1"`, 1), "UNRECOGNIZED_PAGE"},
		{"duplicate operation", strings.Replace(string(confirmation), "</form>", `<input type="hidden" name="dse_operationName" value="detectLoginNewDeviceProc"></form>`, 1), "UNRECOGNIZED_PAGE"},
		{"duplicate processor state", strings.Replace(string(confirmation), "</form>", `<input type="hidden" name="dse_processorState" value="confirmPage"></form>`, 1), "UNRECOGNIZED_PAGE"},
		{"visible operation", strings.Replace(string(confirmation), `type="hidden" name="dse_operationName"`, `type="text" name="dse_operationName"`, 1), "UNRECOGNIZED_PAGE"},
		{"duplicate radio", strings.Replace(string(confirmation), "</form>", `<input type="radio" name="AuthTyp" id="safekey"></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"other method radio", strings.Replace(string(confirmation), "</form>", `<input type="radio" name="AuthTyp" id="other"></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"disabled method radio", strings.Replace(string(confirmation), `id="safekey"`, `id="safekey" disabled`, 1), "AMBIGUOUS_CONTROLS"},
		{"hidden method radio", strings.Replace(string(confirmation), `id="safekey"`, `id="safekey" style="display:none"`, 1), "AMBIGUOUS_CONTROLS"},
		{"wrong method name", strings.Replace(string(confirmation), `name="AuthTyp"`, `name="other"`, 1), "AMBIGUOUS_CONTROLS"},
		{"detached radio", strings.Replace(string(confirmation), `id="safekey"`, `id="safekey" form="missing"`, 1), "AMBIGUOUS_CONTROLS"},
		{"detached button", strings.Replace(string(confirmation), `id="button"`, `id="button" form="missing"`, 1), "AMBIGUOUS_SUBMIT"},
		{"duplicate button", strings.Replace(string(confirmation), "</form>", `<input type="button" id="button" name="button" value="Tiếp tục" onclick="submitForm('ok');"></form>`, 1), "AMBIGUOUS_SUBMIT"},
		{"disabled button", strings.Replace(string(confirmation), `id="button"`, `id="button" disabled`, 1), "AMBIGUOUS_SUBMIT"},
		{"hidden button", strings.Replace(string(confirmation), `id="button"`, `id="button" style="display:none"`, 1), "AMBIGUOUS_SUBMIT"},
		{"wrong button type", strings.Replace(string(confirmation), `type="button"`, `type="submit"`, 1), "AMBIGUOUS_SUBMIT"},
		{"wrong button handler", strings.Replace(string(confirmation), "submitForm('ok');", "otherSubmit();", 1), "AMBIGUOUS_SUBMIT"},
		{"wrong button label", strings.Replace(string(confirmation), "Tiếp tục", "Other", 1), "AMBIGUOUS_SUBMIT"},
		{"extra submit", strings.Replace(string(confirmation), "</form>", `<button type="submit">Other</button></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"extra code field", strings.Replace(string(confirmation), "</form>", `<input name="otp"></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"extra handler", strings.Replace(string(confirmation), "</form>", `<a href="#" onclick="submitForm('ok');">Other</a></form>`, 1), "AMBIGUOUS_CONTROLS"},
		{"competing confirmation form", strings.Replace(string(confirmation), "</body>", `<form name="form" action="/acbib/Request" method="post"></form></body>`, 1), "AMBIGUOUS_CONTROLS"},
		{"outside code field", strings.Replace(string(confirmation), "</body>", `<form><input name="otp"></form></body>`, 1), "AMBIGUOUS_CONTROLS"},
		{"outside associated code field", strings.Replace(string(confirmation), `<form name="form"`, `<form id="confirmation" name="form"`, 1) + `<input name="otp" form="confirmation">`, "AMBIGUOUS_CONTROLS"},
		{"confirmation frame", strings.Replace(string(confirmation), "</body>", `<iframe src="about:blank"></iframe></body>`, 1), "FRAME_UNSUPPORTED"},
		{"hidden confirmation frame", strings.Replace(string(confirmation), "</body>", `<iframe style="display:none" src="about:blank"></iframe></body>`, 1), "FRAME_UNSUPPORTED"},
	} {
		t.Run("confirmation/"+tc.name, func(t *testing.T) {
			navigateConfirmation(tc.html)
			o := observe()
			if o.State != authbrowser.Unknown || o.ReasonCode != tc.reason {
				t.Fatalf("unsafe confirmation accepted: %+v", o)
			}
			if _, err := client.RequestOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: o.Revision}); !authbrowser.IsHTTPStatus(err, 422) {
				t.Fatal("unsafe confirmation permitted request action")
			}
			assertNoExtraPosts()
		})
	}
	navigateConfirmation(strings.Replace(string(confirmation), `type="radio"`, `type="text"`, 1))
	if o := observe(); o.State != authbrowser.UnsupportedChallenge {
		t.Fatal("unevidenced SafeKey text input was admitted")
	}
	// An unchanged page after the native button click is an unknown outcome, not
	// permission to replay. This handler deliberately makes no fixture bank request.
	navigateConfirmation(string(confirmation))
	o = observe()
	if o.State != authbrowser.OTPRequestRequired {
		t.Fatalf("observed SafeKey method radio not admitted: %+v", o)
	}
	if again := observe(); again.Revision != o.Revision {
		t.Fatal("unchanged confirmation advanced its revision")
	}
	var fingerprint string
	s.opMu.Lock()
	err = s.withAutomationTab(ctx, s.session, func(tab context.Context, _ target.ID) error {
		return chromedp.Run(tab, chromedp.Evaluate(automationDOMLibrary+`;
window.submitForm=()=>{window.fixtureRequestClicks=(window.fixtureRequestClicks||0)+1;};
inspectRecovery(false).fingerprint`, &fingerprint))
	})
	s.opMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, privateValue := range []string{"synthetic-private-session", "synthetic-selected-method", "Tiếp tục", "detectLoginNewDeviceProc", "confirmPage"} {
		if strings.Contains(fingerprint, privateValue) {
			t.Fatal("confirmation fingerprint contains a field value")
		}
	}
	var afterPrivateChange string
	s.opMu.Lock()
	err = s.withAutomationTab(ctx, s.session, func(tab context.Context, _ target.ID) error {
		return chromedp.Run(tab, chromedp.Evaluate(automationDOMLibrary+`;
document.querySelector('[name="dse_sessionId"]').value='different-private-session';
document.getElementById('safekey').value='different-selected-method';
document.cookie='private_fixture_cookie=private-cookie-value';
inspectRecovery(false).fingerprint`, &afterPrivateChange))
	})
	s.opMu.Unlock()
	if err != nil || afterPrivateChange != fingerprint {
		t.Fatal("private field/cookie values influenced the confirmation fingerprint")
	}
	if again := observe(); again.Revision != o.Revision {
		t.Fatal("private field/cookie changes advanced the confirmation revision")
	}
	consumedRevision := o.Revision
	o, err = client.RequestOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: consumedRevision})
	if err != nil || o.State != authbrowser.Unknown || o.ReasonCode != "ACTION_OUTCOME_UNKNOWN" || o.Revision != consumedRevision {
		t.Fatalf("ambiguous native confirmation click was not consumed: %+v %v", o, err)
	}
	if _, err := client.RequestOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: consumedRevision}); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("unknown confirmation result permitted replay")
	}
	var clickedOnce bool
	s.opMu.Lock()
	err = s.withAutomationTab(ctx, s.session, func(tab context.Context, _ target.ID) error {
		return chromedp.Run(tab, chromedp.Evaluate(`window.fixtureRequestClicks===1 && document.getElementById('safekey').checked`, &clickedOnce))
	})
	s.opMu.Unlock()
	if err != nil || !clickedOnce {
		t.Fatal("confirmation did not select the actual DOM radio and click once")
	}
	assertNoExtraPosts()
	// Radio event handlers can change the button. Reinspect after selection and
	// immediately before clicking; a changed contract must have no POST side effect.
	navigateConfirmation(string(confirmation))
	o = observe()
	s.opMu.Lock()
	err = s.withAutomationTab(ctx, s.session, func(tab context.Context, _ target.ID) error {
		return chromedp.Run(tab, chromedp.Evaluate(`document.getElementById('safekey').onchange=()=>document.getElementById('button').setAttribute('onclick','otherSubmit();');`, nil))
	})
	s.opMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RequestOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: o.Revision}); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("changed confirmation handler was clicked after selecting the radio")
	}
	if _, err := client.RequestOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: o.Revision}); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("failed pre-click inspection permitted replay")
	}
	assertNoExtraPosts()
	t.Run("authentication requires rendered loaded top-level evidence", func(t *testing.T) {
		for _, html := range []string{
			`<html><body><div style="display:none"><a href="?op=ibkLogoutOp">Đăng xuất</a><h2>Thông tin tài khoản</h2><span>Xin chào</span><input name="AccountNbr"><input name="dse_operationName" value="ibkacctDetailProc"></div></body></html>`,
			`<html><body><div><span style="display:none">Xin chào Đăng xuất Thông tin tài khoản</span><p>Ordinary visible content</p></div></body></html>`,
			`<html><body><input type="hidden" name="dse_operationName" value="ibkLogoutOp"><input type="hidden" name="AccountNbr"><div style="display:none">Xin chào</div></body></html>`,
			strings.Replace(string(authenticated), "</body>", `<iframe src="about:blank"></iframe></body>`, 1),
			`<html><body><a href="?op=ibkLogoutOp">Đăng xuất</a><input type="hidden" name="dse_operationName" value="ibkacctDetailProc"><input type="hidden" name="dse_processorState" value="accountPage"></body></html>`,
			strings.Replace(string(authenticated), "</body>", `<iframe style="display:none" src="about:blank"></iframe></body>`, 1),
		} {
			navigateConfirmation(html)
			var signals domSignals
			evaluateOTP(acbDOMCheckScript, &signals)
			if signals.isAuthenticated() {
				t.Fatal("non-rendered or framed evidence authenticated the page")
			}
			if o := observe(); o.State == authbrowser.Authenticated {
				t.Fatal("observer overrode unsupported page evidence")
			}
		}
		navigateConfirmation(string(authenticated))
		evaluateOTP(`Object.defineProperty(document,'readyState',{configurable:true,get:()=> 'loading'});`, nil)
		var signals domSignals
		evaluateOTP(acbDOMCheckScript, &signals)
		if signals.isAuthenticated() {
			t.Fatal("partial document authenticated")
		}
		evaluateOTP(`delete document.readyState;`, nil)
		evaluateOTP(acbDOMCheckScript, &signals)
		if !signals.isAuthenticated() {
			t.Fatal("complete rendered synthetic history contract was rejected")
		}
	})
	t.Run("account ambiguity has no onchange and navigation never replays", func(t *testing.T) {
		form := `<form method="post" action="/acbib/Request"><input type="hidden" name="dse_operationName" value="ibkacctDetailProc"><select name="AccountNbr" onchange="window.accountChanges=(window.accountChanges||0)+1"><option value="111111111">Other</option><option value="222222222">Target</option></select></form>`
		for _, html := range []string{form + form, strings.Replace(form, "</form>", `<input type="hidden" name="AccountNbr" value="222222222"></form>`, 1)} {
			navigateConfirmation(html)
			var unchanged bool
			evaluateOTP(automaticHistoryScript+`('222222222',true); !window.accountChanges && document.querySelector('select').value==='111111111'`, &unchanged)
			if !unchanged {
				t.Fatal("ambiguous target caused selection side effect")
			}
		}
		navigateConfirmation(`<html><body><a href="?op=ibkLogoutOp">Đăng xuất</a><h2>Thông tin tài khoản</h2>` + form + `</body></html>`)
		s.opMu.Lock()
		s.session.accountNavigation = false
		s.opMu.Unlock()
		if o := observe(); o.ReasonCode != "ACCOUNT_SELECTION_PENDING" {
			t.Fatalf("selection did not begin: %+v", o)
		}
		evaluateOTP(`const previous=document.querySelector('select');previous.replaceWith(previous.cloneNode(true));document.querySelector('select').value='111111111';`, nil)
		for range 3 {
			if o := observe(); o.ReasonCode != "ACCOUNT_SELECTION_PENDING" {
				t.Fatalf("rerendered selection not pending: %+v", o)
			}
		}
		var noSelectionReplay bool
		evaluateOTP(`window.accountChanges===1 && document.querySelector('select').value==='111111111'`, &noSelectionReplay)
		if !noSelectionReplay {
			t.Fatal("rerendered exact account selection repeated onchange")
		}
		navigateConfirmation(`<a href="/acbib/Request?dse_operationName=ibkacctDetailProc&AccountNbr=222222222" onclick="event.preventDefault();window.accountClicks=(window.accountClicks||0)+1">Exact target</a><h2>Thông tin tài khoản</h2><a href="?op=ibkLogoutOp">Đăng xuất</a>`)
		s.opMu.Lock()
		s.session.accountNavigation = false
		s.opMu.Unlock()
		for range 3 {
			if o := observe(); o.ReasonCode != "ACCOUNT_SELECTION_PENDING" {
				t.Fatalf("navigation outcome: %+v", o)
			}
		}
		evaluateOTP(`document.querySelector('a').replaceWith(document.querySelector('a').cloneNode(true));`, nil)
		observe()
		var once bool
		evaluateOTP(`window.accountClicks===1`, &once)
		if !once {
			t.Fatal("unchanged or rerendered account navigation replayed")
		}
		assertNoExtraPosts()
	})
	t.Run("recorded structures and unknown evidence survive cancellation privately", func(t *testing.T) {
		for _, html := range []string{string(confirmation), string(otp)} {
			navigateConfirmation(html)
			var summary automationDiagnosticSummary
			evaluateOTP(automationDiagnosticScript+"("+acbDOMCheckScript+")", &summary)
			data := string(summary.boundedJSON())
			if !strings.Contains(data, "detectLoginNewDeviceProc") || !strings.Contains(data, "OK_NATIVE") {
				t.Fatal("recorded protocol/native structure missing")
			}
			for _, private := range []string{"synthetic-private-session", "synthetic-selected-method", "synthetic-unfilled-code", "synthetic-resend", "synthetic-signature", "synthetic-plaintext"} {
				if strings.Contains(data, private) {
					t.Fatal("recorded fixture private value leaked")
				}
			}
		}
		var oversized automationDiagnosticSummary
		navigateConfirmation(`<form method="post" action="/private-path"><input type="hidden" name="dse_operationName" value="a9fc28bProc"></form>`)
		evaluateOTP(`for(let i=0;i<100;i++){const el=document.createElement('input');el.name='token-'+i+'-private';el.id='123456789-'+i;el.value='private-secret';document.body.append(el);}for(let i=0;i<6;i++)document.body.append(document.createElement('form'));`+automationDiagnosticScript+"("+acbDOMCheckScript+")", &oversized)
		if len(oversized.Controls) > 24 || len(oversized.Forms) > 4 || !oversized.Truncated || len(oversized.boundedJSON()) > maxAutomationDiagnosticBytes {
			t.Fatal("browser metadata was not bounded at capture")
		}
		if data, _ := json.Marshal(oversized); bytes.Contains(data, []byte("private-secret")) || bytes.Contains(data, []byte("a9fc28bProc")) || bytes.Contains(data, []byte("123456789")) {
			t.Fatal("browser capture returned token metadata")
		}
		const private = "private-123456789-account-session-password"
		navigateConfirmation(`<html><body>` + private + `<form name="` + private + `" id="` + private + `" method="post" action="/acbib/Request?secret=` + private + `"><input name="` + private + `" id="` + private + `" value="` + private + `"><input type="password" value="` + private + `"><input type="hidden" name="dse_operationName" value="` + private + `Proc"><input type="hidden" name="dse_processorState" value="` + private + `Page"><input type="hidden" name="dse_pageId" value="123456"><button onclick="throw '` + private + `'">` + private + `</button></form></body></html>`)
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
		defer slog.SetDefault(previous)
		s.opMu.Lock()
		s.session.diagnostics = automationDiagnostics{owned: true}
		s.opMu.Unlock()
		if o := observe(); o.State != authbrowser.Unknown {
			t.Fatal("unknown structure was admitted")
		}
		before := logs.String()
		if !strings.Contains(before, "ACB automation unknown page structure") || !strings.Contains(before, "UNKNOWN_IDENTIFIER") || !strings.Contains(before, "UNKNOWN_PROTOCOL") {
			t.Fatal("unknown did not persist classified structural evidence before return")
		}
		observe()
		if logs.String() != before {
			t.Fatal("unchanged structure repeated diagnostic")
		}
		evaluateOTP(`document.querySelector('input').value='different-private-value';document.querySelector('form').name='different-private-name';document.querySelector('button').textContent='different-private-text';`, nil)
		observe()
		if logs.String() != before {
			t.Fatal("private values influenced structural diagnostics")
		}
		if err := client.Cancel(ctx, attempt); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), before) || strings.Contains(logs.String(), private) || strings.Contains(logs.String(), "different-private") {
			t.Fatal("cancellation lost evidence or leaked private data")
		}
		assertNoExtraPosts()
	})
}

func jsString(value string) string { payload, _ := json.Marshal(value); return string(payload) }

func TestAutomationDiagnosticsPrivateAndBounded(t *testing.T) {
	const secret = "private-123456789-password-cookie-session"
	summary := automationDiagnosticSummary{ReadyState: secret, FrameCount: 1000}
	for range 100 {
		summary.Forms = append(summary.Forms, automationDiagnosticForm{Tag: secret, Name: secret, ID: secret, Method: secret, Action: "https://user:" + secret + "@example.invalid/private?token=" + secret})
		summary.Controls = append(summary.Controls, automationDiagnosticControl{Tag: secret, Name: secret, ID: secret, Type: secret, Native: secret, MaxLength: 100000})
		summary.Protocol = append(summary.Protocol, automationDiagnosticProtocol{Name: "dse_operationName", Identifier: secret + "Proc"})
	}
	data := summary.boundedJSON()
	if len(data) > maxAutomationDiagnosticBytes || bytes.Contains(data, []byte(secret)) {
		t.Fatal("unbounded or private diagnostic output")
	}
	var bounded automationDiagnosticSummary
	if err := json.Unmarshal(data, &bounded); err != nil {
		t.Fatal(err)
	}
	if !bounded.Truncated || len(bounded.Forms) > 4 || len(bounded.Controls) > 24 || len(bounded.Protocol) > 3 || bounded.FrameCount != 100 {
		t.Fatal("metadata caps not applied")
	}
	for _, tc := range []struct{ name, value, want string }{
		{"dse_operationName", "detectLoginNewDeviceProc", "detectLoginNewDeviceProc"},
		{"dse_operationName", "ibkacctDetailProc", "ibkacctDetailProc"},
		{"dse_processorState", "otpPage", "otpPage"},
		{"dse_processorState", "confirmPage", "confirmPage"},
		{"dse_pageId", "5", "5"},
		{"dse_pageId", "001234", "UNKNOWN_PROTOCOL"},
		{"dse_operationName", "a8fbb718Op", "UNKNOWN_PROTOCOL"},
		{"dse_processorState", "arbitraryPrivateValueState", "UNKNOWN_PROTOCOL"},
		{"dse_operationName", strings.Repeat("login", 30) + "Proc", "UNKNOWN_PROTOCOL"},
	} {
		if got := diagnosticProtocol(tc.name, tc.value); got != tc.want {
			t.Fatalf("protocol classification %s: %s", tc.name, got)
		}
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	diagnostics := automationDiagnostics{}
	if diagnostics.record(summary) || logs.Len() != 0 {
		t.Fatal("unowned session emitted evidence")
	}
	diagnostics.owned = true
	if !diagnostics.record(summary) || diagnostics.record(summary) {
		t.Fatal("distinct snapshot not deduplicated")
	}
	for i := range maxAutomationDiagnostics + 5 {
		distinct := automationDiagnosticSummary{ReadyState: "complete", FrameCount: i}
		diagnostics.record(distinct)
	}
	if diagnostics.count != maxAutomationDiagnostics || strings.Count(logs.String(), "ACB automation unknown page structure") != maxAutomationDiagnostics || strings.Contains(logs.String(), secret) {
		t.Fatal("finite diagnostic log cap/privacy violated")
	}
}
