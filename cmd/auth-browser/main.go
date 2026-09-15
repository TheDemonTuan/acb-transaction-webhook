package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
)

const (
	defaultACBLoginURL = "https://online.acb.com.vn/acbib/Request"
	sessionTTL         = 15 * time.Minute
	startupLimit       = 10 * time.Second
)

func acbLoginURL() string {
	if value := strings.TrimSpace(os.Getenv("ACB_LOGIN_URL")); value != "" {
		return value
	}
	return defaultACBLoginURL
}

func isValidAttemptID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func newProfileDirName() (string, error) {
	token := make([]byte, 18)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	return "profile-" + base64.RawURLEncoding.EncodeToString(token), nil
}

type browserFormState struct {
	Action string            `json:"action"`
	Fields map[string]string `json:"fields"`
}

type browserSession struct {
	AttemptID string    `json:"attemptId"`
	Status    string    `json:"status"`
	ScreenURL string    `json:"screenUrl"`
	ExpiresAt time.Time `json:"expiresAt"`
	Error     string    `json:"error,omitempty"`
	profile   string
	cancel    context.CancelFunc
	debugURL  string
	handoff   string
	verified  bool

	cmd      *exec.Cmd
	done     chan struct{}
	exitErr  error
	exitOnce sync.Once
}

type browserSessionResponse struct {
	AttemptID string    `json:"attemptId"`
	Status    string    `json:"status"`
	ScreenURL string    `json:"screenUrl"`
	ExpiresAt time.Time `json:"expiresAt"`
	Error     string    `json:"error,omitempty"`
}

func sessionResponse(item *browserSession) browserSessionResponse {
	return browserSessionResponse{
		AttemptID: item.AttemptID,
		Status:    item.Status,
		ScreenURL: item.ScreenURL,
		ExpiresAt: item.ExpiresAt,
		Error:     item.Error,
	}
}

type server struct {
	mu           sync.Mutex
	session      *browserSession
	profiles     string
	browserExec  string
	extraFlags   []string
	allocatePort func() (int, error)
	cmdFunc      func(ctx context.Context, name string, args ...string) *exec.Cmd
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--healthcheck" {
		if err := controllerHealth(); err != nil {
			slog.Error("ACB browser healthcheck failed", "error", err)
			os.Exit(1)
		}
		return
	}

	ctx, cancel := signalContext()
	defer cancel()
	controller := &server{profiles: "/tmp/acb-browser"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", controller.start)
	mux.HandleFunc("DELETE /sessions/{attemptID}", controller.cancel)
	mux.HandleFunc("GET /sessions/{attemptID}/status", controller.status)
	mux.HandleFunc("POST /sessions/{attemptID}/handoff", controller.handoff)
	mux.HandleFunc("POST /sessions/{attemptID}/complete", controller.complete)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Runtime-Role", "auth-browser")
		expectedRole := r.URL.Query().Get("role")
		if expectedRole == "" {
			expectedRole = r.Header.Get("X-Expected-Role")
		}
		if expectedRole != "" && !strings.EqualFold(expectedRole, "auth-browser") && !strings.EqualFold(expectedRole, "all-in-one") {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "error",
				"error":  fmt.Sprintf("role mismatch: expected %s, got auth-browser", expectedRole),
			})
			return
		}
		if err := desktopHealth(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "role": "auth-browser"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Runtime-Role", "auth-browser")
		expectedRole := r.URL.Query().Get("role")
		if expectedRole == "" {
			expectedRole = r.Header.Get("X-Expected-Role")
		}
		if expectedRole != "" && !strings.EqualFold(expectedRole, "auth-browser") && !strings.EqualFold(expectedRole, "all-in-one") {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "not_ready",
				"error":  fmt.Sprintf("role mismatch: expected %s, got auth-browser", expectedRole),
			})
			return
		}
		if err := desktopHealth(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "role": "auth-browser"})
	})

	httpServer := &http.Server{Addr: ":8181", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = httpServer.Shutdown(shutdownCtx)
		controller.stopCurrent()
	}()
	slog.Info("ACB browser controller listening", "address", ":8181")
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("ACB browser controller failed", "error", err)
		os.Exit(1)
	}
}

func allocateFreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate debugging port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, fmt.Errorf("close allocated port listener: %w", err)
	}
	return port, nil
}

