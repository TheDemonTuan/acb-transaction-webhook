package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
)

func TestValidHistoryForm(t *testing.T) {
	cases := []struct {
		name string
		form browserFormState
		want bool
	}{
		{
			name: "empty",
			form: browserFormState{},
			want: false,
		},
		{
			name: "summary page rejected",
			form: browserFormState{
				Action: "https://online.acb.com.vn/acbib/Request",
				Fields: map[string]string{
					"dse_sessionId":      "session123",
					"dse_processorState": "initial",
					"dse_operationName":  "ibkacctSumProc",
				},
			},
			want: false,
		},
		{
			name: "detail page missing account rejected",
			form: browserFormState{
				Action: "https://online.acb.com.vn/acbib/Request",
				Fields: map[string]string{
					"dse_sessionId":      "session123",
					"dse_processorState": "acctDetailPage",
					"dse_operationName":  "ibkacctDetailProc",
					"AccountNbr":         "",
				},
			},
			want: false,
		},
		{
			name: "detail page with account accepted",
			form: browserFormState{
				Action: "https://online.acb.com.vn/acbib/Request",
				Fields: map[string]string{
					"dse_sessionId":      "session123",
					"dse_processorState": "acctDetailPage",
					"dse_operationName":  "ibkacctDetailProc",
					"AccountNbr":         "40478827",
				},
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validHistoryForm(tc.form); got != tc.want {
				t.Fatalf("validHistoryForm(%+v) = %v, want %v", tc.form, got, tc.want)
			}
		})
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)
	args := os.Args
	for len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		args = args[1:]
	}
	if len(args) == 0 {
		return
	}
	mode := args[0]
	switch mode {
	case "exit1":
		os.Exit(1)
	case "mock-browser":
		var port string
		for _, arg := range args {
			if strings.HasPrefix(arg, "--remote-debugging-port=") {
				port = strings.TrimPrefix(arg, "--remote-debugging-port=")
			}
		}
		if port != "" {
			listener, err := net.Listen("tcp", "127.0.0.1:"+port)
			if err == nil {
				mux := http.NewServeMux()
				mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]string{
						"webSocketDebuggerUrl": "ws://127.0.0.1:" + port + "/devtools/browser/1",
					})
				})
				mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
					_ = json.NewEncoder(w).Encode([]map[string]string{{"type": "page", "url": defaultACBLoginURL}})
				})
				server := &http.Server{Handler: mux}
				go server.Serve(listener)
				defer server.Close()
			}
		}
		time.Sleep(30 * time.Second)
	case "sleep":
		time.Sleep(30 * time.Second)
	}
}

func fakeBrowserCmd(ctx context.Context, mode string, extraArgs ...string) *exec.Cmd {
	args := []string{"-test.run=TestHelperProcess", "--", mode}
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	return cmd
}

func TestWaitBrowserReady(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/browser/1"})
		case "/json/list":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"type": "page", "url": defaultACBLoginURL}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan struct{})
	if err := waitBrowserReady(ctx, server.URL, done, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWaitBrowserReadyRequiresStableACBTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/browser/1"})
		case "/json/list":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"type": "page", "url": "about:blank"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if err := waitBrowserReady(ctx, server.URL, make(chan struct{}), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline while ACB target is unavailable", err)
	}
}

func TestWaitBrowserReadyReportsEarlyExit(t *testing.T) {
	done := make(chan struct{})
	close(done)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	item := &browserSession{exitErr: errors.New("exit status 1")}
	if err := waitBrowserReady(ctx, "http://127.0.0.1:1", done, item); err == nil {
		t.Fatal("expected early exit error")
	}
}

func TestEncodeHandoffKeepsCookiesBeforeNonce(t *testing.T) {
	handoff, err := encodeHandoff("https://online.acb.com.vn/acbib/AccountSummary", []*network.Cookie{{Name: "JSESSIONID", Value: "secret", Domain: ".acb.com.vn", Path: "/"}}, browserFormState{})
	if err != nil {
		t.Fatal(err)
	}
	_, encodedNonce, ok := strings.Cut(handoff, ".")
	if !ok || encodedNonce == "" {
		t.Fatalf("invalid handoff framing: %q", handoff)
	}
	decoded, err := authbrowser.DecodeHandoff(handoff)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Version != 1 || decoded.URL != "https://online.acb.com.vn/acbib/AccountSummary" {
		t.Fatalf("unexpected handoff metadata: %+v", decoded)
	}
	if len(decoded.Cookies) != 1 || decoded.Cookies[0].Name != "JSESSIONID" || decoded.Cookies[0].Value != "secret" {
		t.Fatalf("unexpected cookies: %+v", decoded.Cookies)
	}
}

