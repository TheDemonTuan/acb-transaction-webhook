package ttsclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func TestVoiceClipIntegrityAndAuthentication(t *testing.T) {
	audio := []byte("ID3 bounded fixture")
	digest := sha256.Sum256(audio)
	hash := hex.EncodeToString(digest[:])
	corrupt := false
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/assets/clips/"+hash || r.Header.Get("X-Internal-TTS-Token") != "asset-secret" {
			t.Errorf("unexpected asset request: %s %s", r.Method, r.URL.Path)
		}
		if corrupt {
			_, _ = w.Write([]byte("different audio"))
			return
		}
		_, _ = w.Write(audio)
	}))
	defer upstream.Close()
	client := New(upstream.URL, "asset-secret")
	got, status, err := client.VoiceClip(context.Background(), hash)
	if err != nil || status != 200 || string(got) != string(audio) {
		t.Fatalf("clip status=%d error=%v", status, err)
	}
	corrupt = true
	if _, _, err = client.VoiceClip(context.Background(), hash); err == nil {
		t.Fatal("corrupt clip accepted")
	}
	before := requests
	for _, invalid := range []string{"../private", strings.Repeat("0", 63), strings.Repeat("A", 64)} {
		_, status, err := client.VoiceClip(context.Background(), invalid)
		if err != nil || status != 404 {
			t.Fatalf("invalid key status=%d error=%v", status, err)
		}
	}
	if requests != before {
		t.Fatal("invalid asset key reached upstream")
	}
}

func TestVoiceManifestRejectsWrongProfileAndOversizedClips(t *testing.T) {
	key := storage.ComputeSynthesisKey(storage.VoiceIDPiperVais1000, 1.25, 0)
	other := strings.Repeat("b", 64)
	manifestKey := other
	clipBytes := int64(1)
	qualitySha := strings.Repeat("c", 64)
	damage := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manifest := map[string]any{
			"schemaVersion":         2,
			"synthesisKey":          manifestKey,
			"formatterVersion":      "money-vi-v1",
			"catalogVersion":        "v1",
			"qualityEvidenceSha256": qualitySha,
			"voiceId":               storage.VoiceIDPiperVais1000, "rate": 1.25, "pitchHz": 0,
			"composition":    map[string]any{"tail": "whole", "numbers": "whole"},
			"preloadClipIds": []string{"amount-1"},
			"amounts":        map[string]any{"1": map[string]any{"standard": "amount-1", "classic": map[string]any{}}},
			"clips": []any{map[string]any{
				"clipId": "amount-1", "kind": "amount", "transcript": "Đã nhận một đồng.",
				"speechStartMs": nil, "amountStartMs": nil,
				"sha256":     other,
				"bytes":      clipBytes,
				"durationMs": 100,
				"sampleRate": 24000,
			}},
		}
		clip := manifest["clips"].([]any)[0].(map[string]any)
		switch damage {
		case "schema1":
			manifest["schemaVersion"] = 1
		case "empty-clips":
			manifest["clips"] = []any{}
		case "missing-onset":
			delete(clip, "speechStartMs")
		case "invalid-onset":
			clip["amountStartMs"] = 100
		case "unknown-preload":
			manifest["preloadClipIds"] = []string{"missing"}
		case "incomplete-segments":
			manifest["composition"] = map[string]any{"tail": "whole", "numbers": "segments"}
		case "missing-amount":
			manifest["amounts"] = map[string]any{"1": map[string]any{"standard": "missing", "classic": map[string]any{}}}
		case "profile-rate":
			manifest["rate"] = 1.1
		}
		canonical, _ := json.Marshal(manifest)
		digest := sha256.Sum256(canonical)
		manifest["packVersion"] = hex.EncodeToString(digest[:])
		_ = json.NewEncoder(w).Encode(manifest)
	}))
	defer upstream.Close()
	client := New(upstream.URL, "secret")
	if _, _, _, err := client.VoiceManifest(context.Background(), key); err == nil {
		t.Fatal("wrong-profile manifest accepted")
	}
	manifestKey = key
	clipBytes = 4<<20 + 1
	if _, _, _, err := client.VoiceManifest(context.Background(), key); err == nil {
		t.Fatal("oversized clip accepted")
	}
	clipBytes = 1
	if _, _, status, err := client.VoiceManifest(context.Background(), key); err != nil || status != http.StatusOK {
		t.Fatalf("valid manifest status=%d error=%v", status, err)
	}
	for _, name := range []string{"schema1", "empty-clips", "missing-onset", "invalid-onset", "unknown-preload", "incomplete-segments", "missing-amount", "profile-rate"} {
		damage = name
		if _, _, _, err := client.VoiceManifest(context.Background(), key); err == nil {
			t.Fatalf("invalid manifest %s accepted", name)
		}
	}
}
