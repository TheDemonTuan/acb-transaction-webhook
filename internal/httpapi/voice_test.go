package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
)

func setupTestVoiceServer(t *testing.T) (*Server, *storage.Store, *httptest.Server, *http.Cookie, string) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "test_voice_api.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	mockTTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("X-TTS-Provider", "edge")
		w.Header().Set("X-TTS-Voice", "vi-VN-HoaiMyNeural")
		w.Header().Set("X-TTS-Fallback", "false")
		w.Header().Set("X-TTS-Cached", "false")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mock_mp3_data"))
	}))

	cfg := config.Config{
		DevelopmentSubject: "test-owner",
		Roles: config.RoleSubjects{
			Owners: map[string]struct{}{"test-owner": {}},
		},
	}

	server := New(cfg, store).WithTTSClient(ttsclient.New(mockTTS.URL, ""))

	// Get CSRF cookie and token
	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var tokenResp struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&tokenResp)

	return server, store, mockTTS, cookie, tokenResp.Token
}

func prepareAuthedRequest(req *http.Request, cookie *http.Cookie, token string) *http.Request {
	req.Header.Set("Origin", "http://example.test")
	req.Header.Set("X-CSRF-Token", token)
	req.AddCookie(cookie)
	return req
}

func TestVoiceSettingsEndpoints(t *testing.T) {
	server, store, mockTTS, cookie, token := setupTestVoiceServer(t)
	defer store.Close()
	defer mockTTS.Close()

	// 1. GET /api/v1/voice/settings
	req := httptest.NewRequest(http.MethodGet, "/api/v1/voice/settings", nil)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var settings storage.VoiceSettings
	if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	if settings.Revision != 1 || settings.ProviderMode != "ONLINE_AUTO" {
		t.Errorf("unexpected settings: %+v", settings)
	}

	// 2. PUT /api/v1/voice/settings
	settings.EdgeVoice = "vi-VN-NamMinhNeural"
	bodyBytes, _ := json.Marshal(settings)
	putReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPut, "http://example.test/api/v1/voice/settings", bytes.NewReader(bodyBytes)),
		cookie,
		token,
	)
	putW := httptest.NewRecorder()
	server.Handler().ServeHTTP(putW, putReq)

	if putW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", putW.Code, putW.Body.String())
	}
	var updated storage.VoiceSettings
	_ = json.Unmarshal(putW.Body.Bytes(), &updated)
	if updated.Revision != 2 || updated.EdgeVoice != "vi-VN-NamMinhNeural" {
		t.Errorf("unexpected updated settings: %+v", updated)
	}
}

func TestVoiceTestEndpoint(t *testing.T) {
	server, store, mockTTS, cookie, token := setupTestVoiceServer(t)
	defer store.Close()
	defer mockTTS.Close()

	req := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/test", bytes.NewReader([]byte(`{"voiceId":"vi-VN-HoaiMyNeural"}`))),
		cookie,
		token,
	)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("expected Content-Type audio/mpeg, got %s", w.Header().Get("Content-Type"))
	}
	if w.Body.String() != "mock_mp3_data" {
		t.Errorf("unexpected audio body: %q", w.Body.String())
	}
}

func TestPublicVoiceTestEndpoint(t *testing.T) {
	server, store, mockTTS, _, _ := setupTestVoiceServer(t)
	defer store.Close()
	defer mockTTS.Close()

	// Public endpoint requires NO cookie and NO CSRF token
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/public/v1/voice/test",
		bytes.NewReader([]byte(`{"voiceId":"vi-VN-NamMinhNeural","rate":1.25,"pitch":1.1}`)),
	)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("expected Content-Type audio/mpeg, got %s", w.Header().Get("Content-Type"))
	}
	if w.Body.String() != "mock_mp3_data" {
		t.Errorf("unexpected audio body: %q", w.Body.String())
	}
}

