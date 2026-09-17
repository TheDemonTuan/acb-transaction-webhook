package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/httpapi"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type mockPaymentActivator struct {
	called atomic.Int64
	err    error
}

func (m *mockPaymentActivator) ActivatePaymentWindow(ctx context.Context) error {
	m.called.Add(1)
	return m.err
}

func setupActivationServer(t *testing.T, activator httpapi.PaymentWindowActivator) (*httptest.Server, *storage.Store) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		Production: false,
	}
	server := httpapi.New(cfg, store)
	if activator != nil {
		server.WithPaymentActivator(activator)
	}

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		ts.Close()
		store.Close()
	})
	return ts, store
}

func TestPaymentActivation_ValidPOST(t *testing.T) {
	activator := &mockPaymentActivator{}
	ts, _ := setupActivationServer(t, activator)

	body := []byte(`{"identifier": "valid_token_12345678"}`)
	resp, err := http.Post(ts.URL+"/api/public/v1/payment-qr/activate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /payment-qr/activate failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("expected Cache-Control: no-store, got %q", cc)
	}

	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if data["trackingActive"] != true {
		t.Fatalf("expected trackingActive: true, got %v", data["trackingActive"])
	}
	if data["phase"] != "GRACE" {
		t.Fatalf("expected phase: GRACE, got %v", data["phase"])
	}

	if activator.called.Load() != 1 {
		t.Fatalf("expected activator called once, got %d", activator.called.Load())
	}
}

func TestPaymentActivation_GETMethodNotAllowed(t *testing.T) {
	activator := &mockPaymentActivator{}
	ts, _ := setupActivationServer(t, activator)

	resp, err := http.Get(ts.URL + "/api/public/v1/payment-qr/activate")
	if err != nil {
		t.Fatalf("GET /payment-qr/activate failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", resp.StatusCode)
	}
}

func TestPaymentActivation_WorkerUnavailableSafeDegraded(t *testing.T) {
	activator := &mockPaymentActivator{err: errors.New("worker RPC connection refused")}
	ts, _ := setupActivationServer(t, activator)

	resp, err := http.Post(ts.URL+"/api/public/v1/payment-qr/activate", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for degraded response, got %d", resp.StatusCode)
	}

	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if data["trackingActive"] != false {
		t.Fatalf("expected trackingActive: false when worker unavailable, got %v", data["trackingActive"])
	}
}

func TestPaymentActivation_NoMetadataLeaked(t *testing.T) {
	activator := &mockPaymentActivator{err: errors.New("sensitive internal database error at 10.0.0.1")}
	ts, _ := setupActivationServer(t, activator)

	resp, err := http.Post(ts.URL+"/api/public/v1/payment-qr/activate", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if _, hasErr := data["error"]; hasErr {
		t.Fatalf("expected no internal error leaked in response, got %v", data)
	}
}

func TestPaymentActivation_MalformedIdentifierRejected(t *testing.T) {
	activator := &mockPaymentActivator{}
	ts, _ := setupActivationServer(t, activator)

	testCases := []string{
		`{"identifier": "short"}`,
		`{"identifier": "../../../etc/passwd"}`,
		`{"identifier": "<script>alert(1)</script>"}`,
		`{"identifier": "has space in identifier"}`,
	}

	for i, tc := range testCases {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/public/v1/payment-qr/activate", strings.NewReader(tc))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("CF-Connecting-IP", "198.51.100."+string(rune('1'+i)))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("payload %s expected 400 Bad Request, got %d", tc, resp.StatusCode)
		}
	}
}

func TestPaymentActivation_RateLimitPerIP(t *testing.T) {
	activator := &mockPaymentActivator{}
	ts, _ := setupActivationServer(t, activator)

	// Single IP sending 3 requests in a row without pause
	ip := "203.0.113.50"
	var lastStatus int
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/public/v1/payment-qr/activate", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("CF-Connecting-IP", ip)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST failed: %v", err)
		}
		lastStatus = resp.StatusCode
		resp.Body.Close()
	}

	// Third request from same IP within burst limit must be 429
	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests for exceeding IP burst, got %d", lastStatus)
	}
}

func TestPaymentActivation_RepeatedActivationSafeAndDebounced(t *testing.T) {
	activator := &mockPaymentActivator{}
	ts, _ := setupActivationServer(t, activator)

	// Send 3 requests in rapid succession
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/public/v1/payment-qr/activate", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		// Simulate distinct IPs to bypass per-IP limiter and test debouncer collapse
		req.Header.Set("CF-Connecting-IP", "198.51.100."+string(rune('1'+i)))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %d failed: %v", i, err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %d: expected 200 OK, got %d", i, resp.StatusCode)
		}
	}

	// Worker should only be invoked once due to 2-second collapse
	if count := activator.called.Load(); count != 1 {
		t.Fatalf("expected activator to be called only once due to debounce collapse, got %d", count)
	}
}
