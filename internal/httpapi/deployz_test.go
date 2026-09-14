package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/telemetry"
)

type mockWorkerProber struct {
	err error
}

func (m *mockWorkerProber) Ready(ctx context.Context) error {
	return m.err
}

func TestDeployzEndpoint_AuthenticationAndPayload(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "deployz_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	workerToken := "super-secret-worker-token-xyz"
	cfg := config.Config{
		Production:          true,
		DevelopmentSubject:  "dev@example.com",
		WorkerInternalToken: workerToken,
		Slot:                "blue",
		ReleaseCommit:       "git-commit-abc1234",
	}

	server := New(cfg, store)
	handler := server.Handler()

	// 1. Unauthenticated -> 401
	reqUnauth := httptest.NewRequest(http.MethodGet, "/internal/deployz", nil)
	recUnauth := httptest.NewRecorder()
	handler.ServeHTTP(recUnauth, reqUnauth)
	if recUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing token, got %d: %s", recUnauth.Code, recUnauth.Body.String())
	}

	// 2. Bad token -> 401
	reqBadToken := httptest.NewRequest(http.MethodGet, "/internal/deployz", nil)
	reqBadToken.Header.Set("X-Worker-Internal-Token", "wrong-token")
	recBadToken := httptest.NewRecorder()
	handler.ServeHTTP(recBadToken, reqBadToken)
	if recBadToken.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong token, got %d: %s", recBadToken.Code, recBadToken.Body.String())
	}

	// 3. Authorized request (Monolith mode) -> 200
	reqAuth := httptest.NewRequest(http.MethodGet, "/internal/deployz", nil)
	reqAuth.Header.Set("X-Worker-Internal-Token", workerToken)
	recAuth := httptest.NewRecorder()
	handler.ServeHTTP(recAuth, reqAuth)
	if recAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 for authorized deployz, got %d: %s", recAuth.Code, recAuth.Body.String())
	}
	if slot := recAuth.Header().Get("X-Platform-Slot"); slot != "blue" {
		t.Errorf("expected X-Platform-Slot 'blue', got %q", slot)
	}
	if rel := recAuth.Header().Get("X-Release-Commit"); rel != "git-commit-abc1234" {
		t.Errorf("expected X-Release-Commit 'git-commit-abc1234', got %q", rel)
	}

	var resp map[string]any
	if err := json.Unmarshal(recAuth.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["status"] != "ready" {
		t.Errorf("expected status 'ready', got %v", resp["status"])
	}
	if resp["release"] != "git-commit-abc1234" {
		t.Errorf("expected release 'git-commit-abc1234', got %v", resp["release"])
	}
	if resp["slot"] != "blue" {
		t.Errorf("expected slot 'blue', got %v", resp["slot"])
	}
	if resp["nonce"] == nil || resp["nonce"] == "" {
		t.Errorf("expected non-empty instance nonce")
	}
	if resp["storage"] != "ready" {
		t.Errorf("expected storage 'ready', got %v", resp["storage"])
	}
	if resp["worker"] != "monolith" {
		t.Errorf("expected worker 'monolith', got %v", resp["worker"])
	}
}

func TestDeployzEndpoint_WorkerProberIntegration(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "deployz_worker_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	workerToken := "worker-token-xyz"
	cfg := config.Config{
		WorkerRPCURL:        "http://acb-worker:8190",
		WorkerInternalToken: workerToken,
		Slot:                "green",
		ReleaseCommit:       "sha256-green-123",
	}

	prober := &mockWorkerProber{err: errors.New("worker connection refused")}
	server := New(cfg, store).WithWorkerProber(prober)
	handler := server.Handler()

	// 1. Worker failing -> 503
	req := httptest.NewRequest(http.MethodGet, "/internal/deployz", nil)
	req.Header.Set("X-Worker-Internal-Token", workerToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when worker is failing, got %d: %s", rec.Code, rec.Body.String())
	}
	var respFail map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &respFail)
	if respFail["status"] != "not_ready" {
		t.Errorf("expected status 'not_ready', got %v", respFail["status"])
	}

	// 2. Worker healthy -> 200
	prober.err = nil
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 when worker is healthy, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var respOK map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &respOK)
	if respOK["status"] != "ready" || respOK["worker"] != "ready" {
		t.Errorf("expected status ready and worker ready, got: %v", respOK)
	}
}

func TestDeployzReportsRealtimeDegradedWithoutFailingReadiness(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "deployz_realtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	oldDefault := telemetry.Default
	telemetry.Default = telemetry.NewRegistry()
	defer func() { telemetry.Default = oldDefault }()
	telemetry.Default.SetRealtimeStreamState(true, "degraded", "unauthorized")

	cfg := config.Config{WorkerRealtimeEnabled: true, WorkerInternalToken: "secret"}
	server := New(cfg, store)
	req := httptest.NewRequest(http.MethodGet, "/internal/deployz", nil)
	req.Header.Set("X-Worker-Internal-Token", "secret")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected degraded deployz to remain 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"degraded"`) || !strings.Contains(rec.Body.String(), `"realtime":"degraded"`) {
		t.Fatalf("expected realtime degraded response, got %s", rec.Body.String())
	}
}

func TestGatewayReadyzRemainsLocalDBOnly(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "readyz_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Gateway with worker URL, but worker is down
	cfg := config.Config{
		WorkerRPCURL:  "http://nonexistent-worker:8190",
		Slot:          "green",
		ReleaseCommit: "git-commit-readyz",
	}
	server := New(cfg, store)
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected gateway /readyz to return 200 checking local DB only, got %d: %s", rec.Code, rec.Body.String())
	}
	if slot := rec.Header().Get("X-Platform-Slot"); slot != "green" {
		t.Errorf("expected X-Platform-Slot 'green', got %q", slot)
	}
	if rel := rec.Header().Get("X-Release-Commit"); rel != "git-commit-readyz" {
		t.Errorf("expected X-Release-Commit 'git-commit-readyz', got %q", rel)
	}
}