func TestVoiceStreamingAndTelemetry(t *testing.T) {
	ctx := context.Background()
	server, store, mockTTS, cookie, token := setupTestVoiceServer(t)
	defer store.Close()
	defer mockTTS.Close()

	connID := "conn_stream_test"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	rtRes, err := store.IngestTransactionsBatchWithSource(ctx, connID, 1, "123456", []storage.BatchTransactionItem{
		{
			Number:        "TXN_STREAM_1",
			Credit:        500000,
			Debit:         0,
			TransactionAt: "12/09/2026 10:00:00",
			EffectiveAt:   "12/09/2026",
			Description:   "Payment 500k",
		},
	}, false, "REALTIME")
	if err != nil || len(rtRes.NewEvents) == 0 {
		t.Fatalf("ingest realtime: %v", err)
	}
	rtTxnID := rtRes.NewEvents[0].TransactionID

	// 1. GET /api/v1/voice/transactions/{id}/stream
	reqGet := prepareAuthedRequest(
		httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/voice/transactions/"+rtTxnID+"/stream?rate=1.25&voiceId=vi-VN-NamMinhNeural", nil),
		cookie,
		token,
	)
	wGet := httptest.NewRecorder()
	server.Handler().ServeHTTP(wGet, reqGet)
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200 for GET stream, got %d: %s", wGet.Code, wGet.Body.String())
	}
	if wGet.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("expected Content-Type audio/mpeg, got %s", wGet.Header().Get("Content-Type"))
	}
	firstByteHeader := wGet.Header().Get("X-TTS-First-Byte-Ms")
	if firstByteHeader == "" {
		t.Errorf("expected X-TTS-First-Byte-Ms header to be present")
	}
	serverTiming := wGet.Header().Get("Server-Timing")
	if !strings.Contains(serverTiming, "tts_fb;dur=") {
		t.Errorf("expected Server-Timing header to contain tts_fb;dur=, got %q", serverTiming)
	}
	if wGet.Body.String() != "mock_mp3_data" {
		t.Errorf("unexpected body: %q", wGet.Body.String())
	}

	// 2. GET /api/public/v1/voice/transactions/{id}/stream
	pubReq := httptest.NewRequest(
		http.MethodGet,
		"/api/public/v1/voice/transactions/"+rtTxnID+"/stream",
		nil,
	)
	pubW := httptest.NewRecorder()
	server.Handler().ServeHTTP(pubW, pubReq)
	if pubW.Code != http.StatusOK {
		t.Fatalf("expected 200 for public GET stream, got %d: %s", pubW.Code, pubW.Body.String())
	}
	if pubW.Header().Get("X-TTS-First-Byte-Ms") == "" {
		t.Errorf("expected public stream X-TTS-First-Byte-Ms header")
	}

	// 3. GET /api/public/v1/voice/test/stream
	testReq := httptest.NewRequest(
		http.MethodGet,
		"/api/public/v1/voice/test/stream",
		nil,
	)
	testW := httptest.NewRecorder()
	server.Handler().ServeHTTP(testW, testReq)
	if testW.Code != http.StatusOK {
		t.Fatalf("expected 200 for public test stream, got %d: %s", testW.Code, testW.Body.String())
	}
	if testW.Header().Get("X-TTS-First-Byte-Ms") == "" {
		t.Errorf("expected public test stream X-TTS-First-Byte-Ms header")
	}
}

