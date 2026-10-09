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

func TestACBRuntimeRoutesRemoved(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "removed-routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Existing ACB history is inert: removed endpoints must not mutate its source.
	_, err = store.DB().ExecContext(ctx, `INSERT INTO connections(id,bank_code,state,generation,created_at,updated_at) VALUES('legacy-acb','ACB','PAUSED',7,'2026-09-12T00:00:00Z','2026-09-12T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	h := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner"}, store).Handler()
	csrf := httptest.NewRecorder()
	h.ServeHTTP(csrf, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil))
	var token struct{ Token string }
	if err := json.Unmarshal(csrf.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	cookie := csrf.Result().Cookies()[0]
	paths := []string{
		"/api/v1/connection", "/api/v1/connection/configure", "/api/v1/connection/credentials/grant", "/api/v1/connection/credentials",
		"/api/v1/connection/pause", "/api/v1/connection/resume", "/api/v1/connection/sync",
		"/api/v1/connection/auth/start", "/api/v1/connection/auth/status", "/api/v1/connection/auth/otp", "/api/v1/connection/auth/cancel",
		"/api/v1/monitor/settings", "/api/v1/poll-runs", "/api/v1/transactions/ensure-history",
		"/api/v1/transactions/history-sync-jobs/latest", "/api/v1/transactions/history-sync-jobs/old", "/api/v1/transactions/history-sync-jobs/old/cancel",
		"/api/v1/history-sync-jobs/latest", "/api/v1/history-sync-jobs/old", "/api/v1/history-sync-jobs/old/cancel",
	}
	for _, prefix := range []string{"/api/v1", "/api/public/v1"} {
		for _, suffix := range []string{"/payment-activity", "/payment-activity/stop", "/payment-readiness", "/payment-qr", "/payment-qr/upload", "/payment-qr/generate", "/payment-qr/image"} {
			paths = append(paths, prefix+suffix)
		}
	}
	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			t.Run(method+" "+path, func(t *testing.T) {
				r := httptest.NewRequest(method, "http://example.test"+path, bytes.NewBufferString(`{}`))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Origin", "http://example.test")
				r.Header.Set("X-CSRF-Token", token.Token)
				r.AddCookie(cookie)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
					t.Fatalf("removed route responded %d: %s", w.Code, w.Body.String())
				}
			})
		}
	}
	var state string
	var generation int
	if err := store.DB().QueryRowContext(ctx, `SELECT state,generation FROM connections WHERE id='legacy-acb'`).Scan(&state, &generation); err != nil {
		t.Fatal(err)
	}
	if state != "PAUSED" || generation != 7 {
		t.Fatalf("legacy history source mutated: %s/%d", state, generation)
	}
	var journalCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM event_journal`).Scan(&journalCount); err != nil {
		t.Fatal(err)
	}
	if journalCount != 0 {
		t.Fatalf("removed routes wrote %d journal entries", journalCount)
	}
}
