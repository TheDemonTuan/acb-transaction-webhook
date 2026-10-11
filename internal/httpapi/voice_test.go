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
	"github.com/thedemontuan/acb-transaction-webhook/internal/voicecopy"
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
	var settings storage.VoiceProfile
	if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	if settings.Revision != 1 || settings.ProviderMode != "EDGE_ONLY" {
		t.Errorf("unexpected settings: %+v", settings)
	}

	// Also verify GET /api/v1/voice/profile and GET /api/public/v1/voice/profile
	profReq := httptest.NewRequest(http.MethodGet, "/api/v1/voice/profile", nil)
	profW := httptest.NewRecorder()
	server.Handler().ServeHTTP(profW, profReq)
	if profW.Code != http.StatusOK {
		t.Fatalf("expected 200 for voice profile, got %d", profW.Code)
	}
	var prof storage.VoiceProfile
	_ = json.Unmarshal(profW.Body.Bytes(), &prof)
	if prof.SynthesisKey == "" || prof.CatalogVersion != "v1" {
		t.Fatalf("invalid profile payload: %+v", prof)
	}

	pubProfReq := httptest.NewRequest(http.MethodGet, "/api/public/v1/voice/profile", nil)
	pubProfW := httptest.NewRecorder()
	server.Handler().ServeHTTP(pubProfW, pubProfReq)
	if pubProfW.Code != http.StatusOK {
		t.Fatalf("expected 200 for public voice profile, got %d", pubProfW.Code)
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
	var updated storage.VoiceProfile
	_ = json.Unmarshal(putW.Body.Bytes(), &updated)
	if updated.Revision != 2 || updated.EdgeVoice != "vi-VN-NamMinhNeural" {
		t.Errorf("unexpected updated settings: %+v", updated)
	}
}

func TestVoiceTestEndpoint(t *testing.T) {
	server, store, mockTTS, cookie, token := setupTestVoiceServer(t)
	defer store.Close()
	defer mockTTS.Close()

	// 1. Client override rejected with 400 VOICE_OVERRIDE_NOT_ALLOWED
	badReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/test", bytes.NewReader([]byte(`{"voiceId":"vi-VN-HoaiMyNeural"}`))),
		cookie,
		token,
	)
	badW := httptest.NewRecorder()
	server.Handler().ServeHTTP(badW, badReq)
	if badW.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for override, got %d: %s", badW.Code, badW.Body.String())
	}

	// 2. Stale revision rejected with 409 VOICE_PROFILE_CHANGED
	staleReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/test", bytes.NewReader([]byte(`{"profileRevision":999}`))),
		cookie,
		token,
	)
	staleW := httptest.NewRecorder()
	server.Handler().ServeHTTP(staleW, staleReq)
	if staleW.Code != http.StatusConflict {
		t.Fatalf("expected 409 for stale revision, got %d: %s", staleW.Code, staleW.Body.String())
	}

	// 3. Valid test request without overrides succeeds with 200
	req := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/test", bytes.NewReader([]byte(`{"profileRevision":1}`))),
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
	if w.Header().Get("X-Voice-Revision") != "1" {
		t.Errorf("expected X-Voice-Revision 1, got %s", w.Header().Get("X-Voice-Revision"))
	}
	if w.Header().Get("X-Voice-Synthesis-Key") == "" {
		t.Errorf("expected X-Voice-Synthesis-Key header")
	}
	if w.Body.String() != "mock_mp3_data" {
		t.Errorf("unexpected audio body: %q", w.Body.String())
	}
}