func TestSynthesizeTransactionAudioPolicies(t *testing.T) {
	ctx := context.Background()
	server, store, mockTTS, cookie, token := setupTestVoiceServer(t)
	defer store.Close()
	defer mockTTS.Close()

	connID := "conn_voice_test"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	// Ingest 1 REALTIME credit transaction
	rtRes, err := store.IngestTransactionsBatchWithSource(ctx, connID, 1, "123456", []storage.BatchTransactionItem{
		{
			Number:        "TXN_RT_1",
			Credit:        500000,
			Debit:         0,
			TransactionAt: "12/09/2026 10:00:00",
			EffectiveAt:   "12/09/2026",
			Description:   "Payment 500k",
		},
	}, false, "REALTIME")
	if err != nil || len(rtRes.NewEvents) == 0 {
		t.Fatalf("ingest realtime: %v", err)
	}
	rtTxnID := rtRes.NewEvents[0].TransactionID

	// Ingest 1 CATCH_UP credit transaction
	cuRes, err := store.IngestTransactionsBatchWithSource(ctx, connID, 1, "123456", []storage.BatchTransactionItem{
		{
			Number:        "TXN_CU_1",
			Credit:        300000,
			Debit:         0,
			TransactionAt: "12/09/2026 09:00:00",
			EffectiveAt:   "12/09/2026",
			Description:   "Catchup 300k",
		},
	}, false, "CATCH_UP")
	if err != nil || len(cuRes.NewEvents) == 0 {
		t.Fatalf("ingest catchup: %v", err)
	}
	cuTxnID := cuRes.NewEvents[0].TransactionID

	// 1. Unknown transaction -> 404
	req404 := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/transactions/txn_unknown", nil),
		cookie,
		token,
	)
	w404 := httptest.NewRecorder()
	server.Handler().ServeHTTP(w404, req404)
	if w404.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown transaction, got %d", w404.Code)
	}

	// 2. CATCH_UP transaction -> 422 voice_source_not_realtime for automatic announcement
	reqCU := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/transactions/"+cuTxnID, nil),
		cookie,
		token,
	)
	wCU := httptest.NewRecorder()
	server.Handler().ServeHTTP(wCU, reqCU)
	if wCU.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for non-realtime transaction, got %d", wCU.Code)
	}

	// 3. REALTIME transaction -> 200 OK with audio
	reqRT := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/transactions/"+rtTxnID, nil),
		cookie,
		token,
	)
	wRT := httptest.NewRecorder()
	server.Handler().ServeHTTP(wRT, reqRT)
	if wRT.Code != http.StatusOK {
		t.Fatalf("expected 200 for realtime transaction, got %d: %s", wRT.Code, wRT.Body.String())
	}
	if wRT.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("expected Content-Type audio/mpeg, got %s", wRT.Header().Get("Content-Type"))
	}
	if wRT.Header().Get("X-TTS-Provider") != "edge" {
		t.Errorf("expected X-TTS-Provider edge, got %s", wRT.Header().Get("X-TTS-Provider"))
	}

	// 4. Replay CATCH_UP transaction -> 200 OK (manual replay allowed!)
	reqReplay := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/transactions/"+cuTxnID+"/replay", nil),
		cookie,
		token,
	)
	wReplay := httptest.NewRecorder()
	server.Handler().ServeHTTP(wReplay, reqReplay)
	if wReplay.Code != http.StatusOK {
		t.Fatalf("expected 200 for replay transaction, got %d: %s", wReplay.Code, wReplay.Body.String())
	}
}

func TestSynthesizeSummaryAudio(t *testing.T) {
	ctx := context.Background()
	server, store, mockTTS, cookie, token := setupTestVoiceServer(t)
	defer store.Close()
	defer mockTTS.Close()

	connID := "conn_summary_test"
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID); err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	res, err := store.IngestTransactionsBatchWithSource(ctx, connID, 1, "123456", []storage.BatchTransactionItem{
		{Number: "TXN_SUM_1", Credit: 100000, Debit: 0, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Desc 1"},
		{Number: "TXN_SUM_2", Credit: 200000, Debit: 0, TransactionAt: "12/09/2026 10:00:01", EffectiveAt: "12/09/2026", Description: "Desc 2"},
	}, false, "REALTIME")
	if err != nil || len(res.NewEvents) < 2 {
		t.Fatalf("ingest: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"transactionIds": []string{res.NewEvents[0].TransactionID, res.NewEvents[1].TransactionID},
	})
	req := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/transactions/summary", bytes.NewReader(body)),
		cookie,
		token,
	)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("expected Content-Type audio/mpeg, got %s", w.Header().Get("Content-Type"))
	}
}

