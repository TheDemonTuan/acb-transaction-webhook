package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestAdminLifecycle(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner"}, store).Handler()
	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	h.ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var token struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&token)
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://example.test"+path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://example.test")
		r.Header.Set("X-CSRF-Token", token.Token)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post("/api/v1/connection/configure", `{"accountMasked":"***1234"}`); w.Code != http.StatusCreated {
		t.Fatalf("configure %d %s", w.Code, w.Body.String())
	}
	if w := post("/api/v1/webhooks", `{"name":"receiver","url":"https://events.example.com/bank"}`); w.Code != http.StatusCreated {
		t.Fatalf("endpoint %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/webhooks", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("get endpoints %d", w.Code)
	}
}

func TestGatewayDoesNotServeFrontendRoutes(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway-no-spa.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	h := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner"}, store).Handler()
	for _, requestPath := range []string{"/", "/admin/activity", "/transactions/example"} {
		t.Run(requestPath, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://example.test"+requestPath, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("gateway must not serve frontend route %s; got %d", requestPath, w.Code)
			}
		})
	}
}

func TestBrowserScreenCSP(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "csp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAuthAttempt(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<!doctype html><title>noVNC</title>"))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		AuthBrowserVNCURL:  upstream.URL,
	}
	h := New(cfg, store).Handler()

	parseCSPDirectives := func(csp string) map[string][]string {
		dirs := make(map[string][]string)
		for _, part := range strings.Split(csp, ";") {
			fields := strings.Fields(strings.TrimSpace(part))
			if len(fields) == 0 {
				continue
			}
			dirs[fields[0]] = fields[1:]
		}
		return dirs
	}

	contains := func(slice []string, val string) bool {
		for _, s := range slice {
			if s == val {
				return true
			}
		}
		return false
	}

	t.Run("vnc.html receives img-src data CSP", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/"+attempt.ID+"/screen/vnc.html", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
		}
		cspHeaders := w.Result().Header.Values("Content-Security-Policy")
		if len(cspHeaders) != 1 {
			t.Fatalf("expected exactly 1 Content-Security-Policy header, got %d: %v", len(cspHeaders), cspHeaders)
		}
		dirs := parseCSPDirectives(cspHeaders[0])
		imgSrc, ok := dirs["img-src"]
		if !ok {
			t.Fatalf("missing img-src directive: %s", cspHeaders[0])
		}
		if len(imgSrc) != 2 || !contains(imgSrc, "'self'") || !contains(imgSrc, "data:") {
			t.Fatalf("expected img-src ['self' data:], got %v", imgSrc)
		}
		fontSrc, ok := dirs["font-src"]
		if !ok {
			t.Fatalf("missing font-src directive: %s", cspHeaders[0])
		}
		if len(fontSrc) != 2 || !contains(fontSrc, "'self'") || !contains(fontSrc, "data:") {
			t.Fatalf("expected font-src ['self' data:], got %v", fontSrc)
		}
		if val, ok := dirs["default-src"]; !ok || len(val) != 1 || val[0] != "'self'" {
			t.Fatalf("default-src mismatch: %v", dirs["default-src"])
		}
		if val, ok := dirs["base-uri"]; !ok || len(val) != 1 || val[0] != "'none'" {
			t.Fatalf("base-uri mismatch: %v", dirs["base-uri"])
		}
		if val, ok := dirs["frame-ancestors"]; !ok || len(val) != 1 || val[0] != "'self'" {
			t.Fatalf("frame-ancestors mismatch: %v", dirs["frame-ancestors"])
		}
		if val, ok := dirs["object-src"]; !ok || len(val) != 1 || val[0] != "'none'" {
			t.Fatalf("object-src mismatch: %v", dirs["object-src"])
		}
		if val, ok := dirs["connect-src"]; !ok || !contains(val, "'self'") || !contains(val, "ws:") || !contains(val, "wss:") {
			t.Fatalf("connect-src mismatch: %v", dirs["connect-src"])
		}
		scriptSrc, ok := dirs["script-src"]
		if !ok || !contains(scriptSrc, "'self'") || !contains(scriptSrc, "'unsafe-inline'") {
			t.Fatalf("expected script-src with 'self' and 'unsafe-inline', got %v", scriptSrc)
		}
		styleSrc, ok := dirs["style-src"]
		if !ok || !contains(styleSrc, "'self'") || !contains(styleSrc, "'unsafe-inline'") {
			t.Fatalf("expected style-src with 'self' and 'unsafe-inline', got %v", styleSrc)
		}
	})

	t.Run("healthz does not allow data image", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/healthz", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		cspHeaders := w.Result().Header.Values("Content-Security-Policy")
		if len(cspHeaders) != 1 {
			t.Fatalf("expected 1 CSP header, got %d: %v", len(cspHeaders), cspHeaders)
		}
		if strings.Contains(cspHeaders[0], "data:") {
			t.Fatalf("healthz must not contain data:, got %s", cspHeaders[0])
		}
		dirs := parseCSPDirectives(cspHeaders[0])
		if _, ok := dirs["img-src"]; ok {
			t.Fatalf("healthz should not have img-src directive, got %s", cspHeaders[0])
		}
	})

	t.Run("nonexistent attempt returns 404 without data CSP", func(t *testing.T) {
		callsBefore := upstreamCalls.Load()
		r := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/nonexistent/screen/vnc.html", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
		if upstreamCalls.Load() != callsBefore {
			t.Fatalf("upstream called for nonexistent attempt")
		}
		cspHeaders := w.Result().Header.Values("Content-Security-Policy")
		if len(cspHeaders) != 1 {
			t.Fatalf("expected 1 CSP header, got %d: %v", len(cspHeaders), cspHeaders)
		}
		if strings.Contains(cspHeaders[0], "data:") {
			t.Fatalf("404 response must not contain data:, got %s", cspHeaders[0])
		}
	})
}

