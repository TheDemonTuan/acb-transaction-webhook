package httpapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/bark"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/workerrpc"
)

func setupTestServerWithKeyring(t *testing.T) (*Server, *storage.Store) {
	t.Helper()
	ctx := context.Background()
	keyPath := filepath.Join(t.TempDir(), "master.key")
	rawKey := make([]byte, 32)
	for i := range rawKey {
		rawKey[i] = byte(i + 1)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(rawKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	kr, err := security.LoadKeyring(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "test_notif.db")
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.WithKeyring(kr)

	cfg := config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		DatabasePath:       dbPath,
		MasterKeyFile:      keyPath,
	}

	srv := New(cfg, store)
	return srv, store
}

func getCSRF(srv *Server) (string, *http.Cookie) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)
	cookie := rec.Result().Cookies()[0]
	var token struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &token)
	return token.Token, cookie
}

func prepareAuthedPost(url string, body []byte, csrf string, cookie *http.Cookie) *http.Request {
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader([]byte{})
	}
	req := httptest.NewRequest(http.MethodPost, url, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.test")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(cookie)
	return req
}

func prepareAuthedPut(url string, body []byte, csrf string, cookie *http.Cookie) *http.Request {
	req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.test")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(cookie)
	return req
}

func TestNotificationProvidersAndChannelsLifecycle(t *testing.T) {
	srv, store := setupTestServerWithKeyring(t)
	defer store.Close()

	// 1. GET /api/v1/notification-providers
	req := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/notification-providers", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var provResp struct {
		Providers []struct {
			ID         string `json:"id"`
			Configured bool   `json:"configured"`
		} `json:"providers"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &provResp)
	if len(provResp.Providers) != 2 {
		t.Fatalf("expected 2 providers, got: %d", len(provResp.Providers))
	}

	csrf, cookie := getCSRF(srv)

	// 2. Create Bark Channel via POST /api/v1/notification-channels
	barkBody, _ := json.Marshal(map[string]any{
		"provider":  "BARK",
		"name":      "iPhone Tuan",
		"deviceKey": "bark_key_super_secret_999",
		"barkConfig": map[string]any{
			"group":          "BankAlerts",
			"level":          "timeSensitive",
			"includeBalance": true,
		},
	})
	reqCreate := prepareAuthedPost("http://example.test/api/v1/notification-channels", barkBody, csrf, cookie)

	recCreate := httptest.NewRecorder()
	srv.handler.ServeHTTP(recCreate, reqCreate)
	if recCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 created, got %d: %s", recCreate.Code, recCreate.Body.String())
	}

	var createdCh storage.NotificationChannel
	_ = json.Unmarshal(recCreate.Body.Bytes(), &createdCh)
	if createdCh.ID == "" || createdCh.Provider != "BARK" || createdCh.Status != "DISABLED" || !createdCh.HasDeviceKey {
		t.Fatalf("unexpected created channel: %+v", createdCh)
	}
	// Verify device key is NEVER in the response body!
	if strings.Contains(recCreate.Body.String(), "bark_key_super_secret_999") {
		t.Fatalf("device key leaked in response body!")
	}

	// 3. GET /api/v1/notification-channels
	reqList := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/notification-channels", nil)
	recList := httptest.NewRecorder()
	srv.handler.ServeHTTP(recList, reqList)
	if recList.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recList.Code)
	}
	var listResp struct {
		Items []storage.NotificationChannel `json:"items"`
	}
	_ = json.Unmarshal(recList.Body.Bytes(), &listResp)
	if len(listResp.Items) != 1 || listResp.Items[0].ID != createdCh.ID {
		t.Fatalf("unexpected channel list: %+v", listResp)
	}
	if strings.Contains(recList.Body.String(), "bark_key_super_secret_999") {
		t.Fatalf("device key leaked in list response body!")
	}

	// 4. Update Bark Channel via PUT /api/v1/notification-channels/{id}
	updateBody, _ := json.Marshal(map[string]any{
		"expectedRevision": 1,
		"name":             "iPhone Tuan Renamed",
		"barkConfig": map[string]any{
			"group": "PersonalAlerts",
			"sound": "minuet",
		},
	})
	reqUpdate := prepareAuthedPut("http://example.test/api/v1/notification-channels/"+createdCh.ID, updateBody, csrf, cookie)

	recUpdate := httptest.NewRecorder()
	srv.handler.ServeHTTP(recUpdate, reqUpdate)
	if recUpdate.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recUpdate.Code, recUpdate.Body.String())
	}

	// 5. Toggle Channel Enable/Disable
	reqEnable := prepareAuthedPost("http://example.test/api/v1/notification-channels/"+createdCh.ID+"/enable", nil, csrf, cookie)

	recEnable := httptest.NewRecorder()
	srv.handler.ServeHTTP(recEnable, reqEnable)
	if recEnable.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recEnable.Code, recEnable.Body.String())
	}

	chAfter, _ := store.NotificationChannelByID(context.Background(), createdCh.ID)
	if chAfter.Status != "ACTIVE" {
		t.Fatalf("expected status ACTIVE, got: %s", chAfter.Status)
	}

	// 6. Rotate Bark Device Key
	rotateBody, _ := json.Marshal(map[string]any{
		"deviceKey": "new_rotated_key_777",
	})
	reqRotate := prepareAuthedPost("http://example.test/api/v1/notification-channels/"+createdCh.ID+"/rotate-secret", rotateBody, csrf, cookie)

	recRotate := httptest.NewRecorder()
	srv.handler.ServeHTTP(recRotate, reqRotate)
	if recRotate.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recRotate.Code, recRotate.Body.String())
	}
	if strings.Contains(recRotate.Body.String(), "new_rotated_key_777") {
		t.Fatalf("device key leaked in rotate response body!")
	}
}

func TestRotateSecretMalformedBodyHasNoSideEffects(t *testing.T) {
	srv, store := setupTestServerWithKeyring(t)
	defer store.Close()

	ch, err := store.CreateBarkChannel(context.Background(), "Test Phone", "original_device_key", nil)
	if err != nil {
		t.Fatal(err)
	}
	csrf, cookie := getCSRF(srv)
	req := prepareAuthedPost("http://example.test/api/v1/notification-channels/"+ch.ID+"/rotate-secret", []byte(`{"deviceKey":`), csrf, cookie)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var keyCount int
	if err := store.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM endpoint_secrets WHERE endpoint_id = ?`, ch.ID).Scan(&keyCount); err != nil {
		t.Fatal(err)
	}
	if keyCount != 1 {
		t.Fatalf("malformed rotation changed key count to %d", keyCount)
	}
}

