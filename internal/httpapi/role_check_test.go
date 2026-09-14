package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestGateway_ExactRouteACKAndSplitRoleChecks(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "role_check.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	cfg := config.Config{
		RuntimeRole:         config.RuntimeRoleGateway,
		Slot:                "blue",
		ReleaseCommit:       "commit-abc-123",
		WorkerInternalToken: "valid-secret-token",
		WorkerRPCURL:        "http://127.0.0.1:8190",
	}
	srv := New(cfg, store).WithWorkerProber(&mockWorkerProber{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := ts.Client()

	t.Run("GET /healthz route ACK headers and role check", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/healthz", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		if slot := resp.Header.Get("X-Platform-Slot"); slot != "blue" {
			t.Errorf("expected X-Platform-Slot 'blue', got %q", slot)
		}
		if rel := resp.Header.Get("X-Release-Commit"); rel != "commit-abc-123" {
			t.Errorf("expected X-Release-Commit 'commit-abc-123', got %q", rel)
		}
		if role := resp.Header.Get("X-Runtime-Role"); role != "gateway" {
			t.Errorf("expected X-Runtime-Role 'gateway', got %q", role)
		}
	})

	t.Run("GET /healthz role match gateway", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/healthz?role=gateway", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for role=gateway, got %d", resp.StatusCode)
		}
	})

	t.Run("GET /healthz role mismatch worker fails", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/healthz?role=worker", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for role=worker on gateway, got %d", resp.StatusCode)
		}
	})

	t.Run("GET /readyz role and release match", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/readyz?role=gateway&release=commit-abc-123", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("GET /readyz release mismatch fails", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/readyz?release=wrong-commit", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for wrong release, got %d", resp.StatusCode)
		}
	})

	t.Run("GET /readyz schema mismatch fails", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/readyz?schema=9999", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for wrong schema, got %d", resp.StatusCode)
		}
	})

	t.Run("GET /internal/deployz role and release verification", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/deployz?role=gateway&release=commit-abc-123", nil)
		req.Header.Set("X-Worker-Internal-Token", "valid-secret-token")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode deployz: %v", err)
		}
		if body["status"] != "ready" {
			t.Errorf("expected status 'ready', got %v", body["status"])
		}
		if body["role"] != "gateway" {
			t.Errorf("expected role 'gateway', got %v", body["role"])
		}
	})

	t.Run("GET /internal/deployz wrong role fails", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/internal/deployz?role=worker", nil)
		req.Header.Set("X-Worker-Internal-Token", "valid-secret-token")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for wrong role on deployz, got %d", resp.StatusCode)
		}
	})
}
