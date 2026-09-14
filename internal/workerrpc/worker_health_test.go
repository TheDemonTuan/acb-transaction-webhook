package workerrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerstate"
)

func TestWorkerRPC_HealthAndReadinessSemantics(t *testing.T) {
	mockHandler := &mockWorkerHandler{}
	srv, err := workerrpc.NewServer(mockHandler, "test-worker-token")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	lastHb := time.Now().UTC()
	srv.SetRuntimeInfo("worker", "worker-commit-123", "blue", "8")
	srv.SetHeartbeatProvider(func() time.Time {
		return lastHb
	})
	srv.SetStaleThreshold(60 * time.Second)

	isReady := true
	srv.SetReadyChecker(func(ctx context.Context) error {
		if !isReady {
			return errors.New("worker not ready")
		}
		return nil
	})
	state := workerstate.StateReady
	srv.SetStateProvider(func() workerstate.State {
		return state
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	t.Run("GET /healthz process liveness 200 OK and headers", func(t *testing.T) {
		resp, err := client.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatalf("healthz failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}
		if role := resp.Header.Get("X-Runtime-Role"); role != "worker" {
			t.Errorf("expected X-Runtime-Role 'worker', got %q", role)
		}
		if rel := resp.Header.Get("X-Release-Commit"); rel != "worker-commit-123" {
			t.Errorf("expected X-Release-Commit 'worker-commit-123', got %q", rel)
		}
	})

	t.Run("GET /healthz remains 200 OK during ACB maintenance or auth required", func(t *testing.T) {
		isReady = false
		defer func() { isReady = true }()

		resp, err := client.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatalf("healthz failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("healthz must stay 200 during degraded state, got %d", resp.StatusCode)
		}
	})

	t.Run("GET /healthz role check match and mismatch", func(t *testing.T) {
		respOK, _ := client.Get(ts.URL + "/healthz?role=worker")
		if respOK.StatusCode != http.StatusOK {
			t.Errorf("expected 200 for role=worker, got %d", respOK.StatusCode)
		}
		respOK.Body.Close()

		respErr, _ := client.Get(ts.URL + "/healthz?role=gateway")
		if respErr.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected 503 for role=gateway on worker, got %d", respErr.StatusCode)
		}
		respErr.Body.Close()
	})

	t.Run("GET /readyz role and release check", func(t *testing.T) {
		resp, _ := client.Get(ts.URL + "/readyz?role=worker&release=worker-commit-123&schema=8")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for matching readyz probe, got %d", resp.StatusCode)
		}
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()

		if body["status"] != "ready" || body["role"] != "worker" {
			t.Errorf("unexpected body: %+v", body)
		}

		respWrongRole, _ := client.Get(ts.URL + "/readyz?role=gateway")
		if respWrongRole.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected 503 for wrong role, got %d", respWrongRole.StatusCode)
		}
		respWrongRole.Body.Close()

		respWrongRel, _ := client.Get(ts.URL + "/readyz?release=wrong-release")
		if respWrongRel.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected 503 for wrong release, got %d", respWrongRel.StatusCode)
		}
		respWrongRel.Body.Close()

		respWrongSchema, _ := client.Get(ts.URL + "/readyz?schema=99")
		if respWrongSchema.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected 503 for wrong schema, got %d", respWrongSchema.StatusCode)
		}
		respWrongSchema.Body.Close()
	})

	t.Run("GET /readyz detects stale worker heartbeat", func(t *testing.T) {
		lastHb = time.Now().UTC().Add(-120 * time.Second)
		defer func() { lastHb = time.Now().UTC() }()

		resp, err := client.Get(ts.URL + "/readyz")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for stale worker heartbeat, got %d", resp.StatusCode)
		}
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if body["stale"] != true {
			t.Errorf("expected stale: true in response, got %v", body["stale"])
		}
	})

	t.Run("GET /readyz returns 503 during worker draining while healthz stays 200", func(t *testing.T) {
		state = workerstate.StateDraining
		isReady = false
		defer func() {
			state = workerstate.StateReady
			isReady = true
		}()

		respHealth, _ := client.Get(ts.URL + "/healthz")
		if respHealth.StatusCode != http.StatusOK {
			t.Errorf("expected healthz 200 during drain, got %d", respHealth.StatusCode)
		}
		respHealth.Body.Close()

		respReady, _ := client.Get(ts.URL + "/readyz")
		if respReady.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected readyz 503 during drain, got %d", respReady.StatusCode)
		}
		respReady.Body.Close()
	})
}
