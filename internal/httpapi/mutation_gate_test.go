package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestMutationGate_GuardsAllGatewayWrites(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_api_gate.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	cfg := config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		ReleaseCommit:      "f0ebc62f7dc6382f2ab52f5a525e07ff0e909a34",
		Slot:               "blue",
	}

	h := New(cfg, store).Handler()

	// Get CSRF token and cookie
	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	h.ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var csrfToken struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&csrfToken)

	doReq := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://example.test"+path, bytes.NewBufferString(body))
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("Origin", "http://example.test")
		r.Header.Set("X-CSRF-Token", csrfToken.Token)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	// 1. Initially when gate is OPEN, configure connection succeeds
	wConf := doReq(http.MethodPost, "/api/v1/connection/configure", `{"accountMasked":"***1234"}`)
	if wConf.Code != http.StatusCreated && wConf.Code != http.StatusOK {
		t.Fatalf("expected 200/201 for configure when gate is OPEN, got %d %s", wConf.Code, wConf.Body.String())
	}

	// 2. Lock mutation gate (simulate deployment transaction)
	gate, err := store.AcquireMutationGate(ctx, "deploy-worker-test", 5*time.Minute, "upgrade")
	if err != nil {
		t.Fatalf("acquire mutation gate: %v", err)
	}

	// 3. Verify write endpoints return 503 MUTATION_GATE_LOCKED
	writeEndpoints := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/connection/auth/start", `{}`},
		{http.MethodPost, "/api/v1/connection/pause", `{}`},
		{http.MethodPost, "/api/v1/webhooks", `{"name":"test","url":"https://example.com"}`},
		{http.MethodPost, "/api/v1/payment-qr", `{"accountNumber":"123","accountName":"Test"}`},
		{http.MethodPut, "/api/v1/voice/settings", `{"providerMode":"ONLINE_AUTO","edgeVoice":"vi-VN-HoaiMyNeural"}`},
		{http.MethodPost, "/api/v1/monitor/settings", `{"enabled":true}`},
		{http.MethodPost, "/api/v1/transactions/ensure-history", `{"days":7}`},
	}

	for _, ep := range writeEndpoints {
		w := doReq(ep.method, ep.path, ep.body)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("expected 503 for %s %s while gate is LOCKED, got %d %s", ep.method, ep.path, w.Code, w.Body.String())
		}
		if retry := w.Header().Get("Retry-After"); retry != "5" {
			t.Errorf("expected Retry-After: 5 on %s %s, got %q", ep.method, ep.path, retry)
		}
		if gateHdr := w.Header().Get("X-Mutation-Gate"); gateHdr != "LOCKED" {
			t.Errorf("expected X-Mutation-Gate: LOCKED on %s %s, got %q", ep.method, ep.path, gateHdr)
		}
		var errResp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &errResp)
		if errResp["code"] != "MUTATION_GATE_LOCKED" {
			t.Errorf("expected code MUTATION_GATE_LOCKED on %s %s, got %+v", ep.method, ep.path, errResp)
		}
	}

	// 4. Verify READ endpoints continue serving 200 OK while gate is locked!
	readEndpoints := []string{
		"/api/v1/status",
		"/api/v1/connection",
		"/api/v1/transactions",
		"/api/v1/deliveries",
		"/api/v1/poll-runs",
		"/api/v1/realtime/status",
		"/api/v1/csrf",
	}

	for _, path := range readEndpoints {
		w := doReq(http.MethodGet, path, "")
		if w.Code != http.StatusOK {
			t.Errorf("expected 200 for GET %s while gate is LOCKED, got %d", path, w.Code)
		}
	}

	// 5. Release gate
	if err := store.ReleaseMutationGate(ctx, "deploy-worker-test", gate.LeaseToken); err != nil {
		t.Fatalf("release gate: %v", err)
	}

	// 6. After release: writes succeed again
	wPostRelease := doReq(http.MethodPost, "/api/v1/payment-qr", `{"accountNumber":"123","accountName":"Test"}`)
	if wPostRelease.Code != http.StatusOK {
		t.Fatalf("expected 200 for write after gate release, got %d %s", wPostRelease.Code, wPostRelease.Body.String())
	}
}
