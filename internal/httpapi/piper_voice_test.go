package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
)

func TestVoicePackMetadataMatchesExplicitProvider(t *testing.T) {
	for _, provider := range []string{storage.ProviderModePiperOnly, storage.ProviderModeEdgeOnly} {
		for _, mismatch := range []bool{false, true} {
			profile := storage.DefaultVoiceProfile
			profile.ProviderMode = provider
			if provider == storage.ProviderModeEdgeOnly {
				profile.VoiceID = storage.VoiceIDNamMinh
				profile.Rate = 1.25
			} else if provider == storage.ProviderModePiperOnly {
				profile.VoiceID = storage.VoiceIDPiperVais1000
				profile.Rate = 1.25
			}
			profile.SynthesisKey = storage.ComputeSynthesisKey(profile.VoiceID, profile.Rate, profile.PitchHz)
			audio := []byte("ID3exact-pack-audio")
			digest := sha256.Sum256(audio)
			clipHash := hex.EncodeToString(digest[:])
			qualityEvidence := strings.Repeat("c", 64)
			manifest := ttsclient.AssetManifest{SchemaVersion: 2, QualityEvidenceSHA256: qualityEvidence, SynthesisKey: profile.SynthesisKey, VoiceID: profile.VoiceID, Rate: profile.Rate, PitchHz: profile.PitchHz, FormatterVersion: "money-vi-v1", CatalogVersion: "v1", Composition: ttsclient.CompositionModes{Tail: "whole", Numbers: "whole"}, PreloadClipIDs: []string{"amount-500000"}, Clips: []ttsclient.AssetClip{{ClipID: "amount-500000", Kind: "amount", Transcript: "Đã nhận năm trăm nghìn đồng.", SHA256: clipHash, Bytes: int64(len(audio)), DurationMS: 1000, SampleRate: 24000}}}
			manifest.Amounts = map[string]any{"500000": map[string]any{"standard": "amount-500000", "classic": map[string]any{}}}
			if mismatch {
				manifest.VoiceID = "wrong-voice"
			}
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			var canonical map[string]any
			if err := json.Unmarshal(raw, &canonical); err != nil {
				t.Fatal(err)
			}
			delete(canonical, "packVersion")
			raw, err = json.Marshal(canonical)
			if err != nil {
				t.Fatal(err)
			}
			packHash := sha256.Sum256(raw)
			manifest.PackVersion = hex.EncodeToString(packHash[:])
			manifestBytes, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			clipCalls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/assets/profiles/" + profile.SynthesisKey + "/manifest":
					_, _ = w.Write(manifestBytes)
				case "/assets/clips/" + clipHash:
					clipCalls++
					_, _ = w.Write(audio)
				default:
					http.NotFound(w, r)
				}
			}))
			server := &Server{ttsClient: ttsclient.New(upstream.URL, "")}
			w := httptest.NewRecorder()
			served := server.tryVoicePack(w, httptest.NewRequest(http.MethodGet, "/voice/test", nil), profile, manifest.Clips[0].Transcript)
			upstream.Close()
			if mismatch {
				if served || clipCalls != 0 || w.Body.Len() != 0 {
					t.Fatal("mismatched asset escaped exact-profile matching")
				}
				continue
			}
			if !served || !bytes.Equal(w.Body.Bytes(), audio) || w.Header().Get("X-TTS-Provider") != profile.SynthesisProvider() || w.Header().Get("X-TTS-Voice") != profile.VoiceID || w.Header().Get("X-TTS-Fallback") != "false" || w.Header().Get("X-TTS-Cached") != "true" || w.Header().Get("X-Voice-Synthesis-Key") != profile.SynthesisKey {
				t.Fatalf("incorrect pack metadata/body: served=%v headers=%v body=%s", served, w.Header(), w.Body.String())
			}
		}
	}
}

func TestRetiredVoiceFieldIsRejectedRatherThanAliased(t *testing.T) {
	server, store, upstream, cookie, token := setupTestVoiceServer(t)
	defer store.Close()
	defer upstream.Close()
	profile := httptest.NewRecorder()
	server.Handler().ServeHTTP(profile, httptest.NewRequest(http.MethodGet, "/api/public/v1/voice/profile", nil))
	if profile.Code != http.StatusOK || !strings.Contains(profile.Body.String(), `"voiceId":"`+storage.DefaultVoiceProfile.VoiceID+`"`) || strings.Contains(profile.Body.String(), `"edgeVoice"`) {
		t.Fatalf("incorrect public profile: %s", profile.Body.String())
	}
	request := prepareAuthedRequest(httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/voice/test", strings.NewReader(`{"edgeVoice":"vi-VN-HoaiMyNeural"}`)), cookie, token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "VOICE_OVERRIDE_NOT_ALLOWED") {
		t.Fatalf("retired override accepted: %d %s", response.Code, response.Body.String())
	}
	request = prepareAuthedRequest(httptest.NewRequest(http.MethodPut, "http://example.test/api/v1/voice/settings", strings.NewReader(`{"providerMode":"PIPER_ONLY","voiceId":"vi-VN-vais1000-medium","edgeVoice":"vi-VN-HoaiMyNeural"}`)), cookie, token)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("retired settings field accepted: %d", response.Code)
	}
}