func TestBrowserScreenProxyEndToEnd(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "proxy_e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	ownerAttempt, err := store.StartAuthAttempt(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	otherStore, err := storage.Open(ctx, filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer otherStore.Close()
	if _, err := otherStore.ConfigureConnection(ctx, "***5678"); err != nil {
		t.Fatal(err)
	}
	otherAttempt, err := otherStore.StartAuthAttempt(ctx, "different-owner@example.com", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	type reqRecord struct {
		Path     string
		RawQuery string
		Upgrade  string
	}
	var (
		mu       sync.Mutex
		lastReq  reqRecord
		reqCount int
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqCount++
		lastReq = reqRecord{
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
			Upgrade:  r.Header.Get("Upgrade"),
		}
		mu.Unlock()

		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijacking not supported", http.StatusInternalServerError)
				return
			}
			conn, bufrw, err := hj.Hijack()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer conn.Close()

			res := "HTTP/1.1 101 Switching Protocols\r\n" +
				"Upgrade: websocket\r\n" +
				"Connection: Upgrade\r\n" +
				"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n"
			if _, err := bufrw.WriteString(res); err != nil {
				return
			}
			if err := bufrw.Flush(); err != nil {
				return
			}

			// Bidirectional echo test
			line, err := bufrw.ReadString('\n')
			if err == nil {
				_, _ = bufrw.WriteString("echo:" + line)
				_ = bufrw.Flush()
			}
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "upstream path=%s query=%s", r.URL.Path, r.URL.RawQuery)
	}))
	defer upstream.Close()

	cfg := config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		AuthBrowserVNCURL:  upstream.URL,
	}
	gatewayServer := httptest.NewServer(New(cfg, store).Handler())
	defer gatewayServer.Close()

	t.Run("WebSocket Upgrade 101 and bidirectional forwarding", func(t *testing.T) {
		conn, err := net.Dial("tcp", gatewayServer.Listener.Addr().String())
		if err != nil {
			t.Fatalf("failed to dial gateway: %v", err)
		}
		defer conn.Close()

		reqPath := fmt.Sprintf("/api/v1/connection/auth/%s/screen/websockify?token=vnc-sec-token&autoconnect=true", ownerAttempt.ID)
		rawReq := fmt.Sprintf(
			"GET %s HTTP/1.1\r\n"+
				"Host: %s\r\n"+
				"Upgrade: websocket\r\n"+
				"Connection: Upgrade\r\n"+
				"Sec-WebSocket-Key: SGVsbG8sIHdvcmxkIQ==\r\n"+
				"Sec-WebSocket-Version: 13\r\n\r\n",
			reqPath, gatewayServer.Listener.Addr().String(),
		)

		if _, err := conn.Write([]byte(rawReq)); err != nil {
			t.Fatalf("failed to write WS handshake request: %v", err)
		}

		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("failed to read WS handshake response: %v", err)
		}
		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("expected status 101 Switching Protocols, got %d", resp.StatusCode)
		}
		if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
			t.Fatalf("expected Upgrade: websocket, got %q", resp.Header.Get("Upgrade"))
		}
		if !strings.Contains(strings.ToLower(resp.Header.Get("Connection")), "upgrade") {
			t.Fatalf("expected Connection: Upgrade, got %q", resp.Header.Get("Connection"))
		}

		// Verify upstream observed the correct forwarded path and query
		mu.Lock()
		observed := lastReq
		mu.Unlock()
		if observed.Path != "/websockify" {
			t.Fatalf("expected upstream path /websockify, got %q", observed.Path)
		}
		if observed.RawQuery != "token=vnc-sec-token&autoconnect=true" {
			t.Fatalf("expected upstream raw query 'token=vnc-sec-token&autoconnect=true', got %q", observed.RawQuery)
		}

		// Verify bidirectional payload exchange over hijacked stream
		testMsg := "ping-rfb\n"
		if _, err := conn.Write([]byte(testMsg)); err != nil {
			t.Fatalf("failed to write over upgraded WS connection: %v", err)
		}
		echoLine, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("failed to read echo from upgraded WS connection: %v", err)
		}
		if echoLine != "echo:"+testMsg {
			t.Fatalf("expected echo:%s, got %q", testMsg, echoLine)
		}
	})

	t.Run("Query and path forwarding for subpaths", func(t *testing.T) {
		reqURL := fmt.Sprintf("%s/api/v1/connection/auth/%s/screen/app/images/icons/novnc.svg?resize=scale&logging=warn",
			gatewayServer.URL, ownerAttempt.ID)
		resp, err := gatewayServer.Client().Get(reqURL)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		expectedBody := "upstream path=/app/images/icons/novnc.svg query=resize=scale&logging=warn"
		if string(body) != expectedBody {
			t.Fatalf("expected body %q, got %q", expectedBody, string(body))
		}

		mu.Lock()
		observed := lastReq
		mu.Unlock()
		if observed.Path != "/app/images/icons/novnc.svg" {
			t.Fatalf("expected path /app/images/icons/novnc.svg, got %q", observed.Path)
		}
		if observed.RawQuery != "resize=scale&logging=warn" {
			t.Fatalf("expected query 'resize=scale&logging=warn', got %q", observed.RawQuery)
		}
	})

	t.Run("Ownership enforcement", func(t *testing.T) {
		mu.Lock()
		countBefore := reqCount
		mu.Unlock()

		// 1. Another owner's attempt must be rejected (404) and never forwarded upstream
		diffOwnerURL := fmt.Sprintf("%s/api/v1/connection/auth/%s/screen/websockify",
			gatewayServer.URL, otherAttempt.ID)
		respDiff, err := gatewayServer.Client().Get(diffOwnerURL)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		respDiff.Body.Close()
		if respDiff.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 for mismatched owner, got %d", respDiff.StatusCode)
		}

		// 2. Nonexistent attempt must be rejected (404)
		nonexistentURL := fmt.Sprintf("%s/api/v1/connection/auth/auth_nonexistent999/screen/vnc.html",
			gatewayServer.URL)
		respNonexistent, err := gatewayServer.Client().Get(nonexistentURL)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		respNonexistent.Body.Close()
		if respNonexistent.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 for nonexistent attempt, got %d", respNonexistent.StatusCode)
		}

		// 3. Verify upstream was never touched for unauthorized requests
		mu.Lock()
		countAfter := reqCount
		mu.Unlock()
		if countAfter != countBefore {
			t.Fatalf("upstream was called %d times for unauthorized requests", countAfter-countBefore)
		}
	})
}
