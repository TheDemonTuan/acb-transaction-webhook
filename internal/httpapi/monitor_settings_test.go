package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type mockMonitorNotifier struct {
	wakeCalls atomic.Int32
}

func (m *mockMonitorNotifier) NotifySettingsChanged(ctx context.Context) error {
	m.wakeCalls.Add(1)
	return nil
}

func TestMonitorSettingsAPIAndHotReloadWake(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_mon_api.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	notifier := &mockMonitorNotifier{}
	srv := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner"}, store).
		WithMonitorNotifier(notifier)

	// 1. GET /api/v1/monitor/settings
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/settings", nil)
	recGet := httptest.NewRecorder()
	srv.handler.ServeHTTP(recGet, reqGet)
	if recGet.Code != http.StatusOK {
		t.Fatalf("expected 200 for GET, got %d: %s", recGet.Code, recGet.Body.String())
	}

	var getResp struct {
		Settings storage.MonitorSettings `json:"settings"`
		Current  map[string]any          `json:"current"`
	}
	if err := json.Unmarshal(recGet.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("unmarshal GET response: %v", err)
	}
	if getResp.Settings.Revision != 1 {
		t.Errorf("expected revision 1, got %d", getResp.Settings.Revision)
	}
	if getResp.Current["mode"] == "" {
		t.Errorf("expected non-empty current mode")
	}

	// 2. PUT /api/v1/monitor/settings with CSRF
	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	srv.handler.ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var token struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&token)

	postSettings := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://example.test/api/v1/monitor/settings", bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://example.test")
		r.Header.Set("X-CSRF-Token", token.Token)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		srv.handler.ServeHTTP(w, r)
		return w
	}

	updatePayload := getResp.Settings
	updatePayload.DefaultProfile.MinSeconds = 130
	payloadBytes, _ := json.Marshal(updatePayload)

	recPut := postSettings(http.MethodPost, string(payloadBytes))
	if recPut.Code != http.StatusOK {
		t.Fatalf("expected 200 for POST, got %d: %s", recPut.Code, recPut.Body.String())
	}
	if getResp.Settings.DefaultProfile.MinSeconds != 120 || getResp.Settings.DefaultProfile.MaxSeconds != 180 {
		t.Fatalf("unexpected default keepalive interval: %+v", getResp.Settings.DefaultProfile)
	}
	if got := updatePayload.Windows[0].Profile; got.MinSeconds != 3 || got.MaxSeconds != 10 {
		t.Fatalf("unexpected default realtime interval: %+v", got)
	}

	// Verify notifier was signaled to wake the timer immediately!
	if notifier.wakeCalls.Load() != 1 {
		t.Errorf("expected 1 wake call, got %d", notifier.wakeCalls.Load())
	}

	// Re-sending with stale revision should return 409 conflict
	recStale := postSettings(http.MethodPut, string(payloadBytes))
	if recStale.Code != http.StatusConflict {
		t.Errorf("expected 409 conflict on stale revision, got %d: %s", recStale.Code, recStale.Body.String())
	}
}