func TestVoiceRateAndPitchPropagation(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "test_rate_pitch.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	var lastTTSReq ttsclient.SynthesizeRequest
	mockTTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&lastTTSReq)
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("X-TTS-Provider", "edge")
		w.Header().Set("X-TTS-Voice", lastTTSReq.Voice)
		w.Header().Set("X-TTS-Fallback", "false")
		w.Header().Set("X-TTS-Cached", "false")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mock_mp3_data"))
	}))
	defer mockTTS.Close()

	cfg := config.Config{
		DevelopmentSubject: "test-owner",
		Roles: config.RoleSubjects{
			Owners: map[string]struct{}{"test-owner": {}},
		},
	}
	server := New(cfg, store).WithTTSClient(ttsclient.New(mockTTS.URL, ""))

	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var tokenResp struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&tokenResp)
	token := tokenResp.Token

	// 1. Test endpoint with numeric rate 1.25 -> "+25%"
	reqBody, _ := json.Marshal(map[string]any{
		"voiceId": "vi-VN-HoaiMyNeural",
		"rate":    1.25,
		"pitch":   1.1,
	})
	testReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/test", bytes.NewReader(reqBody)),
		cookie,
		token,
	)
	testW := httptest.NewRecorder()
	server.Handler().ServeHTTP(testW, testReq)
	if testW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", testW.Code, testW.Body.String())
	}
	if lastTTSReq.Rate != "+25%" {
		t.Errorf("expected rate +25%%, got %q", lastTTSReq.Rate)
	}
	if lastTTSReq.Pitch != "+5Hz" {
		t.Errorf("expected pitch +5Hz, got %q", lastTTSReq.Pitch)
	}
	if !lastTTSReq.Cacheable {
		t.Errorf("expected Cacheable=true when rate/pitch specified")
	}

	// 2. Synthesize transaction audio with string rate "+30%"
	connID := "conn_rate_pitch_test"
	_, _ = store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID)

	rtRes, err := store.IngestTransactionsBatchWithSource(ctx, connID, 1, "123456", []storage.BatchTransactionItem{
		{Number: "TXN_RATE_1", Credit: 200000, Debit: 0, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Rate test"},
	}, false, "REALTIME")
	if err != nil || len(rtRes.NewEvents) == 0 {
		t.Fatalf("ingest: %v", err)
	}
	txnID := rtRes.NewEvents[0].TransactionID

	txBody, _ := json.Marshal(map[string]any{
		"rate":  "+30%",
		"pitch": "+0Hz",
	})
	txReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/transactions/"+txnID, bytes.NewReader(txBody)),
		cookie,
		token,
	)
	txW := httptest.NewRecorder()
	server.Handler().ServeHTTP(txW, txReq)
	if txW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", txW.Code, txW.Body.String())
	}
	if lastTTSReq.Rate != "+30%" {
		t.Errorf("expected rate +30%%, got %q", lastTTSReq.Rate)
	}
	if lastTTSReq.Pitch != "+0Hz" {
		t.Errorf("expected pitch +0Hz, got %q", lastTTSReq.Pitch)
	}
	if !lastTTSReq.Cacheable {
		t.Errorf("expected transaction audio Cacheable=true when rate/pitch specified")
	}

	// 3. Synthesize transaction audio with slowed rate 0.8 -> "-20%"
	txBodySlow, _ := json.Marshal(map[string]any{
		"rate": 0.8,
	})
	txReqSlow := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/transactions/"+txnID, bytes.NewReader(txBodySlow)),
		cookie,
		token,
	)
	txWSlow := httptest.NewRecorder()
	server.Handler().ServeHTTP(txWSlow, txReqSlow)
	if txWSlow.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", txWSlow.Code, txWSlow.Body.String())
	}
	if lastTTSReq.Rate != "-20%" {
		t.Errorf("expected rate -20%%, got %q", lastTTSReq.Rate)
	}
	if !lastTTSReq.Cacheable {
		t.Errorf("expected transaction audio Cacheable=true when slowed rate specified")
	}
}

