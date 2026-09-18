package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func setupTestServerForQR(t *testing.T) (*Server, *storage.Store, string, *http.Cookie, string) {
	t.Helper()
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "test_qr.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	connID := "conn_qr_preview_test"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	srv := New(config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		DatabasePath:       dbPath,
	}, store)

	// Fetch CSRF token
	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	srv.handler.ServeHTTP(csrfRec, csrfReq)
	cookies := csrfRec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("no csrf cookie returned")
	}
	cookie := cookies[0]
	var tokenResp struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&tokenResp)

	return srv, store, dbDir, cookie, tokenResp.Token
}

func TestPaymentQRPreviewIsolation(t *testing.T) {
	srv, store, tempDir, cookie, csrfToken := setupTestServerForQR(t)

	// Establish a baseline payment QR in database
	qrDir := filepath.Join(tempDir, "qr")
	_ = os.MkdirAll(qrDir, 0o750)
	initialPath := filepath.Join(qrDir, "qr_initial.png")
	_ = os.WriteFile(initialPath, []byte("fake initial png"), 0o640)

	initialQR := storage.PaymentQR{
		AccountNumber: "9999999999",
		AccountName:   "BASELINE OWNER",
		ImagePath:     initialPath,
		ImageHash:     "initialhash12345",
		Provider:      "MANUAL",
	}
	savedBaseline, err := store.SavePaymentQR(context.Background(), initialQR)
	if err != nil {
		t.Fatalf("failed to save baseline QR: %v", err)
	}

	modes := []string{"standard", "reference", "hybrid"}
	for _, mode := range modes {
		t.Run("Mode_"+mode, func(t *testing.T) {
			bodyData := map[string]string{
				"accountNumber": "123456789",
				"accountName":   "TEST NGUYEN",
				"mode":          mode,
				"testId":        "TEST_TOKEN_01",
				"host":          "test.gateway.local",
			}
			reqBody, _ := json.Marshal(bodyData)
			req := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/payment-qr/preview", bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "http://example.test")
			req.Header.Set("X-CSRF-Token", csrfToken)
			req.AddCookie(cookie)

			w := httptest.NewRecorder()
			srv.handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
			}

			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to unmarshal response: %v", err)
			}

			// Validate payload fields
			payload, ok := resp["payload"].(string)
			if !ok || payload == "" {
				t.Fatal("expected non-empty payload string")
			}

			imgURI, ok := resp["image"].(string)
			if !ok || !strings.HasPrefix(imgURI, "data:image/png;base64,") {
				t.Fatalf("expected data URI image, got %v", resp["image"])
			}

			crcValid, ok := resp["crcValid"].(bool)
			if !ok || !crcValid {
				t.Fatalf("expected crcValid to be true, got %v", resp["crcValid"])
			}

			parsed, ok := resp["parsed"].(map[string]any)
			if !ok {
				t.Fatal("expected parsed metadata object")
			}
			if parsed["bin"] != "970416" {
				t.Errorf("expected BIN 970416, got %v", parsed["bin"])
			}
			if parsed["accountNumber"] != "123456789" {
				t.Errorf("expected account 123456789, got %v", parsed["accountNumber"])
			}
			if parsed["service"] != "QRIBFTTA" {
				t.Errorf("expected service QRIBFTTA, got %v", parsed["service"])
			}

			if mode == "reference" || mode == "hybrid" {
				if parsed["reference"] != "TEST_TOKEN_01" {
					t.Errorf("expected reference TEST_TOKEN_01, got %v", parsed["reference"])
				}
			}
			if mode == "hybrid" {
				if parsed["customTemplate"] != true {
					t.Errorf("expected customTemplate to be true")
				}
				if parsed["customHost"] != "test.gateway.local" {
					t.Errorf("expected customHost test.gateway.local, got %v", parsed["customHost"])
				}
				if parsed["customToken"] != "TEST_TOKEN_01" {
					t.Errorf("expected customToken TEST_TOKEN_01, got %v", parsed["customToken"])
				}
			}

			// CRITICAL ASSERTION: Zero mutations to DB or production QR files!
			currentQR, err := store.GetPaymentQR(context.Background(), "")
			if err != nil {
				t.Fatalf("store.GetPaymentQR failed: %v", err)
			}
			if currentQR.Revision != savedBaseline.Revision {
				t.Fatalf("DB mutation detected! Revision changed from %d to %d", savedBaseline.Revision, currentQR.Revision)
			}
			if currentQR.AccountNumber != "9999999999" {
				t.Fatalf("DB mutation detected! AccountNumber changed to %s", currentQR.AccountNumber)
			}

			// Ensure baseline file is intact
			diskData, err := os.ReadFile(initialPath)
			if err != nil || string(diskData) != "fake initial png" {
				t.Fatalf("Production QR file modified or missing!")
			}
		})
	}
	_ = tempDir
}

