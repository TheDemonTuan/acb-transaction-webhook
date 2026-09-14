package httpapi

import (
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

func TestStatusIncludesLastSuccessfulPoll(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "status.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`); err != nil {
		t.Fatal(err)
	}
	poll, err := store.StartPoll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	poll.Status = "SUCCEEDED"
	if err := store.FinishPoll(ctx, poll); err != nil {
		t.Fatal(err)
	}

	h := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner"}, store).Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	var result struct {
		ACB struct {
			LastSuccessfulPollAt *string `json:"lastSuccessfulPollAt"`
		} `json:"acb"`
	}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.ACB.LastSuccessfulPollAt == nil || *result.ACB.LastSuccessfulPollAt == "" {
		t.Fatal("expected lastSuccessfulPollAt")
	}
}

func TestHealthStatusAndAPIRouting(t *testing.T) {
	t.Parallel()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := New(config.Config{Timezone: time.UTC}, store).Handler()
	for _, tc := range []struct {
		path        string
		code        int
		contentType string
	}{{"/healthz", http.StatusOK, "application/json"}, {"/readyz", http.StatusOK, "application/json"}, {"/api/v1/status", http.StatusOK, "application/json"}, {"/transactions", http.StatusNotFound, "text/plain"}, {"/api/v1/missing", http.StatusNotFound, "text/plain"}} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != tc.code {
			t.Fatalf("%s: code=%d, want %d", tc.path, rec.Code, tc.code)
		}
		if got := rec.Header().Get("Content-Type"); len(got) < len(tc.contentType) || got[:len(tc.contentType)] != tc.contentType {
			t.Fatalf("%s: Content-Type=%q", tc.path, got)
		}
		if rec.Header().Get("X-Request-Id") == "" {
			t.Fatalf("%s missing request ID", tc.path)
		}
	}
}
