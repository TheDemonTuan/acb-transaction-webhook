package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
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
	if err != nil || observation.State != authbrowser.OTPRequired {
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
	initial, confirmation, otp, authenticated := readFixture("bank-login.html"), readFixture("observed-otp-request.html"), readFixture("otp.html"), readFixture("authenticated.html")
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
		if r.URL.Query().Get("stage") == "otp" {
			otpCount++
			if r.Form.Get("otp") != "001234" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write(authenticated)
			return
		}
		if r.Form.Get("dse_operationName") == "detectLoginNewDeviceProc" {
			requestCount++
			requestOK = r.Form.Get("dse_processorState") == "confirmPage" && r.Form.Get("dse_nextEventName") == "ok" && r.Form.Get("AuthTyp") == "synthetic-selected-method"
			_, _ = w.Write(otp) // Synthetic next page; not evidence of real ACB OTP selectors.
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
	if err != nil || o.State != authbrowser.OTPRequired {
		t.Fatalf("observed confirmation did not reach the synthetic numeric OTP fixture: %+v %v", o, err)
	}
	if _, err := client.RequestOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: requestRevision}); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("confirmation revision was replayed after navigation")
	}
	if _, err := client.RequestOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: o.Revision}); !authbrowser.IsHTTPStatus(err, 422) {
		t.Fatal("request action was accepted outside the exact confirmation state")
	}
	otpRevision := o.Revision
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
		t.Fatal("observed login/confirmation and synthetic OTP did not preserve exact single submissions")
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
			t.Fatal("guarded confirmation produced an extra bank-fixture request")
		}
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
}

func jsString(value string) string { payload, _ := json.Marshal(value); return string(payload) }
