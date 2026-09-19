package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
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
