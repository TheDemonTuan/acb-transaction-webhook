package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
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
		if val, ok := dirs["connect-src"]; !ok || len(val) != 1 || val[0] != "'self'" {
			t.Fatalf("connect-src mismatch: %v", dirs["connect-src"])
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
		callsBefore := upstreamCalls
		r := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/connection/auth/nonexistent/screen/vnc.html", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", w.Code)
		}
		if upstreamCalls != callsBefore {
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
