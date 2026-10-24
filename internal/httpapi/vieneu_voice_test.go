package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
)

func TestVieneuVoicePackMetadataMatchesExplicitProvider(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		profile := storage.DefaultVoiceProfile
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

func TestRetainedVieneuManifestThroughUpstreamAndGoProxy(t *testing.T) {
	const key = "b5a2b7b8dec26c5ec21c3f8a7d91fa33ee75bcc587b51f444a6dbcb6bb34fcf0"
	const packVersion = "4710505ae4e7d31113d96623d0e3df6471b8c2e3781e00da4778ba79eacea8fd"
	retained, err := os.ReadFile(filepath.Join("..", "..", "tts-gateway", "assets", "voicepacks", key, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	server, store, originalTTS, cookie, token := setupTestVoiceServer(t)
	defer store.Close()
	defer originalTTS.Close()
	manifestBytes := retained
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.Method != http.MethodGet || r.URL.Path != "/assets/profiles/"+key+"/manifest" || r.Header.Get("X-Internal-TTS-Token") != "retained-pack-secret" {
			t.Errorf("unexpected retained-pack request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(manifestBytes)
	}))
	defer upstream.Close()
	client := ttsclient.New(upstream.URL, "retained-pack-secret")
	server.WithTTSClient(client)
	if storage.DefaultVoiceProfile.SynthesisKey != key {
		t.Fatalf("fixed profile lost retained synthesis key: %s", storage.DefaultVoiceProfile.SynthesisKey)
	}
	for _, damage := range []string{"", "voice", "hash", "rate", "pitch"} {
		name := damage
		if name == "" {
			name = "published"
		}
		t.Run(name, func(t *testing.T) {
			manifestBytes = retained
			if damage != "" {
				var manifest map[string]any
				if err := json.Unmarshal(retained, &manifest); err != nil {
					t.Fatal(err)
				}
				switch damage {
				case "voice":
					manifest["voiceId"] = "vi-VN-vais1000-medium"
				case "rate":
					manifest["rate"] = 1.25
				case "pitch":
					manifest["pitchHz"] = 5
				}
				delete(manifest, "packVersion")
				var canonical bytes.Buffer
				encoder := json.NewEncoder(&canonical)
				encoder.SetEscapeHTML(false)
				if err := encoder.Encode(manifest); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte("\n")))
				manifest["packVersion"] = hex.EncodeToString(digest[:])
				if damage == "hash" {
					manifest["packVersion"] = strings.Repeat("0", 64)
				}
				manifestBytes, err = json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := upstreamCalls
			data, manifest, status, err := client.VoiceManifest(context.Background(), key)
			if damage == "" {
				if err != nil || status != http.StatusOK || manifest == nil {
					t.Fatalf("retained manifest rejected: status=%d err=%v", status, err)
				}
				if !bytes.Equal(data, retained) || manifest.PackVersion != packVersion || manifest.SynthesisKey != key || manifest.SchemaVersion != 2 || manifest.VoiceID != storage.VoiceIDVieneuMaiAnh || manifest.Rate != 1 || manifest.PitchHz != 0 || len(manifest.Clips) != 140 {
					t.Fatalf("retained pack identity changed: %+v", manifest)
				}
			} else if err == nil {
				t.Fatalf("client accepted wrong %s", damage)
			}
			for _, prefix := range []string{"/api/public/v1", "/api/v1"} {
				request := httptest.NewRequest(http.MethodGet, "http://example.test"+prefix+"/voice/profiles/"+key+"/manifest", nil)
				if prefix == "/api/v1" {
					request = prepareAuthedRequest(request, cookie, token)
				}
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				if damage != "" {
					if response.Code != http.StatusNotFound || bytes.Equal(response.Body.Bytes(), manifestBytes) {
						t.Fatalf("proxy accepted wrong %s: status=%d", damage, response.Code)
					}
					continue
				}
				if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), retained) || response.Header().Get("ETag") != `"`+packVersion+`"` || response.Header().Get("Cache-Control") != "no-cache" {
					t.Fatalf("retained proxy response: status=%d headers=%v", response.Code, response.Header())
				}
				request.Header.Set("If-None-Match", `"`+packVersion+`"`)
				response = httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				if response.Code != http.StatusNotModified || response.Body.Len() != 0 {
					t.Fatalf("retained conditional response: status=%d body=%s", response.Code, response.Body.String())
				}
			}
			wantCalls := 3
			if damage == "" {
				wantCalls = 5
			}
			if upstreamCalls-before != wantCalls {
				t.Fatalf("unexpected upstream call count: got %d want %d", upstreamCalls-before, wantCalls)
			}
		})
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