func TestBarkTestNotificationEndpoint(t *testing.T) {
	srv, store := setupTestServerWithKeyring(t)
	defer store.Close()

	// Mock Bark Server
	var receivedPush bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/push" {
			receivedPush = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code":    200,
				"message": "success",
			})
		}
	}))
	defer ts.Close()

	barkCfg := bark.Config{
		ServerURL: ts.URL,
		Timeout:   2 * time.Second,
	}
	barkSender := bark.NewSender(barkCfg, ts.Client(), "")
	srv.WithBarkSender(barkSender)

	ch, err := store.CreateBarkChannel(context.Background(), "Test Phone", "device_key_smoke", nil)
	if err != nil {
		t.Fatal(err)
	}

	csrf, cookie := getCSRF(srv)

	reqTest := prepareAuthedPost("http://example.test/api/v1/notification-channels/"+ch.ID+"/test", nil, csrf, cookie)

	recTest := httptest.NewRecorder()
	srv.handler.ServeHTTP(recTest, reqTest)
	if recTest.Code != http.StatusOK {
		t.Fatalf("expected 200 test success, got %d: %s", recTest.Code, recTest.Body.String())
	}
	if !receivedPush {
		t.Fatalf("expected push request sent to bark mock server")
	}

	// Immediate second test -> 429 Too Many Requests (cooldown)
	reqTest2 := prepareAuthedPost("http://example.test/api/v1/notification-channels/"+ch.ID+"/test", nil, csrf, cookie)

	recTest2 := httptest.NewRecorder()
	srv.handler.ServeHTTP(recTest2, reqTest2)
	if recTest2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 TooManyRequests on cooldown, got %d", recTest2.Code)
	}
}