func TestEncodeHandoffPreservesSessionCookie(t *testing.T) {
	handoff, err := encodeHandoff("https://online.acb.com.vn/acbib/AccountSummary", []*network.Cookie{{Name: "JSESSIONID", Value: "secret", Domain: "online.acb.com.vn", Path: "/", Expires: -1, Secure: true, HTTPOnly: true}}, browserFormState{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := authbrowser.DecodeHandoff(handoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(decoded.Cookies))
	}
	if !decoded.Cookies[0].Expires.IsZero() {
		t.Fatalf("session cookie expiry = %s, want zero", decoded.Cookies[0].Expires)
	}
}

func TestEncodeHandoffPreservesPersistentCookieExpiry(t *testing.T) {
	expected := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	handoff, err := encodeHandoff("https://online.acb.com.vn/acbib/AccountSummary", []*network.Cookie{{Name: "persistent", Value: "secret", Domain: "online.acb.com.vn", Path: "/", Expires: float64(expected.Unix())}}, browserFormState{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := authbrowser.DecodeHandoff(handoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Cookies) != 1 || !decoded.Cookies[0].Expires.Equal(expected) {
		t.Fatalf("persistent cookie expiry = %s, want %s", decoded.Cookies[0].Expires, expected)
	}
}

func TestAuthenticatedACBRejectsLoginRoutes(t *testing.T) {
	cookies := []*network.Cookie{{Name: "JSESSIONID", Domain: ".acb.com.vn", Value: "present"}}
	signals := domSignals{
		HasLogout:           true,
		HasAccountOverview:  true,
		HasWelcome:          true,
		HasAccountProcessor: true,
		HasProcessorState:   true,
	}
	rejected := []string{
		"https://online.acb.com.vn/login",
		"https://online.acb.com.vn/acbib/Request?op=OBKLoginOp",
		"https://online.acb.com.vn",
		"https://example.com/home",
	}
	for _, location := range rejected {
		if authenticatedACB(location, signals, cookies) {
			t.Fatalf("authenticated login or non-app URL %q", location)
		}
	}
	if !authenticatedACB("https://online.acb.com.vn/acbib/AccountSummary", signals, cookies) {
		t.Fatal("rejected authenticated ACB URL with valid signals")
	}
}

func TestAuthenticatedACB_SameURLLoggedOutAndIn(t *testing.T) {
	const acbURL = "https://online.acb.com.vn/acbib/Request"
	validCookies := []*network.Cookie{{Name: "JSESSIONID", Domain: ".online.acb.com.vn", Value: "session-secret"}}

	// Case 1: Verified production DOM signals -> ACCEPTED
	authSignals := domSignals{
		HasLogout:           true,
		HasAccountOverview:  true,
		HasWelcome:          true,
		HasAccountProcessor: true,
		HasProcessorState:   true,
		VisiblePassword:     false,
		VisibleCaptcha:      false,
	}
	if !authenticatedACB(acbURL, authSignals, validCookies) {
		t.Fatal("expected production verified DOM signals at /acbib/Request to be authenticated")
	}

	// Case 2: Same URL but logged out (visible password) -> REJECTED
	loggedOutSignals := authSignals
	loggedOutSignals.VisiblePassword = true
	if authenticatedACB(acbURL, loggedOutSignals, validCookies) {
		t.Fatal("expected /acbib/Request with visible password to be rejected")
	}

	// Case 3: Same URL but login form visible -> REJECTED
	loginVisibleSignals := authSignals
	loginVisibleSignals.VisibleLogin = true
	if authenticatedACB(acbURL, loginVisibleSignals, validCookies) {
		t.Fatal("expected /acbib/Request with visible login to be rejected")
	}

	// Case 4: Same URL with visible OTP -> REJECTED
	otpSignals := authSignals
	otpSignals.VisibleOTP = true
	if authenticatedACB(acbURL, otpSignals, validCookies) {
		t.Fatal("expected /acbib/Request with visible OTP to be rejected")
	}

	// Case 5: Same URL with visible CAPTCHA -> REJECTED
	captchaSignals := authSignals
	captchaSignals.VisibleCaptcha = true
	if authenticatedACB(acbURL, captchaSignals, validCookies) {
		t.Fatal("expected /acbib/Request with visible CAPTCHA to be rejected")
	}

	// Case 6: Same URL without logout element -> REJECTED
	noLogoutSignals := authSignals
	noLogoutSignals.HasLogout = false
	if authenticatedACB(acbURL, noLogoutSignals, validCookies) {
		t.Fatal("expected /acbib/Request without logout to be rejected")
	}

	// Case 7: Only 1 positive signal (requires multiple positive signals) -> REJECTED
	singleSignal := domSignals{HasLogout: true}
	if authenticatedACB(acbURL, singleSignal, validCookies) {
		t.Fatal("expected /acbib/Request with single positive signal to be rejected")
	}

	// Case 8: Logout + WebSphere ProcessorState only (no account/welcome signals) -> REJECTED
	logoutAndProcessorOnly := domSignals{
		HasLogout:         true,
		HasProcessorState: true,
	}
	if authenticatedACB(acbURL, logoutAndProcessorOnly, validCookies) {
		t.Fatal("expected /acbib/Request with only logout and processor state (no account/welcome) to be rejected")
	}
}

func TestAuthenticatedACB_CookieOnlyRejection(t *testing.T) {
	validCookies := []*network.Cookie{
		{Name: "JSESSIONID", Domain: ".online.acb.com.vn", Value: "session-secret"},
		{Name: "ROUTEID", Domain: ".acb.com.vn", Value: "route-1"},
	}

	// Cookie present but blank/zero DOM signals -> REJECTED
	zeroSignals := domSignals{}
	if authenticatedACB("https://online.acb.com.vn/acbib/Request", zeroSignals, validCookies) {
		t.Fatal("expected cookie-only state at /acbib/Request to be rejected")
	}
	if authenticatedACB("https://online.acb.com.vn/acbib/AccountSummary", zeroSignals, validCookies) {
		t.Fatal("expected cookie-only state at /acbib/AccountSummary to be rejected")
	}
}

func TestAuthenticatedACB_SpoofedHostAndScheme(t *testing.T) {
	validCookies := []*network.Cookie{{Name: "JSESSIONID", Domain: ".online.acb.com.vn", Value: "secret"}}
	authSignals := domSignals{
		HasLogout:          true,
		HasAccountOverview: true,
	}

	spoofedURLs := []string{
		"https://evil.com/acbib/Request",
		"https://spoofed.host/acbib/Request",
		"https://online.acb.com.vn.attacker.com/acbib/Request",
		"https://attacker-online.acb.com.vn/acbib/Request",
		"https://online.acb.com.vn.evil/acbib/Request",
		"http://online.acb.com.vn/acbib/Request", // Unencrypted HTTP must be rejected
	}
	for _, rawURL := range spoofedURLs {
		if authenticatedACB(rawURL, authSignals, validCookies) {
			t.Fatalf("expected spoofed or unencrypted URL %q to be rejected", rawURL)
		}
	}
}

func TestCookieFilteringAndDomainBoundary(t *testing.T) {
	validScopedCookies := []*network.Cookie{
		{Name: "session1", Domain: ".online.acb.com.vn", Value: "val1"},
		{Name: "session2", Domain: "online.acb.com.vn", Value: "val2"},
		{Name: "session3", Domain: ".acb.com.vn", Value: "val3"},
		{Name: "session4", Domain: "acb.com.vn", Value: "val4"},
	}
	for _, c := range validScopedCookies {
		if !isValidACBCookie(c) {
			t.Fatalf("expected cookie with domain %q to be valid", c.Domain)
		}
	}

	invalidCookies := []*network.Cookie{
		{Name: "bad1", Domain: "evil-acb.com.vn", Value: "val"},
		{Name: "bad2", Domain: "notacb.com.vn", Value: "val"},
		{Name: "bad3", Domain: "attacker.com", Value: "val"},
		{Name: "bad4", Domain: "online.acb.com.vn.evil.com", Value: "val"},
		{Name: "bad5", Domain: ".online.acb.com.vn", Value: ""}, // Empty value
		{Name: "", Domain: ".online.acb.com.vn", Value: "val"},  // Empty name
		nil,
	}
	for _, c := range invalidCookies {
		if isValidACBCookie(c) {
			t.Fatalf("expected cookie %+v to be invalid", c)
		}
	}

	// Mixed cookies passed to encodeHandoff: only valid ACB cookies are retained
	mixed := append(validScopedCookies, invalidCookies...)
	handoff, err := encodeHandoff("https://online.acb.com.vn/acbib/AccountSummary", mixed, browserFormState{})
	if err != nil {
		t.Fatalf("encodeHandoff failed: %v", err)
	}
	decoded, err := authbrowser.DecodeHandoff(handoff)
	if err != nil {
		t.Fatalf("decode handoff: %v", err)
	}
	if len(decoded.Cookies) != len(validScopedCookies) {
		t.Fatalf("expected %d filtered cookies, got %d", len(validScopedCookies), len(decoded.Cookies))
	}

	// Only invalid cookies: encodeHandoff must error
	if _, err := encodeHandoff("https://online.acb.com.vn/acbib/AccountSummary", invalidCookies, browserFormState{}); err == nil {
		t.Fatal("expected error when encodeHandoff has no valid ACB cookies")
	}
}

func TestTerminalStatus(t *testing.T) {
	for _, status := range []string{"FAILED", "EXPIRED", "CANCELLED", "COMPLETED"} {
		if !terminalStatus(status) {
			t.Fatalf("expected %s to be terminal", status)
		}
	}
	if terminalStatus("AWAITING_USER_LOGIN") {
		t.Fatal("waiting status must not be terminal")
	}
	if terminalStatus("STARTING") {
		t.Fatal("starting status must not be terminal")
	}
	if terminalStatus("VERIFIED") {
		t.Fatal("verified status must not be terminal")
	}
}

func TestAllocateFreePort(t *testing.T) {
	port1, err := allocateFreePort()
	if err != nil {
		t.Fatalf("allocate port 1: %v", err)
	}
	port2, err := allocateFreePort()
	if err != nil {
		t.Fatalf("allocate port 2: %v", err)
	}
	if port1 <= 0 || port2 <= 0 {
		t.Fatalf("invalid ports allocated: %d, %d", port1, port2)
	}
	if port1 == 9222 || port2 == 9222 {
		t.Fatalf("unexpected hardcoded port 9222 allocated: %d, %d", port1, port2)
	}
}

func TestSafeStatusTransitions(t *testing.T) {
	s := &server{
		session: &browserSession{
			AttemptID: "test-attempt",
			Status:    "AWAITING_USER_LOGIN",
		},
	}

	// Transition from waiting to verified
	if !s.setStatus("test-attempt", "VERIFIED", "") {
		t.Fatal("expected status transition to VERIFIED to succeed")
	}
	if s.session.Status != "VERIFIED" {
		t.Fatalf("got status %q, want VERIFIED", s.session.Status)
	}

	// Transition from verified to cancelled (terminal)
	if !s.setStatus("test-attempt", "CANCELLED", "") {
		t.Fatal("expected status transition to CANCELLED to succeed")
	}

	// Attempting to overwrite terminal status with FAILED must fail
	if s.setStatus("test-attempt", "FAILED", "some error") {
		t.Fatal("expected setStatus to reject transition from terminal CANCELLED")
	}
	if s.session.Status != "CANCELLED" {
		t.Fatalf("terminal status was overwritten: %q", s.session.Status)
	}

	// Attempting to overwrite with EXPIRED must also fail
	if s.setStatus("test-attempt", "EXPIRED", "expired") {
		t.Fatal("expected setStatus to reject transition from terminal CANCELLED")
	}
	if s.session.Status != "CANCELLED" {
		t.Fatalf("terminal status was overwritten: %q", s.session.Status)
	}
}

func TestCancelReapsProcessAndPreservesCancelledStatus(t *testing.T) {
	profileDir := t.TempDir()
	s := &server{
		profiles: profileDir,
		cmdFunc: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return fakeBrowserCmd(ctx, "mock-browser", args...)
		},
	}

	body := bytes.NewBufferString(`{"attemptId":"cancel-reap-test"}`)
	req := httptest.NewRequest(http.MethodPost, "/sessions", body)
	w := httptest.NewRecorder()
	s.start(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("start returned HTTP %d: %s", w.Code, w.Body.String())
	}

	session := s.session
	if session == nil || session.AttemptID != "cancel-reap-test" {
		t.Fatal("session not created")
	}
	if session.Status != "AWAITING_USER_LOGIN" {
		t.Fatalf("got status %q, want AWAITING_USER_LOGIN", session.Status)
	}

	// Cancel session
	cancelReq := httptest.NewRequest(http.MethodDelete, "/sessions/cancel-reap-test", nil)
	cancelReq.SetPathValue("attemptID", "cancel-reap-test")
	cancelW := httptest.NewRecorder()
	s.cancel(cancelW, cancelReq)
	if cancelW.Code != http.StatusNoContent {
		t.Fatalf("cancel returned HTTP %d", cancelW.Code)
	}

	// Process must be reaped (done channel closed)
	select {
	case <-session.done:
	default:
		t.Fatal("process was not reaped after cancel")
	}

	// Check status: must NOT be 404! It must return 200 with CANCELLED
	statusReq := httptest.NewRequest(http.MethodGet, "/sessions/cancel-reap-test/status", nil)
	statusReq.SetPathValue("attemptID", "cancel-reap-test")
	statusW := httptest.NewRecorder()
	s.status(statusW, statusReq)
	if statusW.Code != http.StatusOK {
		t.Fatalf("status after cancel returned HTTP %d: %s (expected 200 OK)", statusW.Code, statusW.Body.String())
	}

	var statusResp browserSession
	if err := json.NewDecoder(statusW.Body).Decode(&statusResp); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if statusResp.Status != "CANCELLED" {
		t.Fatalf("got status %q, want CANCELLED", statusResp.Status)
	}
}

func TestRestartReapsTerminalSession(t *testing.T) {
	profileDir := t.TempDir()
	s := &server{
		profiles: profileDir,
		cmdFunc: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return fakeBrowserCmd(ctx, "mock-browser", args...)
		},
	}

	// Start first session
	w1 := httptest.NewRecorder()
	s.start(w1, httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(`{"attemptId":"session-1"}`)))
	if w1.Code != http.StatusCreated {
		t.Fatalf("first start failed: HTTP %d %s", w1.Code, w1.Body.String())
	}
	sess1 := s.session

	// Cancel first session
	cancelReq := httptest.NewRequest(http.MethodDelete, "/sessions/session-1", nil)
	cancelReq.SetPathValue("attemptID", "session-1")
	cancelW := httptest.NewRecorder()
	s.cancel(cancelW, cancelReq)
	if cancelW.Code != http.StatusNoContent {
		t.Fatalf("cancel failed: HTTP %d", cancelW.Code)
	}

	// Start second session immediately - must succeed without 409 conflict
	w2 := httptest.NewRecorder()
	s.start(w2, httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(`{"attemptId":"session-2"}`)))
	if w2.Code != http.StatusCreated {
		t.Fatalf("second start failed: HTTP %d %s", w2.Code, w2.Body.String())
	}
	sess2 := s.session

	if sess2.AttemptID != "session-2" {
		t.Fatalf("expected session-2, got %q", sess2.AttemptID)
	}
	if sess1.debugURL == sess2.debugURL {
		t.Fatalf("both sessions got identical debugURL: %s", sess1.debugURL)
	}

	// Clean up second session
	s.stopCurrent()
}