func TestPublicVoiceTestEndpoint(t *testing.T) {
	server, store, mockTTS, _, _ := setupTestVoiceServer(t)
	defer store.Close()
	defer mockTTS.Close()

	// 1. Override rejected with 400
	badReq := httptest.NewRequest(
		http.MethodPost,
		"/api/public/v1/voice/test",
		bytes.NewReader([]byte(`{"voiceId":"vi-VN-NamMinhNeural","rate":1.25,"pitch":1.1}`)),
	)
	badW := httptest.NewRecorder()
	server.Handler().ServeHTTP(badW, badReq)
	if badW.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for override, got %d", badW.Code)
	}

	// 2. Public test without overrides succeeds with 200
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/public/v1/voice/test",
		bytes.NewReader([]byte(`{}`)),
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

	rtRes, err := seedHistoricalTransactions(ctx, store, connID, "123456", []historicalTransactionFixture{
		{
			Number:        "TXN_STREAM_1",
			Credit:        500000,
			Debit:         0,
			TransactionAt: "12/09/2026 10:00:00",
			EffectiveAt:   "12/09/2026",
			Description:   "Payment 500k",
		},
	}, "REALTIME")
	if err != nil || len(rtRes.NewEvents) == 0 {
		t.Fatalf("ingest realtime: %v", err)
	}
	rtTxnID := rtRes.NewEvents[0].TransactionID

	// 1a. Override query param rejected with 400
	reqBad := prepareAuthedRequest(
		httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/voice/transactions/"+rtTxnID+"/stream?rate=1.25&voiceId=vi-VN-NamMinhNeural", nil),
		cookie,
		token,
	)
	wBad := httptest.NewRecorder()
	server.Handler().ServeHTTP(wBad, reqBad)
	if wBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for override, got %d", wBad.Code)
	}

	// 1b. GET /api/v1/voice/transactions/{id}/stream without overrides
	reqGet := prepareAuthedRequest(
		httptest.NewRequest(http.MethodGet, "http://example.test/api/v1/voice/transactions/"+rtTxnID+"/stream", nil),
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
	rtRes, err := seedHistoricalTransactions(ctx, store, connID, "123456", []historicalTransactionFixture{
		{
			Number:        "TXN_RT_1",
			Credit:        500000,
			Debit:         0,
			TransactionAt: "12/09/2026 10:00:00",
			EffectiveAt:   "12/09/2026",
			Description:   "Payment 500k",
		},
	}, "REALTIME")
	if err != nil || len(rtRes.NewEvents) == 0 {
		t.Fatalf("ingest realtime: %v", err)
	}
	rtTxnID := rtRes.NewEvents[0].TransactionID

	// Ingest 1 CATCH_UP credit transaction
	cuRes, err := seedHistoricalTransactions(ctx, store, connID, "123456", []historicalTransactionFixture{
		{
			Number:        "TXN_CU_1",
			Credit:        300000,
			Debit:         0,
			TransactionAt: "12/09/2026 09:00:00",
			EffectiveAt:   "12/09/2026",
			Description:   "Catchup 300k",
		},
	}, "CATCH_UP")
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

	res, err := seedHistoricalTransactions(ctx, store, connID, "123456", []historicalTransactionFixture{
		{Number: "TXN_SUM_1", Credit: 100000, Debit: 0, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Desc 1"},
		{Number: "TXN_SUM_2", Credit: 200000, Debit: 0, TransactionAt: "12/09/2026 10:00:01", EffectiveAt: "12/09/2026", Description: "Desc 2"},
	}, "REALTIME")
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

	// 1. Client override rejected with 400 VOICE_OVERRIDE_NOT_ALLOWED
	badReqBody, _ := json.Marshal(map[string]any{
		"rate":  1.25,
		"pitch": 1.1,
	})
	badTestReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/test", bytes.NewReader(badReqBody)),
		cookie,
		token,
	)
	badTestW := httptest.NewRecorder()
	server.Handler().ServeHTTP(badTestW, badTestReq)
	if badTestW.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for override, got %d: %s", badTestW.Code, badTestW.Body.String())
	}

	// 2. Configure profile with rate 1.25 and pitchHz 5
	prof, _ := store.GetVoiceProfile(ctx)
	_, err = store.SaveVoiceProfile(ctx, storage.VoiceSettingsInput{
		Revision:     prof.Revision,
		ProviderMode: "EDGE_ONLY",
		EdgeVoice:    "vi-VN-HoaiMyNeural",
		Rate:         1.25,
		PitchHz:      5,
		AmountBands:  prof.AmountBands,
		ChimeEnabled: true,
	})
	if err != nil {
		t.Fatalf("save profile: %v", err)
	}

	testReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/test", bytes.NewReader([]byte(`{}`))),
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

	// 3. Synthesize transaction audio with slowed profile rate 0.8 -> "-20%"
	connID := "conn_rate_pitch_test"
	_, _ = store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID)

	rtRes, err := seedHistoricalTransactions(ctx, store, connID, "123456", []historicalTransactionFixture{
		{Number: "TXN_RATE_1", Credit: 200000, Debit: 0, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Rate test"},
	}, "REALTIME")
	if err != nil || len(rtRes.NewEvents) == 0 {
		t.Fatalf("ingest: %v", err)
	}
	txnID := rtRes.NewEvents[0].TransactionID

	prof, _ = store.GetVoiceProfile(ctx)
	_, err = store.SaveVoiceProfile(ctx, storage.VoiceSettingsInput{
		Revision:     prof.Revision,
		ProviderMode: "EDGE_ONLY",
		EdgeVoice:    "vi-VN-HoaiMyNeural",
		Rate:         0.80,
		PitchHz:      0,
		AmountBands:  prof.AmountBands,
		ChimeEnabled: true,
	})
	if err != nil {
		t.Fatalf("save slow profile: %v", err)
	}

	txReqSlow := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/transactions/"+txnID, bytes.NewReader([]byte(`{}`))),
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
	if lastTTSReq.Pitch != "+0Hz" {
		t.Errorf("expected pitch +0Hz, got %q", lastTTSReq.Pitch)
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

	// 1. Template override in body rejected with 400 VOICE_OVERRIDE_NOT_ALLOWED
	testBody, _ := json.Marshal(map[string]any{
		"template": "Cảm ơn quý khách đã gửi {amount}.",
	})
	badTestReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/test", bytes.NewReader(testBody)),
		cookie,
		token,
	)
	badTestW := httptest.NewRecorder()
	server.Handler().ServeHTTP(badTestW, badTestReq)
	if badTestW.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for override, got %d: %s", badTestW.Code, badTestW.Body.String())
	}

	// 2. Configure custom template in profile
	prof, _ := store.GetVoiceProfile(ctx)
	for i := range prof.AmountBands {
		prof.AmountBands[i].StyleID = "custom"
	}
	_, err = store.SaveVoiceProfile(ctx, storage.VoiceSettingsInput{
		Revision:       prof.Revision,
		ProviderMode:   "EDGE_ONLY",
		EdgeVoice:      prof.EdgeVoice,
		Rate:           prof.Rate,
		PitchHz:        prof.PitchHz,
		AmountBands:    prof.AmountBands,
		CustomTemplate: "Cảm ơn quý khách đã gửi {amount}.",
		FunEnabled:     true,
		ChimeEnabled:   true,
	})
	if err != nil {
		t.Fatalf("save custom template: %v", err)
	}

	testReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/test", bytes.NewReader([]byte(`{}`))),
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

	// 3. /voice/transactions/{id} with configured custom template
	connID := "conn_tpl_test"
	_, _ = store.DB().ExecContext(ctx, `
		INSERT INTO connections(id, state, generation, account_masked, created_at, updated_at)
		VALUES(?, 'MONITORING', 1, '123456', '2026-09-12T00:00:00Z', '2026-09-12T00:00:00Z')
	`, connID)

	rtRes, err := seedHistoricalTransactions(ctx, store, connID, "123456", []historicalTransactionFixture{
		{Number: "TXN_TPL_1", Credit: 1000000, Debit: 0, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Shop Pay"},
	}, "REALTIME")
	if err != nil || len(rtRes.NewEvents) == 0 {
		t.Fatalf("ingest: %v", err)
	}
	txnID := rtRes.NewEvents[0].TransactionID

	txReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/transactions/"+txnID, bytes.NewReader([]byte(`{}`))),
		cookie,
		token,
	)
	txW := httptest.NewRecorder()
	server.Handler().ServeHTTP(txW, txReq)
	if txW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", txW.Code, txW.Body.String())
	}
	expectedTx := "Cảm ơn quý khách đã gửi một triệu đồng."
	if lastTTSReq.Text != expectedTx {
		t.Errorf("expected transaction text %q, got %q", expectedTx, lastTTSReq.Text)
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
	rtRes, err := seedHistoricalTransactions(ctx, store, connID, "123456", []historicalTransactionFixture{
		{Number: "TXN_PUB_1", Credit: 250000, Debit: 0, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Tip"},
	}, "REALTIME")
	if err != nil || len(rtRes.NewEvents) == 0 {
		t.Fatalf("ingest: %v", err)
	}
	freshTxnID := rtRes.NewEvents[0].TransactionID

	// 1. Call unauthenticated public endpoint without overrides
	pubBody, _ := json.Marshal(map[string]any{})
	pubReq := httptest.NewRequest(http.MethodPost, "/api/public/v1/voice/transactions/"+freshTxnID, bytes.NewReader(pubBody))
	pubW := httptest.NewRecorder()
	server.Handler().ServeHTTP(pubW, pubReq)

	if pubW.Code != http.StatusOK {
		t.Fatalf("expected 200 from public voice endpoint, got %d: %s", pubW.Code, pubW.Body.String())
	}
	if pubW.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("expected Content-Type audio/mpeg, got %s", pubW.Header().Get("Content-Type"))
	}
	if !strings.Contains(lastTTSReq.Text, voicecopy.SpeakVND(250000)) {
		t.Errorf("public audio did not preserve DB credit amount: %q", lastTTSReq.Text)
	}
	if !lastTTSReq.Cacheable {
		t.Errorf("expected Cacheable=true on public voice synthesis")
	}

	// 2. Anti-abuse: client override rejected with 400 VOICE_OVERRIDE_NOT_ALLOWED
	abuseBody, _ := json.Marshal(map[string]any{
		"template": "Hacker text with no amount token",
	})
	abuseReq := httptest.NewRequest(http.MethodPost, "/api/public/v1/voice/transactions/"+freshTxnID, bytes.NewReader(abuseBody))
	abuseW := httptest.NewRecorder()
	server.Handler().ServeHTTP(abuseW, abuseReq)
	if abuseW.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for override, got %d", abuseW.Code)
	}
	// 3. Non-realtime source rejected with 422
	cuRes, err := seedHistoricalTransactions(ctx, store, connID, "123456", []historicalTransactionFixture{
		{Number: "TXN_PUB_CU", Credit: 500000, Debit: 0, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Historical"},
	}, "CATCH_UP")
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

	rtRes, err := seedHistoricalTransactions(ctx, store, connID, "123456", []historicalTransactionFixture{
		{Number: "TXN_SUM_1", Credit: 100000, Debit: 0, TransactionAt: "12/09/2026 10:00:00", EffectiveAt: "12/09/2026", Description: "Order 1"},
		{Number: "TXN_SUM_2", Credit: 200000, Debit: 0, TransactionAt: "12/09/2026 10:01:00", EffectiveAt: "12/09/2026", Description: "Order 2"},
	}, "REALTIME")
	if err != nil || len(rtRes.NewEvents) < 2 {
		t.Fatalf("ingest: %v", err)
	}
	txID1 := rtRes.NewEvents[0].TransactionID
	txID2 := rtRes.NewEvents[1].TransactionID

	// 1. GET with overrides rejected with 400 VOICE_OVERRIDE_NOT_ALLOWED
	qBad := url.Values{}
	qBad.Set("transactionIds", fmt.Sprintf("%s,%s", txID1, txID2))
	qBad.Set("rate", "+200%")
	qBad.Set("pitch", "+80Hz")
	badURL := "http://example.test/api/v1/voice/transactions/summary/stream?" + qBad.Encode()
	badReq := prepareAuthedRequest(
		httptest.NewRequest(http.MethodGet, badURL, nil),
		cookie,
		token,
	)
	badW := httptest.NewRecorder()
	server.Handler().ServeHTTP(badW, badReq)
	if badW.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for override, got %d", badW.Code)
	}

	// 2. GET /voice/transactions/summary/stream without overrides returns 200
	q := url.Values{}
	q.Set("transactionIds", fmt.Sprintf("%s,%s", txID1, txID2))
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
}