func TestVoiceTemplateCustomization(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "test_template.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	var lastTTSReq ttsclient.SynthesizeRequest
	mockTTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&lastTTSReq)
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("X-TTS-Provider", "edge")
		w.Header().Set("X-TTS-Voice", lastTTSReq.Voice)
		w.Header().Set("X-TTS-Fallback", "false")
		w.Header().Set("X-TTS-Cached", "false")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mock_mp3_data"))
	}))
	defer mockTTS.Close()

	cfg := config.Config{
		DevelopmentSubject: "test-owner",
		Roles: config.RoleSubjects{
			Owners: map[string]struct{}{"test-owner": {}},
		},
	}
	server := New(cfg, store).WithTTSClient(ttsclient.New(mockTTS.URL, ""))

	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var tokenResp struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&tokenResp)
	token := tokenResp.Token

	// 1. /voice/test with custom template
	testBody, _ := json.Marshal(map[string]any{
		"template": "Cảm ơn quý khách đã gửi {amount}.",
	})
	testReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/test", bytes.NewReader(testBody)),
		cookie,
		token,
	)
	testW := httptest.NewRecorder()
	server.Handler().ServeHTTP(testW, testReq)
	if testW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", testW.Code, testW.Body.String())
	}
	expectedTest := "Cảm ơn quý khách đã gửi năm trăm nghìn đồng."
	if lastTTSReq.Text != expectedTest {
		t.Errorf("expected test text %q, got %q", expectedTest, lastTTSReq.Text)
	}

	// 2. /voice/transactions/{id} with custom template
	connID := "conn_tpl_test"
	_, _ = store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID)

	rtRes, err := store.IngestTransactionsBatchWithSource(ctx, connID, 1, "123456", []storage.BatchTransactionItem{
		{Number: "TXN_TPL_1", Credit: 1000000, Debit: 0, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Shop Pay"},
	}, false, "REALTIME")
	if err != nil || len(rtRes.NewEvents) == 0 {
		t.Fatalf("ingest: %v", err)
	}
	txnID := rtRes.NewEvents[0].TransactionID

	txBody, _ := json.Marshal(map[string]any{
		"template": "Đã nhận {amount_raw} VND ({amount}).",
	})
	txReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/transactions/"+txnID, bytes.NewReader(txBody)),
		cookie,
		token,
	)
	txW := httptest.NewRecorder()
	server.Handler().ServeHTTP(txW, txReq)
	if txW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", txW.Code, txW.Body.String())
	}
	expectedTx := "Đã nhận 1000000 VND (một triệu đồng)."
	if lastTTSReq.Text != expectedTx {
		t.Errorf("expected transaction text %q, got %q", expectedTx, lastTTSReq.Text)
	}

	// 3. /voice/transactions/{id}/replay with custom template
	replayBody, _ := json.Marshal(map[string]any{
		"template": "Phát lại: nhận {amount}.",
	})
	replayReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/transactions/"+txnID+"/replay", bytes.NewReader(replayBody)),
		cookie,
		token,
	)
	replayW := httptest.NewRecorder()
	server.Handler().ServeHTTP(replayW, replayReq)
	if replayW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", replayW.Code, replayW.Body.String())
	}
	expectedReplay := "Phát lại: nhận một triệu đồng."
	if lastTTSReq.Text != expectedReplay {
		t.Errorf("expected replay text %q, got %q", expectedReplay, lastTTSReq.Text)
	}
}