func findDefaultBrowser() string {
	if bin := os.Getenv("BROWSER_BIN"); bin != "" {
		return bin
	}
	candidates := []string{
		"chromium",
		"google-chrome",
		"chrome",
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	}
	for _, c := range candidates {
		if path, err := exec.LookPath(c); err == nil {
			return path
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "chromium"
}

func removeProfileDir(dir string) {
	for range 10 {
		if err := os.RemoveAll(dir); err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = os.RemoveAll(dir)
}

func killProcessTree(proc *os.Process) {
	if proc == nil {
		return
	}
	_ = proc.Kill()
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprintf("%d", proc.Pid)).Run()
	}
}

func (s *server) reapSession(item *browserSession, timeout time.Duration) {
	if item == nil {
		return
	}
	item.cancel()
	if item.cmd != nil && item.cmd.Process != nil {
		killProcessTree(item.cmd.Process)
	}
	if item.done != nil {
		select {
		case <-item.done:
		case <-time.After(timeout):
			slog.Warn("timed out waiting for ACB browser process to exit", "attempt_id", item.AttemptID)
		}
	}
}

func (s *server) setStatus(id, newStatus, errMsg string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil || s.session.AttemptID != id {
		return false
	}
	if terminalStatus(s.session.Status) {
		return false
	}
	s.session.Status = newStatus
	if errMsg != "" {
		s.session.Error = errMsg
	}
	return true
}

func (s *server) start(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AttemptID string `json:"attemptId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "attemptId is required"})
		return
	}
	attemptID := strings.TrimSpace(input.AttemptID)
	if !isValidAttemptID(attemptID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "attemptId contains invalid characters"})
		return
	}
	profileDir, err := newProfileDirName()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot prepare browser profile"})
		return
	}

	s.mu.Lock()
	if s.session != nil {
		if terminalStatus(s.session.Status) || time.Now().UTC().After(s.session.ExpiresAt) {
			oldSession := s.session
			s.session = nil
			s.mu.Unlock()
			s.reapSession(oldSession, 5*time.Second)
			s.mu.Lock()
		} else if s.session.AttemptID == attemptID {
			resp := sessionResponse(s.session)
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, resp)
			return
		} else {
			s.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]string{"error": "an ACB browser session is already active"})
			return
		}
	}
	if err := os.MkdirAll(s.profiles, 0o700); err != nil {
		s.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot prepare browser profile"})
		return
	}

	portAlloc := s.allocatePort
	if portAlloc == nil {
		portAlloc = allocateFreePort
	}
	port, err := portAlloc()
	if err != nil {
		s.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot allocate debugging port"})
		return
	}

	expiresAt := time.Now().UTC().Add(sessionTTL)
	ctx, cancel := context.WithDeadline(context.Background(), expiresAt)
	item := &browserSession{
		AttemptID: attemptID,
		Status:    "STARTING",
		ScreenURL: "/",
		ExpiresAt: expiresAt,
		profile:   profileDir,
		cancel:    cancel,
		debugURL:  fmt.Sprintf("http://127.0.0.1:%d", port),
		done:      make(chan struct{}),
	}
	s.session = item
	s.mu.Unlock()

	ready := make(chan error, 1)
	go s.launch(ctx, item, port, ready)
	select {
	case err := <-ready:
		if err != nil {
			s.reapSession(item, 3*time.Second)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "ACB browser could not start"})
			return
		}
		s.mu.Lock()
		response := sessionResponse(item)
		s.mu.Unlock()
		writeJSON(w, http.StatusCreated, response)
	case <-r.Context().Done():
		s.setStatus(item.AttemptID, "CANCELLED", "startup aborted by client")
		item.cancel()
		s.reapSession(item, 3*time.Second)
	case <-time.After(startupLimit + time.Second):
		s.setStatus(item.AttemptID, "FAILED", "browser startup timed out")
		item.cancel()
		s.reapSession(item, 3*time.Second)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "ACB browser startup timed out"})
	}
}

func (s *server) launch(ctx context.Context, item *browserSession, port int, ready chan<- error) {
	profile := filepath.Join(s.profiles, item.profile)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		s.failStartup(item, ready, "cannot prepare Chromium profile", err)
		item.exitOnce.Do(func() { close(item.done) })
		return
	}

	browserBin := s.browserExec
	if browserBin == "" {
		browserBin = findDefaultBrowser()
	}

	args := []string{
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--disable-default-apps",
		"--disable-sync",
		"--disable-extensions",
		"--disable-background-networking",
		"--disable-dev-shm-usage",
		"--disable-setuid-sandbox",
		"--window-size=1280,900",
		"--remote-debugging-address=127.0.0.1",
		fmt.Sprintf("--remote-debugging-port=%d", port),
	}
	if len(s.extraFlags) > 0 {
		args = append(args, s.extraFlags...)
	}
	args = append(args, acbLoginURL())

	cmdBuilder := s.cmdFunc
	if cmdBuilder == nil {
		cmdBuilder = exec.CommandContext
	}
	cmd := cmdBuilder(ctx, browserBin, args...)
	if len(cmd.Env) == 0 {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "DISPLAY=:99", "HOME=/tmp")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = 3 * time.Second
	item.cmd = cmd

	if err := cmd.Start(); err != nil {
		s.failStartup(item, ready, "Chromium failed to launch", err)
		removeProfileDir(profile)
		item.exitOnce.Do(func() { close(item.done) })
		return
	}

	go func() {
		err := cmd.Wait()
		item.exitErr = err
		removeProfileDir(profile)
		item.exitOnce.Do(func() { close(item.done) })
	}()

	startupCtx, startupCancel := context.WithTimeout(ctx, startupLimit)
	err := waitBrowserReady(startupCtx, item.debugURL, item.done, item)
	startupCancel()
	if err != nil {
		item.cancel()
		s.failStartup(item, ready, "Chromium did not become ready", err)
		return
	}

	s.mu.Lock()
	if s.session == item && !terminalStatus(item.Status) {
		item.Status = "AWAITING_USER_LOGIN"
		item.Error = ""
	}
	s.mu.Unlock()
	ready <- nil
	go s.observeLogin(ctx, item.AttemptID, item.debugURL, item.done)

	<-item.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != item || terminalStatus(item.Status) || item.Status == "VERIFIED" {
		return
	}
	if time.Now().UTC().After(item.ExpiresAt) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		item.Status = "EXPIRED"
		item.Error = "ACB login session expired"
		slog.Info("ACB Chromium session expired", "attempt_id", item.AttemptID)
		return
	}
	item.Status = "FAILED"
	item.Error = "Chromium exited before login completed"
	slog.Warn("ACB Chromium exited", "attempt_id", item.AttemptID, "error", item.exitErr)
}

func (s *server) failStartup(item *browserSession, ready chan<- error, message string, err error) {
	s.setStatus(item.AttemptID, "FAILED", message)
	slog.Warn(message, "attempt_id", item.AttemptID, "error", err)
	ready <- err
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("attemptID")
	s.mu.Lock()
	if s.session == nil || s.session.AttemptID != id {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	item := s.session
	if !terminalStatus(item.Status) {
		item.Status = "CANCELLED"
	}
	s.mu.Unlock()

	s.reapSession(item, 5*time.Second)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("attemptID")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil || s.session.AttemptID != id {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	if time.Now().UTC().After(s.session.ExpiresAt) && !terminalStatus(s.session.Status) {
		s.session.Status = "EXPIRED"
		s.session.Error = "ACB login session expired"
		s.session.cancel()
	}
	writeJSON(w, http.StatusOK, sessionResponse(s.session))
}

func (s *server) handoff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("attemptID")
	s.mu.Lock()
	if s.session == nil || s.session.AttemptID != id || !s.session.verified || s.session.handoff == "" {
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "ACB login has not been verified"})
		return
	}
	handoff := s.session.handoff
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]string{"session": handoff})
}

func (s *server) complete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("attemptID")
	s.mu.Lock()
	if s.session == nil || s.session.AttemptID != id || !s.session.verified {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "ACB browser session not found"})
		return
	}
	item := s.session
	item.handoff = ""
	item.Status = "COMPLETED"
	s.mu.Unlock()

	s.reapSession(item, 5*time.Second)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) observeLogin(ctx context.Context, id, debugURL string, done <-chan struct{}) {
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()

	backoff := 250 * time.Millisecond
	const maxBackoff = 2 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		default:
		}

		s.mu.Lock()
		if s.session == nil || s.session.AttemptID != id || (s.session.Status != "AWAITING_USER_LOGIN" && s.session.Status != "VERIFIED") {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		connected, verified := s.runObserverCycle(ctx, id, debugURL, done, ticker)
		if verified {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-time.After(1 * time.Second):
			}
			continue
		}
		if !connected {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-time.After(backoff):
				if backoff < maxBackoff {
					backoff *= 2
				}
			}
		} else {
			backoff = 250 * time.Millisecond
		}
	}
}

func (s *server) runObserverCycle(ctx context.Context, id, debugURL string, done <-chan struct{}, ticker *time.Ticker) (connected bool, verified bool) {
	allocatorCtx, allocatorCancel := chromedp.NewRemoteAllocator(ctx, debugURL)
	defer allocatorCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocatorCtx)
	defer browserCancel()

	browser, err := chromedp.FromContext(browserCtx).Allocator.Allocate(browserCtx)
	if err != nil {
		slog.Warn("attach ACB Chromium observer retry", "attempt_id", id, "error", err)
		return false, false
	}
	executor := cdp.WithExecutor(browserCtx, browser)

	var (
		lastReason  string
		lastLogTime time.Time
	)

	for {
		select {
		case <-ctx.Done():
			return true, false
		case <-done:
			return true, false
		case <-browser.LostConnection:
			slog.Warn("ACB Chromium observer lost connection, reconnecting", "attempt_id", id)
			return false, false
		case <-ticker.C:
		}

		s.mu.Lock()
		if s.session == nil || s.session.AttemptID != id || (s.session.Status != "AWAITING_USER_LOGIN" && s.session.Status != "VERIFIED") {
			s.mu.Unlock()
			return true, false
		}
		s.mu.Unlock()

		checkCtx, cancel := context.WithTimeout(executor, 5*time.Second)
		currentURL, signals, cookies, form, reason, err := browserLoginState(checkCtx, browserCtx)
		cancel()
		if err != nil {
			continue
		}
		if !authenticatedACB(currentURL, signals, cookies) || !validHistoryForm(form) {
			if reason != "" && (reason != lastReason || time.Since(lastLogTime) >= 10*time.Second) {
				slog.Info("ACB observer waiting", "attempt_id", id, "reason", reason)
				lastReason = reason
				lastLogTime = time.Now()
			}
			continue
		}
		handoff, err := encodeHandoff(currentURL, cookies, form)
		if err != nil {
			slog.Warn("encode ACB browser handoff", "attempt_id", id, "error", err)
			continue
		}
		s.mu.Lock()
		if s.session != nil && s.session.AttemptID == id && (s.session.Status == "AWAITING_USER_LOGIN" || s.session.Status == "VERIFIED") {
			s.session.handoff = handoff
			s.session.verified = true
			s.session.Status = "VERIFIED"
			slog.Info("ACB login verified via page DOM signals", "attempt_id", id)
		}
		s.mu.Unlock()
		return true, true
	}
}

type domSignals struct {
	HasLogout           bool `json:"hasLogout"`
	HasAccountOverview  bool `json:"hasAccountOverview"`
	HasWelcome          bool `json:"hasWelcome"`
	HasAccountProcessor bool `json:"hasAccountProcessor"`
	HasProcessorState   bool `json:"hasProcessorState"`
	VisiblePassword     bool `json:"visiblePassword"`
	VisibleCaptcha      bool `json:"visibleCaptcha"`
	VisibleOTP          bool `json:"visibleOTP"`
	VisibleLogin        bool `json:"visibleLogin"`
}

func (s domSignals) positiveCount() int {
	count := 0
	if s.HasLogout {
		count++
	}
	if s.HasAccountOverview {
		count++
	}
	if s.HasWelcome {
		count++
	}
	if s.HasAccountProcessor {
		count++
	}
	if s.HasProcessorState {
		count++
	}
	return count
}

func (s domSignals) isAuthenticated() bool {
	if s.VisiblePassword || s.VisibleCaptcha || s.VisibleOTP || s.VisibleLogin {
		return false
	}
	if !s.HasLogout {
		return false
	}
	if !s.HasAccountOverview && !s.HasWelcome && !s.HasAccountProcessor {
		return false
	}
	if s.positiveCount() < 2 {
		return false
	}
	return true
}

const acbDOMCheckScript = `(() => {
	function isVisible(el) {
		if (!el) return false;
		try {
			const style = window.getComputedStyle(el);
			if (!style) return false;
			if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
			if (el.offsetParent === null && style.position !== 'fixed') return false;
			const rect = el.getBoundingClientRect();
			return rect.width > 0 && rect.height > 0;
		} catch (e) {
			return false;
		}
	}

	let hasLogout = false;
	try {
		const logoutElements = document.querySelectorAll('a, button, input[type="button"], input[type="submit"], [role="button"], span, div');
		for (let i = 0; i < logoutElements.length; i++) {
			const el = logoutElements[i];
			if (el.children.length > 2) continue;
			const text = (el.textContent || '').trim().toLowerCase();
			const href = (el.getAttribute('href') || '').toLowerCase();
			const onclick = (el.getAttribute('onclick') || '').toLowerCase();
			const val = (el.value || '').toLowerCase();
			const id = (el.id || '').toLowerCase();
			if (text.includes('đăng xuất') || text.includes('dang xuat') ||
				href.includes('logout') || onclick.includes('logout') ||
				val.includes('đăng xuất') || val.includes('dang xuat') ||
				id.includes('logout') || href.includes('ibklogoutop') || onclick.includes('ibklogoutop')) {
				hasLogout = true;
				break;
			}
		}
		if (!hasLogout) {
			hasLogout = document.querySelector('input[value*="ibkLogoutOp" i], [name="dse_operationName"][value*="ibkLogoutOp" i]') !== null;
		}
	} catch (e) {}

	let hasAccountOverview = false;
	try {
		const overviewElements = document.querySelectorAll('a, button, [role="tab"], h1, h2, h3, h4, span, td, th, div');
		for (let i = 0; i < overviewElements.length; i++) {
			const el = overviewElements[i];
			if (el.children.length > 2) continue;
			const text = (el.textContent || '').trim().toLowerCase();
			const href = (el.getAttribute('href') || '').toLowerCase();
			const id = (el.id || '').toLowerCase();
			if (text.includes('thông tin tài khoản') || text.includes('thong tin tai khoan') ||
				text.includes('tổng quan') || text.includes('tong quan') ||
				text.includes('tài khoản thanh toán') || text.includes('danh sách tài khoản') ||
				href.includes('accountsummary') || href.includes('accountoverview') ||
				id.includes('accountsummary') || id.includes('accountoverview')) {
				hasAccountOverview = true;
				break;
			}
		}
		if (!hasAccountOverview) {
			hasAccountOverview = document.querySelector('[name="AccountNbr" i], [id*="AccountNbr" i], select[name*="account" i]') !== null;
		}
	} catch (e) {}

	let hasWelcome = false;
	try {
		const welcomeElements = document.querySelectorAll('h1, h2, h3, h4, span, p, div, [class*="welcome" i], [id*="welcome" i], [class*="user" i], [id*="user" i]');
		for (let i = 0; i < welcomeElements.length; i++) {
			const el = welcomeElements[i];
			if (el.children.length > 2) continue;
			const text = (el.textContent || '').trim().toLowerCase();
			if (text.startsWith('xin chào') || text.startsWith('xin chao') ||
				text.startsWith('chào mừng') || text.startsWith('chao mung') ||
				text.startsWith('chào,') || text.startsWith('chao,') ||
				text.startsWith('welcome') ||
				(el.className && typeof el.className === 'string' && el.className.toLowerCase().includes('welcome')) ||
				(el.id && el.id.toLowerCase().includes('welcome'))) {
				hasWelcome = true;
				break;
			}
		}
	} catch (e) {}

	let hasAccountProcessor = false;
	try {
		const procElements = document.querySelectorAll('input, form, a, [name="dse_operationName"]');
		for (let i = 0; i < procElements.length; i++) {
			const el = procElements[i];
			const val = (el.value || '').toLowerCase();
			const action = (el.getAttribute('action') || '').toLowerCase();
			const href = (el.getAttribute('href') || '').toLowerCase();
			if (val.includes('ibkacctdetailproc') || action.includes('ibkacctdetailproc') || href.includes('ibkacctdetailproc')) {
				hasAccountProcessor = true;
				break;
			}
		}
	} catch (e) {}

	let hasProcessorState = false;
	try {
		hasProcessorState = document.querySelector('input[name*="processorState" i], [name="dse_processorState"], [name="dse_processorstate"]') !== null;
	} catch (e) {}

	let visiblePassword = false;
	try {
		const pwInputs = document.querySelectorAll('input[type="password"]');
		for (let i = 0; i < pwInputs.length; i++) {
			if (isVisible(pwInputs[i])) {
				visiblePassword = true;
				break;
			}
		}
	} catch (e) {}

	let visibleCaptcha = false;
	try {
		const captchaElements = document.querySelectorAll('input[name*="captcha" i], input[id*="captcha" i], img[src*="captcha" i], [id*="captcha" i], [class*="captcha" i]');
		for (let i = 0; i < captchaElements.length; i++) {
			if (isVisible(captchaElements[i])) {
				visibleCaptcha = true;
				break;
			}
		}
	} catch (e) {}

	let visibleOTP = false;
	try {
		const otpElements = document.querySelectorAll('input[name*="otp" i], input[id*="otp" i], input[name*="safekey" i], input[id*="safekey" i], input[name*="authcode" i], input[id*="authcode" i]');
		for (let i = 0; i < otpElements.length; i++) {
			if (isVisible(otpElements[i])) {
				visibleOTP = true;
				break;
			}
		}
	} catch (e) {}

	let visibleLogin = false;
	try {
		const loginInputs = document.querySelectorAll('input[name="username" i], input[name="user" i], input[id="username" i], input[id="user" i], button[id*="login" i], input[value*="đăng nhập" i], input[value*="dang nhap" i]');
		for (let i = 0; i < loginInputs.length; i++) {
			if (isVisible(loginInputs[i])) {
				visibleLogin = true;
				break;
			}
		}
		if (!visibleLogin) {
			visibleLogin = document.querySelector('input[name="dse_operationName"][value*="obkloginop" i]') !== null;
		}
	} catch (e) {}

	return {
		hasLogout: !!hasLogout,
		hasAccountOverview: !!hasAccountOverview,
		hasWelcome: !!hasWelcome,
		hasAccountProcessor: !!hasAccountProcessor,
		hasProcessorState: !!hasProcessorState,
		visiblePassword: !!visiblePassword,
		visibleCaptcha: !!visibleCaptcha,
		visibleOTP: !!visibleOTP,
		visibleLogin: !!visibleLogin
	};
})()`

const defaultACBHost = "online.acb.com.vn"

func acbExpectedHost() string {
	if loginURL, err := url.Parse(acbLoginURL()); err == nil && loginURL.Hostname() != "" {
		return loginURL.Hostname()
	}
	return defaultACBHost
}

func isACBCookieDomain(domain string) bool {
	d := strings.ToLower(strings.TrimPrefix(domain, "."))
	if d == "" {
		return false
	}
	expectedHost := acbExpectedHost()
	if expectedHost != "" && (d == expectedHost || strings.HasSuffix(d, "."+expectedHost)) {
		return true
	}
	if d == "online.acb.com.vn" || d == "acb.com.vn" {
		return true
	}
	if strings.HasSuffix(d, ".acb.com.vn") {
		return true
	}
	return false
}

func isValidACBCookie(cookie *network.Cookie) bool {
	if cookie == nil {
		return false
	}
	name := strings.TrimSpace(cookie.Name)
	val := strings.TrimSpace(cookie.Value)
	if name == "" || val == "" {
		return false
	}
	return isACBCookieDomain(cookie.Domain)
}

func filterACBCookies(cookies []*network.Cookie) []*network.Cookie {
	filtered := make([]*network.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if isValidACBCookie(cookie) {
			filtered = append(filtered, cookie)
		}
	}
	return filtered
}

func isMatchingACBPageURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	expectedHost := acbExpectedHost()
	expectedScheme := "https"
	expectedPathPrefix := "/acbib"
	if loginURL, err := url.Parse(acbLoginURL()); err == nil {
		if loginURL.Scheme != "" {
			expectedScheme = loginURL.Scheme
		}
		if loginURL.Path != "" && loginURL.Path != "/" {
			expectedPathPrefix = loginURL.Path
		}
	}
	if !strings.EqualFold(parsed.Scheme, expectedScheme) {
		return false
	}
	if !strings.EqualFold(parsed.Hostname(), expectedHost) {
		return false
	}
	lowerPath := strings.ToLower(parsed.Path)
	if lowerPath == "" || lowerPath == "/" || lowerPath == "/login" {
		return false
	}
	if !strings.HasPrefix(lowerPath, "/acbib") && !strings.HasPrefix(lowerPath, strings.ToLower(expectedPathPrefix)) {
		return false
	}
	lowerQuery := strings.ToLower(parsed.RawQuery)
	if strings.Contains(lowerQuery, "obkloginop") {
		return false
	}
	return true
}

func evaluateDOMSignals(ctx context.Context, browserCtx context.Context, targetID target.ID) (domSignals, error) {
	if err := ctx.Err(); err != nil {
		return domSignals{}, err
	}
	tabCtx, tabCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(targetID))
	defer func() {
		c := chromedp.FromContext(tabCtx)
		if c != nil && c.Target != nil {
			c.Target.TargetID = ""
		}
		tabCancel()
	}()

	var signals domSignals
	evalCtx, evalCancel := context.WithTimeout(tabCtx, 3*time.Second)
	defer evalCancel()

	if err := chromedp.Run(evalCtx, chromedp.Evaluate(acbDOMCheckScript, &signals)); err != nil {
		return domSignals{}, err
	}
	return signals, nil
}

func classifyWaitingReason(signals domSignals, cookies []*network.Cookie) string {
	if len(cookies) == 0 {
		return "waiting for valid ACB session cookies"
	}
	if signals.VisiblePassword || signals.VisibleLogin {
		return "login form currently visible"
	}
	if signals.VisibleOTP {
		return "OTP challenge currently visible"
	}
	if signals.VisibleCaptcha {
		return "CAPTCHA challenge currently visible"
	}
	if !signals.HasLogout {
		return "waiting for logout element in page DOM"
	}
	if !signals.HasAccountOverview && !signals.HasWelcome && !signals.HasAccountProcessor {
		return "waiting for authenticated account or welcome DOM signals"
	}
	if signals.positiveCount() < 2 {
		return "waiting for multiple positive post-login DOM signals"
	}
	return "waiting for user authentication"
}

func browserLoginState(ctx context.Context, browserCtx context.Context) (string, domSignals, []*network.Cookie, browserFormState, string, error) {
	targets, err := target.GetTargets().Do(ctx)
	if err != nil {
		return "", domSignals{}, nil, browserFormState{}, "failed to get browser targets", err
	}
	var matchingTargets []*target.Info
	for _, info := range targets {
		if info.Type == "page" && isMatchingACBPageURL(info.URL) {
			matchingTargets = append(matchingTargets, info)
		}
	}
	if len(matchingTargets) == 0 {
		return "", domSignals{}, nil, browserFormState{}, "no matching ACB page target found", nil
	}

	allCookies, err := storage.GetCookies().Do(ctx)
	if err != nil {
		return "", domSignals{}, nil, browserFormState{}, "failed to read browser cookies", err
	}
	acbCookies := filterACBCookies(allCookies)

	var lastURL string
	var lastSignals domSignals
	for _, info := range matchingTargets {
		lastURL = info.URL
		signals, err := evaluateDOMSignals(ctx, browserCtx, info.TargetID)
		if err != nil {
			continue
		}
		lastSignals = signals
		if authenticatedACB(info.URL, signals, acbCookies) {
			form, _ := evaluateHistoryForm(ctx, browserCtx, info.TargetID)
			if validHistoryForm(form) {
				slog.Info("ACB authenticated form state captured", "field_count", len(form.Fields), "has_session_id", form.Fields["dse_sessionId"] != "", "has_processor_id", form.Fields["dse_processorId"] != "", "has_processor_state", form.Fields["dse_processorState"] != "", "has_account", form.Fields["AccountNbr"] != "")
				return info.URL, signals, acbCookies, form, "", nil
			}
			lastSignals = signals
		}
	}

	reason := classifyWaitingReason(lastSignals, acbCookies)
	if lastSignals.isAuthenticated() {
		reason = "waiting for authenticated ACB form state"
	}
	return lastURL, lastSignals, acbCookies, browserFormState{}, reason, nil
}

func validHistoryForm(form browserFormState) bool {
	if form.Action == "" || form.Fields["dse_sessionId"] == "" || form.Fields["dse_processorState"] == "" {
		return false
	}
	if form.Fields["dse_operationName"] == "ibkacctSumProc" {
		return false
	}
	if form.Fields["dse_operationName"] != "" && form.Fields["dse_operationName"] != "ibkacctDetailProc" {
		return false
	}
	if form.Fields["dse_operationName"] == "ibkacctDetailProc" && form.Fields["AccountNbr"] == "" {
		return false
	}
	return true
}

func evaluateHistoryForm(ctx context.Context, browserCtx context.Context, targetID target.ID) (browserFormState, error) {
	tabCtx, tabCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(targetID))
	defer func() {
		c := chromedp.FromContext(tabCtx)
		if c != nil && c.Target != nil {
			c.Target.TargetID = ""
		}
		tabCancel()
	}()
	var form browserFormState
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(acbHistoryFormScript, &form)); err != nil {
		return browserFormState{}, err
	}
	return form, nil
}

const acbHistoryFormScript = `(() => {
	const allowed = new Set([
		'dse_applicationId', 'dse_operationName', 'dse_pageId',
		'dse_processorState', 'dse_errorPage', 'dse_nextEventName',
		'dse_sessionId', 'dse_processorId', 'dse_processorIdForGenMenu', 'AccountNbr', 'virtualAccount',
		'storeName', 'CheckRef', 'EdtRef', 'CheckDoiUng',
		'activeDatetimeYN', 'FromDate', 'ToDate'
	]);

	function extractFormFields(form) {
		const fields = {};
		for (const element of form.elements) {
			if (!element.name || !allowed.has(element.name) || element.disabled) continue;
			const type = (element.type || '').toLowerCase();
			if ((type === 'checkbox' || type === 'radio') && !element.checked) continue;
			fields[element.name] = element.value || '';
		}
		return fields;
	}

	let bestForm = null;
	let bestScore = -1;

	for (const form of document.forms) {
		const fields = extractFormFields(form);
		if (!fields.dse_sessionId && !fields.dse_processorState) continue;

		let score = 0;
		if (fields.dse_operationName === 'ibkacctDetailProc') score += 10;
		if (fields.AccountNbr) score += 5;
		if (fields.dse_processorId) score += 3;
		if (fields.dse_sessionId) score += 2;
		if (fields.dse_processorState) score += 1;

		if (score > bestScore) {
			bestScore = score;
			bestForm = { action: form.action || location.href, fields };
		}
	}

	if (bestForm) {
		// If AccountNbr was not in the form itself, search the DOM for it
		if (!bestForm.fields.AccountNbr) {
			const acctEl = document.querySelector('[name="AccountNbr" i], select[name*="account" i] option:checked, [id*="AccountNbr" i]');
			if (acctEl) {
				const val = acctEl.value || acctEl.textContent || '';
				const clean = val.trim();
				if (clean && clean.length >= 6) {
					bestForm.fields.AccountNbr = clean;
				}
			}
		}
		// If we are still on the overview/welcome page without the detail processor, try clicking the account
		if (bestForm.fields.dse_operationName !== 'ibkacctDetailProc' || !bestForm.fields.AccountNbr) {
			try {
				const link = document.querySelector('a[href*="ibkacctdetailproc" i], a[href*="AccountNbr" i], a.acc_bold, [onclick*="ibkacctdetailproc" i], [class*="account-card" i], [class*="acct-card" i], [class*="account-item" i]');
				if (link && typeof link.click === 'function') {
					link.click();
				}
			} catch (e) {}
		}
		return bestForm;
	}

	try {
		const link = document.querySelector('a[href*="ibkacctdetailproc" i], a[href*="AccountNbr" i], a.acc_bold');
		if (link && typeof link.click === 'function') {
			link.click();
		}
	} catch (e) {}

	return { action: '', fields: {} };
})()`

func encodeHandoff(currentURL string, cookies []*network.Cookie, form browserFormState) (string, error) {
	if !isMatchingACBPageURL(currentURL) {
		return "", errors.New("invalid authenticated ACB URL")
	}
	filtered := filterACBCookies(cookies)
	if len(filtered) == 0 {
		return "", errors.New("no valid ACB cookies to hand off")
	}
	serializable := make([]authbrowser.Cookie, 0, len(filtered))
	for _, cookie := range filtered {
		// CDP represents session cookies with a non-positive expiry. Keep its
		// zero-value representation so net/http's cookie jar does not discard it.
		expires := time.Time{}
		if cookie.Expires > 0 {
			expires = time.Unix(int64(cookie.Expires), 0).UTC()
		}
		serializable = append(serializable, authbrowser.Cookie{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path, Expires: expires, Secure: cookie.Secure, HTTPOnly: cookie.HTTPOnly})
	}
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return authbrowser.EncodeHandoff(authbrowser.Handoff{Version: 1, URL: currentURL, Action: form.Action, Fields: form.Fields, Cookies: serializable}, nonce)
}

func waitBrowserReady(ctx context.Context, debugURL string, done <-chan struct{}, item *browserSession) error {
	client := &http.Client{Timeout: time.Second}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			var exitErr error
			if item != nil {
				exitErr = item.exitErr
			}
			if exitErr == nil {
				return errors.New("Chromium exited during startup")
			}
			return fmt.Errorf("Chromium exited during startup: %w", exitErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, debugURL+"/json/version", nil)
			if err != nil {
				continue
			}
			response, err := client.Do(request)
			if err != nil {
				continue
			}
			var version struct {
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 32<<10)).Decode(&version)
			response.Body.Close()
			if response.StatusCode != http.StatusOK || decodeErr != nil || version.WebSocketDebuggerURL == "" {
				continue
			}
			if err := waitACBTargetStable(ctx, client, debugURL, done); err != nil {
				return err
			}
			return nil
		}
	}
}

func waitACBTargetStable(ctx context.Context, client *http.Client, debugURL string, done <-chan struct{}) error {
	const stableFor = 2 * time.Second
	stableSince := time.Time{}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return errors.New("Chromium exited before the ACB page became stable")
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, debugURL+"/json/list", nil)
		if err != nil {
			stableSince = time.Time{}
			continue
		}
		response, err := client.Do(request)
		if err != nil {
			stableSince = time.Time{}
			continue
		}
		var targets []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 128<<10)).Decode(&targets)
		response.Body.Close()
		found := false
		if response.StatusCode == http.StatusOK && decodeErr == nil {
			loginLocation, _ := url.Parse(acbLoginURL())
			for _, targetInfo := range targets {
				targetLocation, parseErr := url.Parse(targetInfo.URL)
				if targetInfo.Type == "page" && parseErr == nil && loginLocation != nil &&
					strings.EqualFold(targetLocation.Hostname(), loginLocation.Hostname()) {
					found = true
					break
				}
			}
		}
		if !found {
			stableSince = time.Time{}
			continue
		}
		if stableSince.IsZero() {
			stableSince = time.Now()
			continue
		}
		if time.Since(stableSince) >= stableFor {
			return nil
		}
	}
}

func desktopHealth() error {
	if _, err := os.Stat("/tmp/.X11-unix/X99"); err != nil {
		return errors.New("X display is unavailable")
	}
	for _, address := range []string{"127.0.0.1:5900", "127.0.0.1:6080"} {
		connection, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			return fmt.Errorf("desktop service %s is unavailable", address)
		}
		connection.Close()
	}
	return nil
}

func controllerHealth() error {
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://127.0.0.1:8181/healthz")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("controller returned HTTP %d", response.StatusCode)
	}
	return nil
}

func terminalStatus(status string) bool {
	switch status {
	case "FAILED", "EXPIRED", "CANCELLED", "COMPLETED":
		return true
	default:
		return false
	}
}

func authenticatedACB(location string, signals domSignals, cookies []*network.Cookie) bool {
	if !isMatchingACBPageURL(location) {
		return false
	}
	if !signals.isAuthenticated() {
		return false
	}
	filtered := filterACBCookies(cookies)
	return len(filtered) > 0
}

func (s *server) stopCurrent() {
	s.mu.Lock()
	item := s.session
	s.session = nil
	s.mu.Unlock()
	if item != nil {
		s.reapSession(item, 5*time.Second)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
