package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

type mockPaymentBooster struct {
	calledWithAmount    int64
	calledWithSessionID string
	callCount           int
	stopCallCount       int
	err                 error
}

func (m *mockPaymentBooster) StartPaymentBoost(ctx context.Context, amount int64) (workerrpc.PaymentBoostStatus, error) {
	m.calledWithAmount = amount
	m.callCount++
	if m.err != nil {
		return workerrpc.PaymentBoostStatus{}, m.err
	}
	return workerrpc.PaymentBoostStatus{
		Active:     true,
		SessionID:  "test-session-id-123",
		AmountVnd:  amount,
		ExpiresIn:  180,
		Phase:      1,
		MinSeconds: 1,
		MaxSeconds: 3,
	}, nil
}

func (m *mockPaymentBooster) StopPaymentBoost(ctx context.Context, sessionID string) error {
	m.calledWithSessionID = sessionID
	m.stopCallCount++
	return m.err
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestPaymentActivity_API_ValidationAndBoost(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "api_boost.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING' WHERE id = ?`, conn.ID); err != nil {
		t.Fatal(err)
	}

	booster := &mockPaymentBooster{}
	cfg := config.Config{
		DatabasePath: filepath.Join(t.TempDir(), "api_boost.db"),
	}
	srv := New(cfg, store).WithPaymentBooster(booster)

	// 1. Valid request with amount
	body := bytes.NewBufferString(`{"amountVnd": 250000}`)
	req := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity", body)
	req.RemoteAddr = "192.168.1.100:12345"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if booster.calledWithAmount != 250000 {
		t.Fatalf("expected booster to receive 250000, got %d", booster.calledWithAmount)
	}

	var status workerrpc.PaymentBoostStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !status.Active || status.AmountVnd != 250000 || status.Phase != 1 || status.MinSeconds != 1 || status.MaxSeconds != 3 || status.SessionID != "test-session-id-123" {
		t.Fatalf("unexpected response payload: %+v", status)
	}

	// 2. Negative amount -> 400 Bad Request
	bodyNeg := bytes.NewBufferString(`{"amountVnd": -500}`)
	reqNeg := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity", bodyNeg)
	reqNeg.RemoteAddr = "192.168.1.100:12345"
	recNeg := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recNeg, reqNeg)

	if recNeg.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative amount, got %d: %s", recNeg.Code, recNeg.Body.String())
	}

	// 3. Empty body -> amountVnd defaults to 0 -> 200 OK
	reqEmpty := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity", bytes.NewBuffer(nil))
	reqEmpty.RemoteAddr = "192.168.1.100:12345"
	recEmpty := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recEmpty, reqEmpty)

	if recEmpty.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty body, got %d: %s", recEmpty.Code, recEmpty.Body.String())
	}
	if booster.calledWithAmount != 0 {
		t.Fatalf("expected booster to receive 0, got %d", booster.calledWithAmount)
	}
}

func TestPaymentActivity_RateLimiting(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "api_boost_rl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING' WHERE id = ?`, conn.ID); err != nil {
		t.Fatal(err)
	}

	booster := &mockPaymentBooster{}
	cfg := config.Config{
		DatabasePath: filepath.Join(t.TempDir(), "api_boost_rl.db"),
	}
	srv := New(cfg, store).WithPaymentBooster(booster)

	clientIP := "203.0.113.42:5000"

	// 6 allowed requests in same window
	for i := 1; i <= 6; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity", bytes.NewBufferString(`{"amountVnd": 100000}`))
		req.RemoteAddr = clientIP
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	// 7th request from same IP -> 429 Too Many Requests
	reqBlocked := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity", bytes.NewBufferString(`{"amountVnd": 100000}`))
	reqBlocked.RemoteAddr = clientIP
	recBlocked := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBlocked, reqBlocked)

	if recBlocked.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for 7th request, got %d: %s", recBlocked.Code, recBlocked.Body.String())
	}

	// Request from distinct IP is NOT blocked
	reqOtherIP := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity", bytes.NewBufferString(`{"amountVnd": 100000}`))
	reqOtherIP.RemoteAddr = "198.51.100.99:5000"
	recOtherIP := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recOtherIP, reqOtherIP)

	if recOtherIP.Code != http.StatusOK {
		t.Fatalf("expected 200 for distinct IP, got %d: %s", recOtherIP.Code, recOtherIP.Body.String())
	}
}