func TestPublicVoiceSynthesisFallback(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "test_public_voice.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	var lastTTSReq ttsclient.SynthesizeRequest
	mockTTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&lastTTSReq)
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("X-TTS-Provider", "edge")
		w.Header().Set("X-TTS-Voice", lastTTSReq.Voice)
		w.Header().Set("X-TTS-Fallback", "false")
		w.Header().Set("X-TTS-Cached", "false")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mock_mp3_data"))
	}))
	defer mockTTS.Close()

	cfg := config.Config{
		DevelopmentSubject: "test-owner",
	}
	server := New(cfg, store).WithTTSClient(ttsclient.New(mockTTS.URL, ""))

	connID := "conn_pub_voice"
	_, _ = store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID)

	// 1. Fresh REALTIME transaction
	rtRes, err := store.IngestTransactionsBatchWithSource(ctx, connID, 1, "123456", []storage.BatchTransactionItem{
		{Number: "TXN_PUB_1", Credit: 250000, Debit: 0, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Tip"},
	}, false, "REALTIME")
	if err != nil || len(rtRes.NewEvents) == 0 {
		t.Fatalf("ingest: %v", err)
	}
	freshTxnID := rtRes.NewEvents[0].TransactionID

	// Call unauthenticated public endpoint
	pubBody, _ := json.Marshal(map[string]any{
		"rate": 1.25,
	})
	pubReq := httptest.NewRequest(http.MethodPost, "/api/public/v1/voice/transactions/"+freshTxnID, bytes.NewReader(pubBody))
	pubW := httptest.NewRecorder()
	server.Handler().ServeHTTP(pubW, pubReq)

	if pubW.Code != http.StatusOK {
		t.Fatalf("expected 200 from public voice endpoint, got %d: %s", pubW.Code, pubW.Body.String())
	}
	if pubW.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("expected Content-Type audio/mpeg, got %s", pubW.Header().Get("Content-Type"))
	}
	expectedDefault := "Đa tạ quý khách vì hai trăm năm mươi nghìn đồng."
	if lastTTSReq.Text != expectedDefault {
		t.Errorf("expected default text %q, got %q", expectedDefault, lastTTSReq.Text)
	}
	if !lastTTSReq.Cacheable {
		t.Errorf("expected Cacheable=true on public voice synthesis")
	}

	// 2. Anti-abuse: arbitrary template without amount placeholder safely falls back to default template
	abuseBody, _ := json.Marshal(map[string]any{
		"template": "Hacker text with no amount token",
	})
	abuseReq := httptest.NewRequest(http.MethodPost, "/api/public/v1/voice/transactions/"+freshTxnID, bytes.NewReader(abuseBody))
	abuseW := httptest.NewRecorder()
	server.Handler().ServeHTTP(abuseW, abuseReq)
	if abuseW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", abuseW.Code)
	}
	if lastTTSReq.Text != expectedDefault {
		t.Errorf("unsafe template must fallback to default template, got %q", lastTTSReq.Text)
	}

	// 3. Non-realtime source rejected with 422
	cuRes, err := store.IngestTransactionsBatchWithSource(ctx, connID, 1, "123456", []storage.BatchTransactionItem{
		{Number: "TXN_PUB_CU", Credit: 500000, Debit: 0, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Historical"},
	}, false, "CATCH_UP")
	if err != nil || len(cuRes.NewEvents) == 0 {
		t.Fatalf("ingest catchup: %v", err)
	}
	cuTxnID := cuRes.NewEvents[0].TransactionID

	cuReq := httptest.NewRequest(http.MethodPost, "/api/public/v1/voice/transactions/"+cuTxnID, nil)
	cuW := httptest.NewRecorder()
	server.Handler().ServeHTTP(cuW, cuReq)
	if cuW.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for non-realtime, got %d", cuW.Code)
	}

	// 4. Nonexistent transaction -> 404
	badReq := httptest.NewRequest(http.MethodPost, "/api/public/v1/voice/transactions/nonexistent", nil)
	badW := httptest.NewRecorder()
	server.Handler().ServeHTTP(badW, badReq)
	if badW.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing transaction, got %d", badW.Code)
	}
}