func TestReplayDeliveryEndpoint(t *testing.T) {
	srv, store := setupTestServerWithKeyring(t)
	defer store.Close()

	ch, _ := store.CreateBarkChannel(context.Background(), "Replay Phone", "key_rp", nil)
	_ = store.SetEndpointStatus(context.Background(), ch.ID, "ACTIVE")

	nowAt := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = store.DB().ExecContext(context.Background(), `
		INSERT INTO events(id, event_type, payload, payload_hash, created_at)
		VALUES('evt_deliv_rp', 'bank.transaction.credit', X'7B7D', 'hash', ?)
	`, nowAt)
	_, _ = store.DB().ExecContext(context.Background(), `
		INSERT INTO deliveries(id, event_id, endpoint_id, endpoint_revision, key_id, status, attempts, next_attempt_at, created_at, updated_at)
		VALUES('deliv_dead_1', 'evt_deliv_rp', ?, 1, 'k1', 'DEAD_LETTER', 3, ?, ?, ?)
	`, ch.ID, nowAt, nowAt, nowAt)

	csrf, cookie := getCSRF(srv)

	var woken bool
	srv.WithWakeDispatcher(func(ctx context.Context) error {
		woken = true
		return nil
	})

	reqReplay := prepareAuthedPost("http://example.test/api/v1/deliveries/deliv_dead_1/replay", nil, csrf, cookie)

	recReplay := httptest.NewRecorder()
	srv.handler.ServeHTTP(recReplay, reqReplay)
	if recReplay.Code != http.StatusOK {
		t.Fatalf("expected 200 replay success, got %d: %s", recReplay.Code, recReplay.Body.String())
	}
	if !woken {
		t.Fatalf("expected dispatcher wakeFn to be called after replay")
	}

	// Replay again immediately -> 409 Conflict (since status is now PENDING)
	recReplay2 := httptest.NewRecorder()
	srv.handler.ServeHTTP(recReplay2, reqReplay)
	if recReplay2.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when delivery is already PENDING, got %d", recReplay2.Code)
	}
}

type mockChannelTester struct {
	calledChannelID string
	resp            workerrpc.TestNotificationResponse
	err             error
}

func (m *mockChannelTester) TestNotificationChannel(ctx context.Context, channelID string) (workerrpc.TestNotificationResponse, error) {
	m.calledChannelID = channelID
	return m.resp, m.err
}

func TestChannelTestDelegatesToWorkerRPC(t *testing.T) {
	srv, store := setupTestServerWithKeyring(t)

	ch, err := store.CreateBarkChannel(context.Background(), "Worker Bark", "device_key_worker", nil)
	if err != nil {
		t.Fatal(err)
	}

	tester := &mockChannelTester{
		resp: workerrpc.TestNotificationResponse{
			Success:   true,
			Status:    "DELIVERED",
			LatencyMs: 99,
			Message:   "Worker delivered Bark push",
		},
	}
	srv.WithNotificationTester(tester)

	csrf, cookie := getCSRF(srv)
	req := prepareAuthedPost("http://example.test/api/v1/notification-channels/"+ch.ID+"/test", nil, csrf, cookie)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from worker RPC delegation, got %d: %s", rec.Code, rec.Body.String())
	}
	if tester.calledChannelID != ch.ID {
		t.Fatalf("expected tester called with channel %s, got %s", ch.ID, tester.calledChannelID)
	}
	if !strings.Contains(rec.Body.String(), "DELIVERED") {
		t.Fatalf("expected DELIVERED response, got %s", rec.Body.String())
	}
}