func TestActiveSessionConflict(t *testing.T) {
	profileDir := t.TempDir()
	s := &server{
		profiles: profileDir,
		cmdFunc: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return fakeBrowserCmd(ctx, "mock-browser", args...)
		},
	}
	defer s.stopCurrent()

	w1 := httptest.NewRecorder()
	s.start(w1, httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(`{"attemptId":"active-1"}`)))
	if w1.Code != http.StatusCreated {
		t.Fatalf("start 1 returned HTTP %d: %s", w1.Code, w1.Body.String())
	}

	w2 := httptest.NewRecorder()
	s.start(w2, httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(`{"attemptId":"active-2"}`)))
	if w2.Code != http.StatusConflict {
		t.Fatalf("start 2 returned HTTP %d (expected 409 Conflict): %s", w2.Code, w2.Body.String())
	}
}

func TestFakeProcessStartupFailureCleanedUp(t *testing.T) {
	profileDir := t.TempDir()
	s := &server{
		profiles: profileDir,
		cmdFunc: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return fakeBrowserCmd(ctx, "exit1", args...)
		},
	}

	w := httptest.NewRecorder()
	s.start(w, httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(`{"attemptId":"fail-startup"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got HTTP %d, want 503 Service Unavailable: %s", w.Code, w.Body.String())
	}

	if s.session.Status != "FAILED" {
		t.Fatalf("got status %q, want FAILED", s.session.Status)
	}

	// Profile directory must be cleaned up
	profilePath := filepath.Join(profileDir, "fail-startup")
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Fatalf("profile directory %q was not cleaned up after startup failure", profilePath)
	}
}

func TestObserverBoundedRetryPreservesSession(t *testing.T) {
	s := &server{
		session: &browserSession{
			AttemptID: "observer-retry-test",
			Status:    "AWAITING_USER_LOGIN",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	done := make(chan struct{})
	// Point to an unavailable endpoint: 127.0.0.1 on a port not in use
	go s.observeLogin(ctx, "observer-retry-test", "http://127.0.0.1:59999", done)

	// Wait for context to expire (observer will have retried several times)
	<-ctx.Done()

	// Crucial: The session must NOT be marked FAILED or CANCELLED by the observer
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session.Status != "AWAITING_USER_LOGIN" {
		t.Fatalf("observer mutated session status: %q (expected AWAITING_USER_LOGIN)", s.session.Status)
	}
}

func TestHandoffRequiresInternalToken(t *testing.T) {
	s := &server{
		internalToken:        "internal-secret",
		internalAuthRequired: true,
		session: &browserSession{
			AttemptID: "handoff-auth-test",
			Status:    "VERIFIED",
			verified:  true,
			handoff:   "sensitive-handoff",
		},
	}
	request := httptest.NewRequest(http.MethodPost, "/sessions/handoff-auth-test/handoff", nil)
	request.SetPathValue("attemptID", "handoff-auth-test")

	unauthorized := httptest.NewRecorder()
	s.requireInternal(http.HandlerFunc(s.handoff)).ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized handoff status = %d, want 401", unauthorized.Code)
	}
	if strings.Contains(unauthorized.Body.String(), "sensitive-handoff") {
		t.Fatal("unauthorized response leaked handoff")
	}

	request.Header.Set(authbrowser.InternalTokenHeader, "internal-secret")
	authorized := httptest.NewRecorder()
	s.requireInternal(http.HandlerFunc(s.handoff)).ServeHTTP(authorized, request)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized handoff status = %d: %s", authorized.Code, authorized.Body.String())
	}
}

func TestHandoffReapsProcessAndSetsCompleted(t *testing.T) {
	profileDir := t.TempDir()
	s := &server{
		profiles: profileDir,
		cmdFunc: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return fakeBrowserCmd(ctx, "mock-browser", args...)
		},
	}

	w := httptest.NewRecorder()
	s.start(w, httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(`{"attemptId":"handoff-test"}`)))
	if w.Code != http.StatusCreated {
		t.Fatalf("start failed: HTTP %d %s", w.Code, w.Body.String())
	}
	session := s.session

	// Simulate successful verification
	s.mu.Lock()
	session.verified = true
	session.handoff = "test-token.nonce"
	session.Status = "VERIFIED"
	s.mu.Unlock()

	// Call handoff
	handoffReq := httptest.NewRequest(http.MethodPost, "/sessions/handoff-test/handoff", nil)
	handoffReq.SetPathValue("attemptID", "handoff-test")
	handoffW := httptest.NewRecorder()
	s.handoff(handoffW, handoffReq)
	if handoffW.Code != http.StatusOK {
		t.Fatalf("handoff returned HTTP %d: %s", handoffW.Code, handoffW.Body.String())
	}

	var resp struct {
		Session string `json:"session"`
	}
	if err := json.NewDecoder(handoffW.Body).Decode(&resp); err != nil {
		t.Fatalf("decode handoff response: %v", err)
	}
	if resp.Session != "test-token.nonce" {
		t.Fatalf("got session token %q, want test-token.nonce", resp.Session)
	}

	// Handoff is retryable and keeps the browser alive until acknowledged.
	handoffW2 := httptest.NewRecorder()
	s.handoff(handoffW2, handoffReq)
	if handoffW2.Code != http.StatusOK {
		t.Fatalf("second handoff returned HTTP %d: %s", handoffW2.Code, handoffW2.Body.String())
	}
	select {
	case <-session.done:
		t.Fatal("process was reaped before handoff completion")
	default:
	}

	completeReq := httptest.NewRequest(http.MethodPost, "/sessions/handoff-test/complete", nil)
	completeReq.SetPathValue("attemptID", "handoff-test")
	completeW := httptest.NewRecorder()
	s.complete(completeW, completeReq)
	if completeW.Code != http.StatusNoContent {
		t.Fatalf("complete returned HTTP %d: %s", completeW.Code, completeW.Body.String())
	}
	select {
	case <-session.done:
	default:
		t.Fatal("process was not reaped after handoff completion")
	}

	// Status must be COMPLETED
	statusReq := httptest.NewRequest(http.MethodGet, "/sessions/handoff-test/status", nil)
	statusReq.SetPathValue("attemptID", "handoff-test")
	statusW := httptest.NewRecorder()
	s.status(statusW, statusReq)
	if statusW.Code != http.StatusOK {
		t.Fatalf("status returned HTTP %d: %s", statusW.Code, statusW.Body.String())
	}
	var statusResp browserSession
	if err := json.NewDecoder(statusW.Body).Decode(&statusResp); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if statusResp.Status != "COMPLETED" {
		t.Fatalf("got status %q, want COMPLETED", statusResp.Status)
	}
}

func TestRealChromiumIntegration(t *testing.T) {
	if os.Getenv("ACB_BROWSER_INTEGRATION") != "1" {
		t.Skip("set ACB_BROWSER_INTEGRATION=1 to run the real browser integration test")
	}
	browserBin := findDefaultBrowser()
	if browserBin == "" || browserBin == "chromium" {
		if _, err := exec.LookPath("chromium"); err != nil {
			t.Skip("no local Chromium or Edge installed, skipping real integration test")
		}
	}

	profileDir := t.TempDir()
	s := &server{
		profiles:    profileDir,
		browserExec: browserBin,
		extraFlags:  []string{"--headless=new"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	port, err := allocateFreePort()
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}

	item := &browserSession{
		AttemptID: "real-browser-test",
		Status:    "STARTING",
		ScreenURL: "/",
		ExpiresAt: time.Now().UTC().Add(sessionTTL),
		cancel:    cancel,
		debugURL:  fmt.Sprintf("http://127.0.0.1:%d", port),
		done:      make(chan struct{}),
	}
	s.session = item

	ready := make(chan error, 1)
	go s.launch(ctx, item, port, ready)

	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("real browser launch failed: %v", err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("timed out waiting for real browser ready")
	}

	if item.Status != "AWAITING_USER_LOGIN" {
		t.Fatalf("got status %q, want AWAITING_USER_LOGIN", item.Status)
	}

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-item.done:
			t.Fatalf("real browser exited while observer was active: %v", item.exitErr)
		case <-time.After(250 * time.Millisecond):
		}
		s.mu.Lock()
		status := item.Status
		s.mu.Unlock()
		if status != "AWAITING_USER_LOGIN" {
			t.Fatalf("observer changed status to %q before login", status)
		}
	}

	// Cancel and reap
	s.reapSession(item, 5*time.Second)

	select {
	case <-item.done:
	default:
		t.Fatal("real browser was not reaped")
	}

	// Verify profile is deleted
	profilePath := filepath.Join(profileDir, "real-browser-test")
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Fatalf("profile directory was not cleaned up: %v", err)
	}
}

func TestRealisticCDP_PageDOMSignals(t *testing.T) {
	if os.Getenv("ACB_BROWSER_INTEGRATION") != "1" {
		t.Skip("set ACB_BROWSER_INTEGRATION=1 to run the real browser integration test")
	}
	browserBin := findDefaultBrowser()
	if browserBin == "" || browserBin == "chromium" {
		if _, err := exec.LookPath("google-chrome"); err != nil {
			if _, err := exec.LookPath("chromium"); err != nil {
				t.Skip("no local Chromium or Chrome installed, skipping realistic CDP test")
			}
		}
	}

	loginHTML := `<!DOCTYPE html><html><body>
		<form action="/acbib/Request" method="POST">
			<input type="text" name="username" placeholder="Username">
			<input type="password" name="password" placeholder="Password">
			<input type="text" name="captcha" placeholder="Captcha">
			<button id="loginBtn">Đăng nhập</button>
		</form>
	</body></html>`

	authHTML := `<!DOCTYPE html><html><body>
		<div class="header">
			<span>Xin chào NGUYEN VAN A</span>
			<a href="/acbib/Request?op=ibkLogoutOp" id="btnLogout">Đăng xuất</a>
		</div>
		<div class="content">
			<span>Thông tin tài khoản</span>
			<div id="accountoverview">Tài khoản thanh toán: 123456789</div>
			<input type="hidden" name="dse_operationName" value="ibkacctDetailProc">
			<input type="hidden" name="dse_processorState" value="fresh_state_abc">
			<input type="hidden" name="AccountNbr" value="123456789">
		</div>
	</body></html>`

	otpHtml := `<!DOCTYPE html><html><body>
		<form action="/acbib/Request" method="POST">
			<input type="text" name="otp" placeholder="Mã OTP Safekey">
			<button>Xác nhận</button>
		</form>
	</body></html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/login":
			_, _ = w.Write([]byte(loginHTML))
		case "/auth":
			_, _ = w.Write([]byte(authHTML))
		case "/otp":
			_, _ = w.Write([]byte(otpHtml))
		case "/exit-dialog":
			_, _ = w.Write([]byte(`<!DOCTYPE html><html><body>
				<div><span>Thông báo phiên giao dịch</span><button>Thoát</button>
				<input type="hidden" name="dse_processorState" value="state_test"></div>
			</body></html>`))
		default:
			_, _ = w.Write([]byte("<!DOCTYPE html><html><body><h1>Other</h1></body></html>"))
		}
	}))
	defer server.Close()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.NoSandbox,
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	bctx, bcancel := chromedp.NewContext(allocCtx)
	defer bcancel()

	// Tab 1: Other page
	if err := chromedp.Run(bctx, chromedp.Navigate(server.URL+"/other")); err != nil {
		t.Fatalf("navigate to other: %v", err)
	}

	// Tab 2: Login page
	tab2Ctx, tab2Cancel := chromedp.NewContext(bctx)
	defer tab2Cancel()
	if err := chromedp.Run(tab2Ctx, chromedp.Navigate(server.URL+"/login")); err != nil {
		t.Fatalf("navigate to login: %v", err)
	}

	// Tab 3: OTP challenge page
	tab3Ctx, tab3Cancel := chromedp.NewContext(bctx)
	defer tab3Cancel()
	if err := chromedp.Run(tab3Ctx, chromedp.Navigate(server.URL+"/otp")); err != nil {
		t.Fatalf("navigate to otp: %v", err)
	}

	// Tab 4: Authenticated page
	tab4Ctx, tab4Cancel := chromedp.NewContext(bctx)
	defer tab4Cancel()
	if err := chromedp.Run(tab4Ctx, chromedp.Navigate(server.URL+"/auth")); err != nil {
		t.Fatalf("navigate to auth: %v", err)
	}

	// Tab 5: Modal exit dialog with "Thoát" and hidden processorState
	tab5Ctx, tab5Cancel := chromedp.NewContext(bctx)
	defer tab5Cancel()
	if err := chromedp.Run(tab5Ctx, chromedp.Navigate(server.URL+"/exit-dialog")); err != nil {
		t.Fatalf("navigate to exit-dialog: %v", err)
	}

	targets, err := chromedp.Targets(bctx)
	if err != nil {
		t.Fatalf("get targets: %v", err)
	}
	initialCount := len(targets)

	for _, info := range targets {
		if info.Type != "page" {
			continue
		}
		sig, err := evaluateDOMSignals(context.Background(), bctx, info.TargetID)
		if err != nil {
			t.Fatalf("evaluateDOMSignals on %s failed: %v", info.URL, err)
		}
		switch {
		case strings.HasSuffix(info.URL, "/other"):
			if sig.isAuthenticated() {
				t.Fatalf("expected /other to be not authenticated, got: %+v", sig)
			}
		case strings.HasSuffix(info.URL, "/login"):
			if !sig.VisiblePassword || !sig.VisibleCaptcha || !sig.VisibleLogin {
				t.Fatalf("expected /login to have visible password/captcha/login, got: %+v", sig)
			}
			if sig.HasLogout {
				t.Fatalf("expected /login to have HasLogout false, got: %+v", sig)
			}
			if sig.isAuthenticated() {
				t.Fatalf("expected /login to be unauthenticated, got: %+v", sig)
			}
		case strings.HasSuffix(info.URL, "/otp"):
			if !sig.VisibleOTP {
				t.Fatalf("expected /otp to have VisibleOTP true, got: %+v", sig)
			}
			if sig.isAuthenticated() {
				t.Fatalf("expected /otp to be unauthenticated, got: %+v", sig)
			}
		case strings.HasSuffix(info.URL, "/exit-dialog"):
			if sig.HasLogout {
				t.Fatalf("expected /exit-dialog to NOT match HasLogout with generic 'Thoát', got: %+v", sig)
			}
			if sig.isAuthenticated() {
				t.Fatalf("expected /exit-dialog to be unauthenticated, got: %+v", sig)
			}
		case strings.HasSuffix(info.URL, "/auth"):
			if !sig.HasLogout || !sig.HasAccountOverview || !sig.HasWelcome || !sig.HasAccountProcessor || !sig.HasProcessorState {
				t.Fatalf("expected /auth to have all 5 positive signals, got: %+v", sig)
			}
			if sig.VisiblePassword || sig.VisibleCaptcha || sig.VisibleOTP || sig.VisibleLogin {
				t.Fatalf("expected /auth to have no visible challenge fields, got: %+v", sig)
			}
			if sig.positiveCount() != 5 {
				t.Fatalf("expected positiveCount 5, got %d", sig.positiveCount())
			}
			if !sig.isAuthenticated() {
				t.Fatalf("expected /auth to be authenticated, got: %+v", sig)
			}
		}
	}

	// Verify evaluateDOMSignals immediately aborts on cancelled parent context
	if len(targets) > 0 {
		cancCtx, doCancel := context.WithCancel(context.Background())
		doCancel()
		if _, err := evaluateDOMSignals(cancCtx, bctx, targets[0].TargetID); err == nil {
			t.Fatal("expected evaluateDOMSignals to return error for cancelled parent context")
		}
	}

	time.Sleep(100 * time.Millisecond)
	targetsAfter, err := chromedp.Targets(bctx)
	if err != nil {
		t.Fatalf("get targets after: %v", err)
	}
	if len(targetsAfter) != initialCount {
		t.Fatalf("tabs were closed during evaluation: before %d, after %d", initialCount, len(targetsAfter))
	}
}