func TestVoiceRateAndPitchClamping(t *testing.T) {
	// Rate clamping: -50% to +100%
	if r := formatTTSRate("+300%"); r != "+100%" {
		t.Errorf("expected +100%%, got %q", r)
	}
	if r := formatTTSRate("-90%"); r != "-50%" {
		t.Errorf("expected -50%%, got %q", r)
	}
	if r := formatTTSRate(3.0); r != "+100%" {
		t.Errorf("expected +100%%, got %q", r)
	}
	if r := formatTTSRate(0.2); r != "-50%" {
		t.Errorf("expected -50%%, got %q", r)
	}
	if r := formatTTSRate(1.25); r != "+25%" {
		t.Errorf("expected +25%%, got %q", r)
	}

	// Pitch clamping: -50Hz/50% to +50Hz/50%
	if p := formatTTSPitch("+100Hz"); p != "+50Hz" {
		t.Errorf("expected +50Hz, got %q", p)
	}
	if p := formatTTSPitch("-90Hz"); p != "-50Hz" {
		t.Errorf("expected -50Hz, got %q", p)
	}
	if p := formatTTSPitch("+100%"); p != "+50%" {
		t.Errorf("expected +50%%, got %q", p)
	}
	if p := formatTTSPitch("-80%"); p != "-50%" {
		t.Errorf("expected -50%%, got %q", p)
	}
	if p := formatTTSPitch(2.5); p != "+50Hz" {
		t.Errorf("expected +50Hz, got %q", p)
	}
}

func TestVoiceSummaryStreamGETWithQueryParams(t *testing.T) {
	ctx := context.Background()
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "test_summary_stream.db")

	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	var lastTTSReq ttsclient.SynthesizeRequest
	mockTTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&lastTTSReq)
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("X-TTS-Provider", "edge")
		w.Header().Set("X-TTS-Voice", lastTTSReq.Voice)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("summary_audio_stream"))
	}))
	defer mockTTS.Close()

	cfg := config.Config{
		DevelopmentSubject: "test-owner",
		Roles: config.RoleSubjects{
			Owners: map[string]struct{}{"test-owner": {}},
		},
	}
	server := New(cfg, store).WithTTSClient(ttsclient.New(mockTTS.URL, ""))

	csrfReq := httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/csrf", nil)
	csrfRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(csrfRec, csrfReq)
	cookie := csrfRec.Result().Cookies()[0]
	var tokenResp struct{ Token string }
	_ = json.NewDecoder(csrfRec.Result().Body).Decode(&tokenResp)
	token := tokenResp.Token

	connID := "conn_summary_test"
	_, _ = store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID)

	rtRes, err := store.IngestTransactionsBatchWithSource(ctx, connID, 1, "123456", []storage.BatchTransactionItem{
		{Number: "TXN_SUM_1", Credit: 100000, Debit: 0, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Order 1"},
		{Number: "TXN_SUM_2", Credit: 200000, Debit: 0, TransactionAt: "12/09/2026 10:01:00", EffectiveAt: "12/09/2026", Description: "Order 2"},
	}, false, "REALTIME")
	if err != nil || len(rtRes.NewEvents) < 2 {
		t.Fatalf("ingest: %v", err)
	}
	txID1 := rtRes.NewEvents[0].TransactionID
	txID2 := rtRes.NewEvents[1].TransactionID

	// GET /voice/transactions/summary/stream with query parameters
	q := url.Values{}
	q.Set("transactionIds", fmt.Sprintf("%s,%s", txID1, txID2))
	q.Set("rate", "+200%")
	q.Set("pitch", "+80Hz")
	summaryURL := "http://example.test/api/v1/voice/transactions/summary/stream?" + q.Encode()
	req := prepareAuthedRequest(
		httptest.NewRequest(http.MethodGet, summaryURL, nil),
		cookie,
		token,
	)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from summary stream, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("expected Content-Type audio/mpeg, got %s", w.Header().Get("Content-Type"))
	}
	// Verify rate and pitch clamped
	if lastTTSReq.Rate != "+100%" {
		t.Errorf("expected clamped rate +100%%, got %q", lastTTSReq.Rate)
	}
	if lastTTSReq.Pitch != "+50Hz" {
		t.Errorf("expected clamped pitch +50Hz, got %q", lastTTSReq.Pitch)
	}

	// GET with txIds alias
	summaryURL2 := fmt.Sprintf("http://example.test/api/v1/voice/transactions/summary/stream?txIds=%s,%s", txID1, txID2)
	req2 := prepareAuthedRequest(
		httptest.NewRequest(http.MethodGet, summaryURL2, nil),
		cookie,
		token,
	)
	w2 := httptest.NewRecorder()
	server.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 from txIds summary stream, got %d: %s", w2.Code, w2.Body.String())
	}
}