func TestBarkDecryptionFailureFromWorkerReturnsActionableMessage(t *testing.T) {
	srv, store := setupTestServerWithKeyring(t)
	ch, err := store.CreateBarkChannel(context.Background(), "Worker Bark", "device_key_worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	tester := &mockChannelTester{resp: workerrpc.TestNotificationResponse{
		Status:            "FAILED",
		ProviderErrorCode: storage.ErrCodeBarkKeyDecryptionFailed,
		SanitizedError:    storage.ErrMsgBarkKeyDecryptionFailed,
	}}
	srv.WithNotificationTester(tester)
	csrf, cookie := getCSRF(srv)
	req := prepareAuthedPost("http://example.test/api/v1/notification-channels/"+ch.ID+"/test", nil, csrf, cookie)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 from worker decryption failure, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"`+storage.ErrCodeBarkKeyDecryptionFailed+`"`) || !strings.Contains(rec.Body.String(), `"requestId"`) {
		t.Fatalf("expected standard actionable response, got %s", rec.Body.String())
	}
}

func TestBarkTestEndpointDecryptionFailureReturnsActionableMessage(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_decryption_err.db")

	rawKeyA := make([]byte, 32)
	for i := range rawKeyA {
		rawKeyA[i] = byte(i + 1)
	}
	keyPathA := filepath.Join(tempDir, "masterA.key")
	if err := os.WriteFile(keyPathA, []byte(hex.EncodeToString(rawKeyA)), 0o600); err != nil {
		t.Fatal(err)
	}
	krA, err := security.LoadKeyring(keyPathA)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Create channel using Key A
	store1, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store1.WithKeyring(krA)
	secretBarkKey := "bark_super_secret_device_key_777"
	ch, err := store1.CreateBarkChannel(ctx, "Encrypted iPhone", secretBarkKey, nil)
	if err != nil {
		store1.Close()
		t.Fatal(err)
	}
	_ = store1.Close()

	// 2. Open server with Key B (wrong master key)
	rawKeyB := make([]byte, 32)
	for i := range rawKeyB {
		rawKeyB[i] = byte(255 - i)
	}
	keyPathB := filepath.Join(tempDir, "masterB.key")
	if err := os.WriteFile(keyPathB, []byte(hex.EncodeToString(rawKeyB)), 0o600); err != nil {
		t.Fatal(err)
	}
	krB, err := security.LoadKeyring(keyPathB)
	if err != nil {
		t.Fatal(err)
	}

	storeWrongKey, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer storeWrongKey.Close()
	storeWrongKey.WithKeyring(krB)

	cfg := config.Config{
		Timezone:           time.UTC,
		DevelopmentSubject: "owner",
		DatabasePath:       dbPath,
		MasterKeyFile:      keyPathB,
	}
	srv := New(cfg, storeWrongKey)

	csrf, cookie := getCSRF(srv)
	req := prepareAuthedPost("http://example.test/api/v1/notification-channels/"+ch.ID+"/test", nil, csrf, cookie)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on decryption failure, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to unmarshal JSON error response: %v", err)
	}

	if errResp.Code != storage.ErrCodeBarkKeyDecryptionFailed {
		t.Fatalf("expected code %q, got %q", storage.ErrCodeBarkKeyDecryptionFailed, errResp.Code)
	}
	if errResp.Error != storage.ErrMsgBarkKeyDecryptionFailed {
		t.Fatalf("expected actionable error message %q, got %q", storage.ErrMsgBarkKeyDecryptionFailed, errResp.Error)
	}

	// Invariant: response body must never leak secrets
	bodyStr := rec.Body.String()
	if strings.Contains(bodyStr, secretBarkKey) {
		t.Fatalf("response body leaked plaintext Bark device key!")
	}
	if strings.Contains(bodyStr, hex.EncodeToString(rawKeyA)) || strings.Contains(bodyStr, hex.EncodeToString(rawKeyB)) {
		t.Fatalf("response body leaked master key material!")
	}
}

func TestBarkTestEndpointPreservesGenericErrorForOtherFailures(t *testing.T) {
	srv, store := setupTestServerWithKeyring(t)
	defer store.Close()

	// Create a webhook endpoint
	ep, err := store.CreateEndpointWithSecret(context.Background(), "Broken Webhook", "https://example.com/hook")
	if err != nil {
		t.Fatal(err)
	}

	// Tamper the webhook secret envelope so decryption fails
	_, err = store.DB().ExecContext(context.Background(), `
		UPDATE endpoint_secrets
		SET envelope = X'7B7D'
		WHERE endpoint_id = ? AND status = 'ACTIVE'
	`, ep.ID)
	if err != nil {
		t.Fatal(err)
	}

	csrf, cookie := getCSRF(srv)
	req := prepareAuthedPost("http://example.test/api/v1/notification-channels/"+ep.ID+"/test", nil, csrf, cookie)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatal(err)
	}

	// Must preserve generic behavior for non-Bark / generic failures
	if errResp.Code == storage.ErrCodeBarkKeyDecryptionFailed {
		t.Fatalf("unexpected BARK_KEY_DECRYPTION_FAILED for webhook failure")
	}
	if !strings.HasPrefix(errResp.Error, "cannot decrypt channel target:") {
		t.Fatalf("expected generic 'cannot decrypt channel target:' prefix, got: %s", errResp.Error)
	}
}

func TestBarkAPIEndpointsNeverLeakPlaintext(t *testing.T) {
	srv, store := setupTestServerWithKeyring(t)
	defer store.Close()

	csrf, cookie := getCSRF(srv)

	secretDeviceKey := "device_key_top_secret_never_leak_007"
	barkBody, _ := json.Marshal(map[string]any{
		"provider":  "BARK",
		"name":      "Private Phone",
		"deviceKey": secretDeviceKey,
	})

	// 1. POST /api/v1/notification-channels
	reqCreate := prepareAuthedPost("http://example.test/api/v1/notification-channels", barkBody, csrf, cookie)
	recCreate := httptest.NewRecorder()
	srv.handler.ServeHTTP(recCreate, reqCreate)
	if recCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", recCreate.Code, recCreate.Body.String())
	}
	if strings.Contains(recCreate.Body.String(), secretDeviceKey) {
		t.Fatalf("device key leaked in create channel response!")
	}

	var createdCh struct {
		ID           string `json:"id"`
		HasDeviceKey bool   `json:"hasDeviceKey"`
		Secret       string `json:"secret"`
	}
	_ = json.Unmarshal(recCreate.Body.Bytes(), &createdCh)
	if !createdCh.HasDeviceKey || createdCh.Secret != "" {
		t.Fatalf("unexpected created channel fields: %+v", createdCh)
	}

	// 2. GET /api/v1/notification-channels
	reqList := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/notification-channels", nil)
	recList := httptest.NewRecorder()
	srv.handler.ServeHTTP(recList, reqList)
	if strings.Contains(recList.Body.String(), secretDeviceKey) {
		t.Fatalf("device key leaked in list channels response!")
	}

	// 3. POST /api/v1/notification-channels/{id}/rotate-secret
	newRotatedKey := "device_key_rotated_never_leak_888"
	rotateBody, _ := json.Marshal(map[string]string{
		"deviceKey": newRotatedKey,
	})
	reqRotate := prepareAuthedPost("http://example.test/api/v1/notification-channels/"+createdCh.ID+"/rotate-secret", rotateBody, csrf, cookie)
	recRotate := httptest.NewRecorder()
	srv.handler.ServeHTTP(recRotate, reqRotate)
	if recRotate.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", recRotate.Code, recRotate.Body.String())
	}
	if strings.Contains(recRotate.Body.String(), newRotatedKey) {
		t.Fatalf("device key leaked in rotate response!")
	}
}

type mockNotificationProviderReader struct {
	resp workerrpc.NotificationProvidersResponse
	err  error
}

func (m *mockNotificationProviderReader) NotificationProviderMetadata(ctx context.Context) (workerrpc.NotificationProvidersResponse, error) {
	return m.resp, m.err
}

func TestNotificationProviders_WorkerRPCDelegation_Configured(t *testing.T) {
	srv, store := setupTestServerWithKeyring(t)
	defer store.Close()

	reader := &mockNotificationProviderReader{
		resp: workerrpc.NotificationProvidersResponse{
			Providers: []workerrpc.NotificationProviderMetadata{
				{
					ID:          "WEBHOOK",
					Name:        "Webhook",
					Description: "Gửi JSON có chữ ký HMAC tới hệ thống khác.",
					Configured:  true,
					Status:      "configured",
				},
				{
					ID:          "BARK",
					Name:        "Bark (iOS)",
					Description: "Đẩy thông báo trực tiếp tới iPhone qua Bark self-host.",
					Configured:  true,
					PublicURL:   "https://bark.worker.site",
					Status:      "configured",
				},
			},
		},
	}
	srv.WithProviderReader(reader)

	req := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/notification-providers", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var provResp struct {
		Providers []struct {
			ID         string `json:"id"`
			Configured bool   `json:"configured"`
			PublicURL  string `json:"publicUrl"`
			Status     string `json:"status"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &provResp); err != nil {
		t.Fatal(err)
	}
	if len(provResp.Providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(provResp.Providers))
	}
	for _, p := range provResp.Providers {
		if p.ID == "BARK" {
			if !p.Configured {
				t.Fatal("expected Bark to be configured via worker RPC")
			}
			if p.PublicURL != "https://bark.worker.site" {
				t.Fatalf("expected publicUrl https://bark.worker.site, got %q", p.PublicURL)
			}
			if p.Status != "configured" {
				t.Fatalf("expected status configured, got %q", p.Status)
			}
		}
	}
}