func TestRealisticCDP_MultiTabTargetSelectionAndTabReplacement(t *testing.T) {
	if os.Getenv("ACB_BROWSER_INTEGRATION") != "1" {
		t.Skip("set ACB_BROWSER_INTEGRATION=1 to run the real browser integration test")
	}
	browserBin := findDefaultBrowser()
	if browserBin == "" || browserBin == "chromium" {
		if _, err := exec.LookPath("google-chrome"); err != nil {
			if _, err := exec.LookPath("chromium"); err != nil {
				t.Skip("no local Chromium or Chrome installed, skipping realistic CDP test")
			}
		}
	}

	var stateMu sync.Mutex
	isLoggedIn := false

	loginHTML := `<!DOCTYPE html><html><body>
		<form action="/acbib/Request" method="POST">
			<input type="text" name="username" placeholder="Username">
			<input type="password" name="password" placeholder="Password">
			<input type="text" name="captcha" placeholder="Captcha">
			<button id="loginBtn">Đăng nhập</button>
		</form>
	</body></html>`

	authHTML := `<!DOCTYPE html><html><body>
		<div class="header">
			<span>Xin chào NGUYEN VAN A</span>
			<a href="/acbib/Request?op=ibkLogoutOp" id="btnLogout">Đăng xuất</a>
		</div>
		<div class="content">
			<span>Thông tin tài khoản</span>
			<div id="accountoverview">Tài khoản thanh toán: 123456789</div>
			<input type="hidden" name="dse_operationName" value="ibkacctDetailProc">
			<input type="hidden" name="dse_processorState" value="fresh_state_abc">
			<input type="hidden" name="AccountNbr" value="123456789">
		</div>
	</body></html>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/acbib/Request" {
			stateMu.Lock()
			loggedIn := isLoggedIn
			stateMu.Unlock()
			if loggedIn {
				_, _ = w.Write([]byte(authHTML))
			} else {
				_, _ = w.Write([]byte(loginHTML))
			}
			return
		}
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body><h1>Unrelated</h1></body></html>"))
	}))
	defer server.Close()

	t.Setenv("ACB_LOGIN_URL", server.URL+"/acbib/Request")

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.NoSandbox,
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	bctx, bcancel := chromedp.NewContext(allocCtx)
	defer bcancel()

	// Tab 1: Unrelated page (navigating initial context allocates the browser)
	if err := chromedp.Run(bctx, chromedp.Navigate(server.URL+"/unrelated")); err != nil {
		t.Fatalf("navigate to unrelated: %v", err)
	}

	browser := chromedp.FromContext(bctx).Browser
	if browser == nil {
		t.Fatal("browser is nil after initial navigation")
	}
	executor := cdp.WithExecutor(bctx, browser)

	// Tab 2: ACB Banking page at /acbib/Request (initially logged out)
	tab2Ctx, tab2Cancel := chromedp.NewContext(bctx)
	defer tab2Cancel()
	if err := chromedp.Run(tab2Ctx, chromedp.Navigate(server.URL+"/acbib/Request")); err != nil {
		t.Fatalf("navigate to /acbib/Request: %v", err)
	}

	targetsBefore, _ := chromedp.Targets(bctx)
	countBefore := len(targetsBefore)

	// Evaluate while logged out: should not verify
	checkCtx, checkCancel := context.WithTimeout(executor, 5*time.Second)
	currURL, signals, cookies, _, reason, err := browserLoginState(checkCtx, bctx)
	checkCancel()
	if err != nil {
		t.Fatalf("browserLoginState returned error: %v", err)
	}
	if authenticatedACB(currURL, signals, cookies) {
		t.Fatal("expected unauthenticated state while logged out")
	}
	if reason == "" {
		t.Fatal("expected a non-empty waiting reason")
	}

	// Verify no tabs were closed
	targetsAfter1, _ := chromedp.Targets(bctx)
	if len(targetsAfter1) != countBefore {
		t.Fatalf("tab count changed during browserLoginState: before %d, after %d", countBefore, len(targetsAfter1))
	}

	// Now transition to logged in
	stateMu.Lock()
	isLoggedIn = true
	stateMu.Unlock()

	// Set valid session cookie in browser storage for server host
	u, _ := url.Parse(server.URL)
	cookieParams := network.SetCookie("JSESSIONID", "secret-session-token").
		WithDomain(u.Hostname()).
		WithPath("/").
		WithHTTPOnly(true)
	if err := chromedp.Run(tab2Ctx, cookieParams); err != nil {
		t.Fatalf("set cookie: %v", err)
	}

	// Reload Tab 2 to get authenticated DOM
	if err := chromedp.Run(tab2Ctx, chromedp.Reload()); err != nil {
		t.Fatalf("reload tab 2: %v", err)
	}

	// Now evaluate again: should verify
	checkCtx2, checkCancel2 := context.WithTimeout(executor, 5*time.Second)
	currURL, signals, cookies, _, _, err = browserLoginState(checkCtx2, bctx)
	checkCancel2()
	if err != nil {
		t.Fatalf("browserLoginState error on authenticated page: %v", err)
	}
	if !authenticatedACB(currURL, signals, cookies) {
		t.Fatalf("expected authenticatedACB to return true, url=%s, signals=%+v, cookies=%+v", currURL, signals, cookies)
	}

	// Test tab replacement: navigate Tab 2 away to /unrelated, then open Tab 3 with /acbib/Request
	if err := chromedp.Run(tab2Ctx, chromedp.Navigate(server.URL+"/unrelated")); err != nil {
		t.Fatalf("navigate tab 2 away: %v", err)
	}
	tab3Ctx, tab3Cancel := chromedp.NewContext(bctx)
	defer tab3Cancel()
	if err := chromedp.Run(tab3Ctx, chromedp.Navigate(server.URL+"/acbib/Request")); err != nil {
		t.Fatalf("navigate tab 3: %v", err)
	}

	// Evaluate again on replaced tab: should seamlessly identify Tab 3 as authenticated
	checkCtx3, checkCancel3 := context.WithTimeout(executor, 5*time.Second)
	currURL, signals, cookies, _, _, err = browserLoginState(checkCtx3, bctx)
	checkCancel3()
	if err != nil {
		t.Fatalf("browserLoginState error on replaced tab: %v", err)
	}
	if !authenticatedACB(currURL, signals, cookies) {
		t.Fatal("expected replaced tab to be authenticated")
	}
}
