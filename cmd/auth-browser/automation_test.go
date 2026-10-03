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
	initial, otp, authenticated := readFixture("bank-login.html"), readFixture("otp.html"), readFixture("authenticated.html")
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 160, 50))); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	loginCount, otpCount := 0, 0
	credentialsOK := false
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
		loginCount++
		credentialsOK = r.Form.Get("UserName") == "fixture-user" && r.Form.Get("PassWord") == "fixture-password" && r.Form.Get("SecurityCode") == "AB12CD"
		_, _ = w.Write(otp)
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
	if err != nil || o.State != authbrowser.OTPRequired {
		t.Fatalf("known bank anchor did not advance to OTP: %+v %v", o, err)
	}
	if _, err := client.SubmitLogin(ctx, attempt, input); !authbrowser.IsHTTPStatus(err, 409) {
		t.Fatal("bank anchor allowed replaying credentials")
	}
	o, err = client.SubmitOTP(ctx, attempt, authbrowser.ChallengeInput{Revision: o.Revision, Value: "001234"})
	if err != nil {
		t.Fatal(err)
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
	countsOK := loginCount == 1 && otpCount == 1 && credentialsOK
	mu.Unlock()
	if !countsOK {
		t.Fatal("observed bank form did not preserve CAPTCHA and single submissions")
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
}

func jsString(value string) string { payload, _ := json.Marshal(value); return string(payload) }
