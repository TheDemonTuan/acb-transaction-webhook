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
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/httpapi"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type mockPaymentActivator struct {
	called atomic.Int64
	state  httpapi.PaymentActivationState
	err    error
}

func (m *mockPaymentActivator) ActivatePaymentWindow(ctx context.Context) (httpapi.PaymentActivationState, error) {
	m.called.Add(1)
	if m.err != nil {
		return httpapi.PaymentActivationState{}, m.err
	}
	if m.state.Phase == "" {
		return httpapi.PaymentActivationState{
			TrackingActive: true,
			Phase:          "GRACE",
			NextPhaseAt:    time.Now().Add(5 * time.Second),
		}, nil
	}
	return m.state, nil
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

func TestPaymentActivation_MissingIdentifierRejected(t *testing.T) {
	activator := &mockPaymentActivator{}
	ts, _ := setupActivationServer(t, activator)

	// Blank JSON body without identifier must return 400 IDENTIFIER_REQUIRED
	resp, err := http.Post(ts.URL+"/api/public/v1/payment-qr/activate", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for blank body, got %d", resp.StatusCode)
	}

	var data map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&data)
	if data["code"] != "IDENTIFIER_REQUIRED" {
		t.Fatalf("expected code IDENTIFIER_REQUIRED, got %v", data["code"])
	}
	if activator.called.Load() != 0 {
		t.Fatalf("activator must not be called when identifier missing")
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

func TestPaymentActivation_WorkerUnavailableReturns503(t *testing.T) {
	activator := &mockPaymentActivator{err: errors.New("worker RPC connection refused")}
	ts, _ := setupActivationServer(t, activator)

	body := `{"identifier":"safe_token_12345"}`
	resp, err := http.Post(ts.URL+"/api/public/v1/payment-qr/activate", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when RPC fails, got %d", resp.StatusCode)
	}

	var data map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&data)
	if data["trackingActive"] != false {
		t.Fatalf("expected trackingActive: false when worker unavailable, got %v", data["trackingActive"])
	}
	if data["code"] != "ACTIVATION_UNAVAILABLE" {
		t.Fatalf("expected code ACTIVATION_UNAVAILABLE, got %v", data["code"])
	}
}

func TestPaymentActivation_DebounceReplaysFailureHonestly(t *testing.T) {
	activator := &mockPaymentActivator{err: errors.New("worker RPC down")}
	ts, _ := setupActivationServer(t, activator)

	// Call 1 fails
	body := `{"identifier":"token_alpha_1234"}`
	req1, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/public/v1/payment-qr/activate", strings.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("CF-Connecting-IP", "198.51.100.10")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("call 1: expected 503, got %d", resp1.StatusCode)
	}

	// Call 2 within 2 seconds MUST still report failure honestly, not fake trackingActive: true
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/public/v1/payment-qr/activate", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("CF-Connecting-IP", "198.51.100.11") // distinct IP bypasses IP bucket
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("call 2: debouncer must NOT forge 200 OK after failed RPC, got status %d", resp2.StatusCode)
	}
	var data2 map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&data2)
	if data2["trackingActive"] != false {
		t.Fatalf("call 2: trackingActive must be false on replayed failure, got %v", data2["trackingActive"])
	}

	// Worker should only be called once because error was debounced/replayed
	if count := activator.called.Load(); count != 1 {
		t.Fatalf("expected exactly 1 worker RPC call, got %d", count)
	}
}

func TestPaymentActivation_BudgetExhaustedReturns429(t *testing.T) {
	activator := &mockPaymentActivator{
		state: httpapi.PaymentActivationState{
			TrackingActive: false,
			Phase:          "LOCKED",
			NextPhaseAt:    time.Now().Add(10 * time.Minute),
		},
	}
	ts, _ := setupActivationServer(t, activator)

	body := `{"identifier":"token_locked_1234"}`
	resp, err := http.Post(ts.URL+"/api/public/v1/payment-qr/activate", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests when boost budget exhausted, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header on budget exhaustion")
	}
	var data map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&data)
	if data["code"] != "BOOST_BUDGET_EXHAUSTED" {
		t.Fatalf("expected code BOOST_BUDGET_EXHAUSTED, got %v", data["code"])
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

	ip := "203.0.113.50"
	var lastStatus int
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/public/v1/payment-qr/activate",
			strings.NewReader(`{"identifier":"token_12345678"}`))
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
		t.Fatalf("expected 429 Too Many Requests for third rapid request from same IP, got %d", lastStatus)
	}
}

func TestPaymentActivation_RepeatedActivationSafeAndDebounced(t *testing.T) {
	activator := &mockPaymentActivator{}
	ts, _ := setupActivationServer(t, activator)

	// Send 3 requests in rapid succession with safe identifiers and distinct IPs
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/public/v1/payment-qr/activate",
			strings.NewReader(`{"identifier":"token_repeat_1234"}`))
		req.Header.Set("Content-Type", "application/json")
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