func TestNotificationProviders_WorkerRPCDelegation_Unconfigured(t *testing.T) {
	srv, store := setupTestServerWithKeyring(t)
	defer store.Close()

	reader := &mockNotificationProviderReader{
		resp: workerrpc.NotificationProvidersResponse{
			Providers: []workerrpc.NotificationProviderMetadata{
				{
					ID:          "WEBHOOK",
					Name:        "Webhook",
					Description: "Gửi JSON có chữ ký HMAC tới hệ thống khác.",
					Configured:  true,
					Status:      "configured",
				},
				{
					ID:          "BARK",
					Name:        "Bark (iOS)",
					Description: "Đẩy thông báo trực tiếp tới iPhone qua Bark self-host.",
					Configured:  false,
					Status:      "unconfigured",
				},
			},
		},
	}
	srv.WithProviderReader(reader)

	req := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/notification-providers", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	var provResp struct {
		Providers []struct {
			ID         string `json:"id"`
			Configured bool   `json:"configured"`
			Status     string `json:"status"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &provResp); err != nil {
		t.Fatal(err)
	}
	for _, p := range provResp.Providers {
		if p.ID == "BARK" {
			if p.Configured {
				t.Fatal("expected Bark to be unconfigured via worker RPC")
			}
			if p.Status != "unconfigured" {
				t.Fatalf("expected status unconfigured, got %q", p.Status)
			}
		}
	}
}

func TestNotificationProviders_WorkerRPCDelegation_WorkerUnavailable(t *testing.T) {
	srv, store := setupTestServerWithKeyring(t)
	defer store.Close()

	reader := &mockNotificationProviderReader{
		err: errors.New("connection refused to worker"),
	}
	srv.WithProviderReader(reader)

	req := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/notification-providers", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when worker is down, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "worker unavailable") {
		t.Fatalf("expected error message to indicate worker unavailable, got %s", rec.Body.String())
	}
}

func TestNotificationProviders_MonolithDevFallback(t *testing.T) {
	srv, store := setupTestServerWithKeyring(t)
	defer store.Close()

	// In monolith dev, providerReader is nil, but barkSender is set
	barkSender := bark.NewSender(bark.Config{
		ServerURL: "http://127.0.0.1:8080",
		PublicURL: "https://bark.local.site",
	}, nil, "")
	srv.WithBarkSender(barkSender)

	req := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/notification-providers", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	var provResp struct {
		Providers []struct {
			ID         string `json:"id"`
			Configured bool   `json:"configured"`
			PublicURL  string `json:"publicUrl"`
			Status     string `json:"status"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &provResp); err != nil {
		t.Fatal(err)
	}
	for _, p := range provResp.Providers {
		if p.ID == "BARK" {
			if !p.Configured {
				t.Fatal("expected Bark to be configured via local barkSender")
			}
			if p.Status != "configured" {
				t.Fatalf("expected status configured, got %q", p.Status)
			}
		}
	}
}
