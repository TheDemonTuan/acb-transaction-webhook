package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestGateway_StaleWorkerDetectionInDeployz(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "stale_worker.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	cfg := config.Config{
		RuntimeRole:         config.RuntimeRoleGateway,
		Slot:                "green",
		ReleaseCommit:       "release-commit-xyz",
		WorkerInternalToken: "test-token",
		WorkerRPCURL:        "http://127.0.0.1:8190",
	}

	staleProber := &mockWorkerProber{
		err: errors.New("worker is stale: last heartbeat was 120s ago"),
	}

	srv := New(cfg, store).WithWorkerProber(staleProber)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/deployz", nil)
	req.Header.Set("X-Worker-Internal-Token", "test-token")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for stale worker in deployz, got %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode deployz response: %v", err)
	}

	if body["status"] != "not_ready" {
		t.Errorf("expected status 'not_ready', got %v", body["status"])
	}

	workerVal, ok := body["worker"].(string)
	if !ok || workerVal == "" {
		t.Fatalf("expected worker field in response, got %v", body["worker"])
	}
	if workerVal != "stale: worker is stale: last heartbeat was 120s ago" {
		t.Errorf("unexpected worker error message: %q", workerVal)
	}
}