func TestPaymentQRCanaryEndpoint(t *testing.T) {
	srv, _, _, cookie, csrfToken := setupTestServerForQR(t)

	token := "CANARY_TEST_123"

	// 1. Check initial status before any hit
	reqStatus := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/payment-qr/canary/"+token, nil)
	reqStatus.AddCookie(cookie)
	wStatus := httptest.NewRecorder()
	srv.handler.ServeHTTP(wStatus, reqStatus)
	if wStatus.Code != http.StatusOK {
		t.Fatalf("GET canary status returned %d", wStatus.Code)
	}
	var initStatus map[string]any
	_ = json.Unmarshal(wStatus.Body.Bytes(), &initStatus)
	if int(initStatus["hitCount"].(float64)) != 0 {
		t.Fatalf("expected initial hitCount 0, got %v", initStatus["hitCount"])
	}

	// 2. Hit public canary endpoint
	reqCanary := httptest.NewRequest(http.MethodGet, "/api/public/v1/payment-qr/canary/"+token, nil)
	reqCanary.Header.Set("User-Agent", "ACB_ONE_App/3.0.0 (iPhone; iOS 17.5)")
	reqCanary.Header.Set("CF-Connecting-IP", "203.0.113.42")
	wCanary := httptest.NewRecorder()
	srv.handler.ServeHTTP(wCanary, reqCanary)

	if wCanary.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from canary hit, got %d: %s", wCanary.Code, wCanary.Body.String())
	}
	var canaryResp map[string]any
	_ = json.Unmarshal(wCanary.Body.Bytes(), &canaryResp)
	if canaryResp["status"] != "recorded" {
		t.Fatalf("expected status 'recorded', got %v", canaryResp["status"])
	}

	// 3. Inspect canary status via admin endpoint
	reqStatus2 := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/payment-qr/canary/"+token, nil)
	reqStatus2.AddCookie(cookie)
	wStatus2 := httptest.NewRecorder()
	srv.handler.ServeHTTP(wStatus2, reqStatus2)

	if wStatus2.Code != http.StatusOK {
		t.Fatalf("GET canary status returned %d", wStatus2.Code)
	}
	var status2 map[string]any
	_ = json.Unmarshal(wStatus2.Body.Bytes(), &status2)
	if int(status2["hitCount"].(float64)) != 1 {
		t.Fatalf("expected hitCount 1, got %v", status2["hitCount"])
	}
	if status2["lastUserAgent"] != "ACB_ONE_App/3.0.0 (iPhone; iOS 17.5)" {
		t.Fatalf("expected lastUserAgent recorded, got %v", status2["lastUserAgent"])
	}
	if status2["lastSourceIp"] != "203.0.113.42" {
		t.Fatalf("expected lastSourceIp 203.0.113.42, got %v", status2["lastSourceIp"])
	}

	// 4. Test invalid token rejection
	reqInvalid := httptest.NewRequest(http.MethodGet, "/api/public/v1/payment-qr/canary/bad%20token!$", nil)
	wInvalid := httptest.NewRecorder()
	srv.handler.ServeHTTP(wInvalid, reqInvalid)
	if wInvalid.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad token, got %d", wInvalid.Code)
	}

	// 5. Test reset canary status
	reqReset := httptest.NewRequest(http.MethodDelete, "http://example.test/api/v1/payment-qr/canary/"+token, nil)
	reqReset.Header.Set("Origin", "http://example.test")
	reqReset.Header.Set("X-CSRF-Token", csrfToken)
	reqReset.AddCookie(cookie)
	wReset := httptest.NewRecorder()
	srv.handler.ServeHTTP(wReset, reqReset)
	if wReset.Code != http.StatusOK {
		t.Fatalf("DELETE canary status returned %d: %s", wReset.Code, wReset.Body.String())
	}

	// Verify hitCount reset to 0
	reqStatus3 := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/payment-qr/canary/"+token, nil)
	reqStatus3.AddCookie(cookie)
	wStatus3 := httptest.NewRecorder()
	srv.handler.ServeHTTP(wStatus3, reqStatus3)
	var status3 map[string]any
	_ = json.Unmarshal(wStatus3.Body.Bytes(), &status3)
	if int(status3["hitCount"].(float64)) != 0 {
		t.Fatalf("expected reset hitCount 0, got %v", status3["hitCount"])
	}
}

func TestPaymentQRPromotionLocalStandard(t *testing.T) {
	srv, store, _, cookie, csrfToken := setupTestServerForQR(t)

	bodyData := map[string]string{
		"accountNumber": "0123456789",
		"accountName":   "PROMOTED ACCOUNT OWNER",
		"mode":          "local_standard",
	}
	reqBody, _ := json.Marshal(bodyData)
	req := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/payment-qr/generate", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.test")
	req.Header.Set("X-CSRF-Token", csrfToken)
	req.AddCookie(cookie)

	w := httptest.NewRecorder()
	srv.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	// Assert stored payment QR
	qr, err := store.GetPaymentQR(context.Background(), "")
	if err != nil || qr == nil {
		t.Fatalf("failed to fetch saved QR: %v", err)
	}

	if qr.AccountNumber != "0123456789" {
		t.Errorf("expected AccountNumber 0123456789, got %s", qr.AccountNumber)
	}
	if qr.AccountName != "PROMOTED ACCOUNT OWNER" {
		t.Errorf("expected AccountName PROMOTED ACCOUNT OWNER, got %s", qr.AccountName)
	}
	if qr.Provider != "LOCAL_VIETQR" {
		t.Errorf("expected Provider LOCAL_VIETQR, got %s", qr.Provider)
	}

	// Verify image file on disk
	fileData, err := os.ReadFile(qr.ImagePath)
	if err != nil {
		t.Fatalf("failed to read generated image file from disk: %v", err)
	}
	if len(fileData) == 0 {
		t.Fatal("generated image file is empty")
	}
}