func TestDynamicVietQRImage_Streaming(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "api_qr_stream.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	connID := "conn_qr_stream_test"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatal(err)
	}

	// Seed configured payment QR settings
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO payment_qr_settings(id, connection_id, revision, bin, bank_name, account_number, account_name, image_path, image_hash, image_content_type, created_at, updated_at)
		VALUES('singleton', ?, 1, '970416', 'ACB', '9876543210', 'NGUYEN VAN A', '/dummy/path.png', 'hash123', 'image/png', '2026-09-19T00:00:00Z', '2026-09-19T00:00:00Z')
	`, connID)
	if err != nil {
		t.Fatal(err)
	}

	// Mock HTTP client that simulates VietQR image response
	fakePNG := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D}
	var capturedUpstreamURL string

	mockTransport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedUpstreamURL = req.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(fakePNG)),
		}, nil
	})
	mockClient := &http.Client{Transport: mockTransport}

	cfg := config.Config{
		DatabasePath: filepath.Join(t.TempDir(), "api_qr_stream.db"),
	}
	srv := New(cfg, store).WithVietQRClient(mockClient)

	// 1. Request with amount=100000
	req := httptest.NewRequest(http.MethodGet, "/api/public/v1/payment-qr/image?amount=100000", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("expected Content-Type image/png, got %s", rec.Header().Get("Content-Type"))
	}
	if !bytes.Equal(rec.Body.Bytes(), fakePNG) {
		t.Fatalf("expected streamed image bytes to match mock PNG")
	}

	// Verify upstream VietQR URL contains compact format, amount=100000, and account name
	if !strings.Contains(capturedUpstreamURL, "970416-9876543210-compact.png") {
		t.Errorf("unexpected upstream URL: %s", capturedUpstreamURL)
	}
	if !strings.Contains(capturedUpstreamURL, "amount=100000") {
		t.Errorf("expected amount=100000 in upstream URL, got: %s", capturedUpstreamURL)
	}
	if !strings.Contains(capturedUpstreamURL, "accountName=NGUYEN+VAN+A") && !strings.Contains(capturedUpstreamURL, "accountName=NGUYEN%20VAN%20A") {
		t.Errorf("expected accountName in upstream URL, got: %s", capturedUpstreamURL)
	}

	// 2. Request with invalid amount parameter -> 400 Bad Request
	reqInvalid := httptest.NewRequest(http.MethodGet, "/api/public/v1/payment-qr/image?amount=abc", nil)
	recInvalid := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recInvalid, reqInvalid)

	if recInvalid.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid amount, got %d: %s", recInvalid.Code, recInvalid.Body.String())
	}
}

func TestPaymentActivity_StopEndpoints(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "api_boost_stop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	booster := &mockPaymentBooster{}
	cfg := config.Config{
		DatabasePath: filepath.Join(t.TempDir(), "api_boost_stop.db"),
	}
	srv := New(cfg, store).WithPaymentBooster(booster)

	// 1. DELETE /api/public/v1/payment-activity with query param sessionId
	reqDel := httptest.NewRequest(http.MethodDelete, "/api/public/v1/payment-activity?sessionId=sess-del-abc", nil)
	recDel := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recDel, reqDel)

	if recDel.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recDel.Code, recDel.Body.String())
	}
	if booster.stopCallCount != 1 {
		t.Fatalf("expected stopCallCount 1, got %d", booster.stopCallCount)
	}
	if booster.calledWithSessionID != "sess-del-abc" {
		t.Fatalf("expected calledWithSessionID sess-del-abc, got %s", booster.calledWithSessionID)
	}

	// 2. POST /api/public/v1/payment-activity/stop with JSON body
	bodyPost := bytes.NewBufferString(`{"sessionId": "sess-post-xyz"}`)
	reqPost := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity/stop", bodyPost)
	recPost := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recPost, reqPost)

	if recPost.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recPost.Code, recPost.Body.String())
	}
	if booster.stopCallCount != 2 {
		t.Fatalf("expected stopCallCount 2, got %d", booster.stopCallCount)
	}
	if booster.calledWithSessionID != "sess-post-xyz" {
		t.Fatalf("expected calledWithSessionID sess-post-xyz, got %s", booster.calledWithSessionID)
	}
}

func TestPublicPaymentReadiness(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "public_readiness.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Config{DatabasePath: dbPath}
	srv := New(cfg, store)

	// 1. Unconfigured store
	reqUnconf := httptest.NewRequest(http.MethodGet, "/api/public/v1/payment-readiness", nil)
	recUnconf := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recUnconf, reqUnconf)

	if recUnconf.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recUnconf.Code, recUnconf.Body.String())
	}
	var unconfResp map[string]any
	if err := json.Unmarshal(recUnconf.Body.Bytes(), &unconfResp); err != nil {
		t.Fatalf("unmarshal unconfResp: %v", err)
	}
	if unconfResp["ready"] != false || unconfResp["status"] != "UNCONFIGURED" {
		t.Fatalf("expected ready=false status=UNCONFIGURED, got %+v", unconfResp)
	}
	// Sanity check: must only contain ready and status, no leaked internal details
	if len(unconfResp) != 2 {
		t.Fatalf("expected sanitized payload with 2 keys, got %d keys: %+v", len(unconfResp), unconfResp)
	}

	// 2. Configured with AUTH_REQUIRED
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}
	reqAuthReq := httptest.NewRequest(http.MethodGet, "/api/public/v1/payment-readiness", nil)
	recAuthReq := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recAuthReq, reqAuthReq)

	if recAuthReq.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recAuthReq.Code, recAuthReq.Body.String())
	}
	var authReqResp map[string]any
	if err := json.Unmarshal(recAuthReq.Body.Bytes(), &authReqResp); err != nil {
		t.Fatalf("unmarshal authReqResp: %v", err)
	}
	if authReqResp["ready"] != false || authReqResp["status"] != "AUTH_REQUIRED" {
		t.Fatalf("expected ready=false status=AUTH_REQUIRED, got %+v", authReqResp)
	}
	if len(authReqResp) != 2 {
		t.Fatalf("expected sanitized payload with 2 keys, got %+v", authReqResp)
	}

	// 3. Updated to MONITORING
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING' WHERE id = ?`, conn.ID); err != nil {
		t.Fatal(err)
	}
	reqMon := httptest.NewRequest(http.MethodGet, "/api/public/v1/payment-readiness", nil)
	recMon := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recMon, reqMon)

	if recMon.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recMon.Code, recMon.Body.String())
	}
	var monResp map[string]any
	if err := json.Unmarshal(recMon.Body.Bytes(), &monResp); err != nil {
		t.Fatalf("unmarshal monResp: %v", err)
	}
	if monResp["ready"] != true || monResp["status"] != "READY" {
		t.Fatalf("expected ready=true status=READY, got %+v", monResp)
	}
	if len(monResp) != 2 {
		t.Fatalf("expected sanitized payload with 2 keys, got %+v", monResp)
	}
}

