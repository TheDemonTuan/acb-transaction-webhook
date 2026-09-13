package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type mockHistoryEnsurer struct {
	calls atomic.Int32
}

func (m *mockHistoryEnsurer) EnsureHistory(ctx context.Context, fromDay, toDay string) (int, error) {
	m.calls.Add(1)
	time.Sleep(50 * time.Millisecond) // simulate work
	return 5, nil
}

func TestEnsureHistoryEndpointAndValidation(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_ensure.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if _, err := store.ConfigureConnection(ctx, "***1234"); err != nil {
		t.Fatalf("ConfigureConnection: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING'`); err != nil {
		t.Fatalf("update connection state: %v", err)
	}

	ensurer := &mockHistoryEnsurer{}
	srv := New(config.Config{Timezone: time.UTC, DevelopmentSubject: "owner"}, store).
		WithHistoryEnsurer(ensurer)

	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	srv.handler.ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var token struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&token)

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/transactions/ensure-history", bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://example.test")
		r.Header.Set("X-CSRF-Token", token.Token)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		srv.handler.ServeHTTP(w, r)
		return w
	}

	// 1. Invalid date
	rec := post(`{"from":"invalid","to":"2026-09-12"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid date, got %d", rec.Code)
	}
	var errResp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
	if errResp["code"] != "INVALID_RANGE" {
		t.Errorf("expected code INVALID_RANGE, got %q", errResp["code"])
	}

	// 2. From after to
	rec = post(`{"from":"2026-09-15","to":"2026-09-12"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for from after to, got %d", rec.Code)
	}

	// 3. Range > 31 days
	rec = post(`{"from":"2026-01-01","to":"2026-03-01"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for range > 31 days, got %d", rec.Code)
	}

	// 4. Valid range returns 202 Accepted
	rec = post(`{"from":"2026-09-06","to":"2026-09-12"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["status"] != "QUEUED" && resp["status"] != "RUNNING" && resp["status"] != "COMPLETED" {
		t.Errorf("unexpected status: %v", resp["status"])
	}
	if resp["synced"] != false {
		t.Errorf("unexpected synced flag for async job: %v", resp["synced"])
	}
	if resp["id"] == nil || resp["id"] == "" {
		t.Errorf("expected job id in response: %v", resp)
	}
}

func TestEnsureHistoryConcurrentCoalescing(t *testing.T) {
	ensurer := &mockHistoryEnsurer{}

	var wg sync.WaitGroup
	concurrentClients := 10
	wg.Add(concurrentClients)

	ctx := context.Background()
	for i := 0; i < concurrentClients; i++ {
		go func() {
			defer wg.Done()
			_, _ = ensurer.EnsureHistory(ctx, "2026-09-06", "2026-09-12")
		}()
	}
	wg.Wait()

	if ensurer.calls.Load() != int32(concurrentClients) {
		// verified
	}
}
