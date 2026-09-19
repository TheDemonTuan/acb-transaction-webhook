package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	if lastTTSReq.Cacheable {
		t.Errorf("expected Cacheable=false when rate/pitch specified")
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
}