func TestPaymentBoost_RejectionWhenBankNotReady(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "boost_rejection.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	booster := &mockPaymentBooster{}
	cfg := config.Config{DatabasePath: dbPath}
	srv := New(cfg, store).WithPaymentBooster(booster)

	// 1. Connection is AUTH_REQUIRED -> local store fast-path rejection 409 PAYMENT_NOT_READY
	conn, err := store.ConfigureConnection(ctx, "***1234")
	if err != nil {
		t.Fatal(err)
	}

	body := bytes.NewBufferString(`{"amountVnd": 100000}`)
	req := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity", body)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when AUTH_REQUIRED, got %d: %s", rec.Code, rec.Body.String())
	}
	var errResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal errResp: %v", err)
	}
	if errResp["code"] != "PAYMENT_NOT_READY" {
		t.Fatalf("expected code=PAYMENT_NOT_READY, got %+v", errResp)
	}
	if booster.callCount != 0 {
		t.Fatalf("booster should not be called when bank connection is unready, callCount=%d", booster.callCount)
	}

	// 2. Connection is MONITORING in local store, but booster reports connection not ready (race/worker sync)
	if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='MONITORING' WHERE id = ?`, conn.ID); err != nil {
		t.Fatal(err)
	}
	booster.err = errors.New("bank connection is not in MONITORING state")

	body2 := bytes.NewBufferString(`{"amountVnd": 100000}`)
	req2 := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity", body2)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when booster rejects with not in MONITORING, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var errResp2 map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &errResp2); err != nil {
		t.Fatalf("unmarshal errResp2: %v", err)
	}
	if errResp2["code"] != "PAYMENT_NOT_READY" {
		t.Fatalf("expected code=PAYMENT_NOT_READY from booster error, got %+v", errResp2)
	}

	// 3. Connection is MONITORING and booster succeeds -> 200 OK
	booster.err = nil
	body3 := bytes.NewBufferString(`{"amountVnd": 100000}`)
	req3 := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity", body3)
	rec3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

func TestStartPaymentActivity_FailClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("missing bank connection returns 409 PAYMENT_NOT_READY", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "unconfigured.db")
		store, err := storage.Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		booster := &mockPaymentBooster{}
		srv := New(config.Config{DatabasePath: dbPath}, store).WithPaymentBooster(booster)

		req := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity", bytes.NewBufferString(`{"amountVnd": 100000}`))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["code"] != "PAYMENT_NOT_READY" || resp["status"] != "UNCONFIGURED" {
			t.Fatalf("expected code=PAYMENT_NOT_READY status=UNCONFIGURED, got %+v", resp)
		}
		if booster.callCount != 0 {
			t.Fatalf("booster should not be called, got callCount=%d", booster.callCount)
		}
	})

	t.Run("store failure returns 503 PAYMENT_UNAVAILABLE", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "store_fail.db")
		store, err := storage.Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		// Close underlying store to trigger query failure on lookup
		store.Close()

		booster := &mockPaymentBooster{}
		srv := New(config.Config{DatabasePath: dbPath}, store).WithPaymentBooster(booster)

		req := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity", bytes.NewBufferString(`{"amountVnd": 100000}`))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["code"] != "PAYMENT_UNAVAILABLE" {
			t.Fatalf("expected code=PAYMENT_UNAVAILABLE, got %+v", resp)
		}
		if booster.callCount != 0 {
			t.Fatalf("booster should not be called, got callCount=%d", booster.callCount)
		}
	})

	t.Run("non-monitoring state returns 409 PAYMENT_NOT_READY", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "non_monitoring.db")
		store, err := storage.Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		conn, err := store.ConfigureConnection(ctx, "***1234")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().ExecContext(ctx, `UPDATE connections SET state='AUTH_STARTING' WHERE id = ?`, conn.ID); err != nil {
			t.Fatal(err)
		}

		booster := &mockPaymentBooster{}
		srv := New(config.Config{DatabasePath: dbPath}, store).WithPaymentBooster(booster)

		req := httptest.NewRequest(http.MethodPost, "/api/public/v1/payment-activity", bytes.NewBufferString(`{"amountVnd": 100000}`))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["code"] != "PAYMENT_NOT_READY" || resp["status"] != "AUTH_STARTING" {
			t.Fatalf("expected code=PAYMENT_NOT_READY status=AUTH_STARTING, got %+v", resp)
		}
		if booster.callCount != 0 {
			t.Fatalf("booster should not be called, got callCount=%d", booster.callCount)
		}
	})
}
