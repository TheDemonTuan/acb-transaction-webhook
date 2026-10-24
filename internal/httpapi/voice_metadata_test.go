package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
)

func TestVoiceStreamRejectsDifferentVoiceBeforePlayback(t *testing.T) {
	for _, fixture := range []struct {
		provider, voice string
		fallback        bool
	}{
		{"edge", "vi-VN-NamMinhNeural", false},
		{"gtts", "vi-VN-HoaiMyNeural", false},
		{"edge", "vi-VN-HoaiMyNeural", true},
		{"edge", "vi-VN-vais1000-medium", false},
		{"piper", "vi-VN-HoaiMyNeural", false},
		{"piper", "vi-VN-vais1000-medium", true},
		{"vieneu", "vi-VN-vais1000-medium", false},
		{"piper", storage.VoiceIDVieneuMaiAnh, false},
		{"vieneu", storage.VoiceIDVieneuMaiAnh, true},
		{"", storage.VoiceIDVieneuMaiAnh, false},
	} {
		stream := &ttsclient.StreamResult{Reader: strings.NewReader("must not play"), Closer: io.NopCloser(strings.NewReader("")), Provider: fixture.provider, Voice: fixture.voice, Fallback: fixture.fallback}
		w := httptest.NewRecorder()
		writeAudioStream(w, stream, storage.DefaultVoiceProfile)
		if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "must not play") {
			t.Fatalf("wrong-profile synthesis escaped: status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

func TestVoiceStreamPreservesExplicitProviderMetadata(t *testing.T) {
	for _, profile := range []storage.VoiceProfile{
		storage.DefaultVoiceProfile,
		{Revision: 7, ProviderMode: storage.ProviderModeVieneuOnly, VoiceID: storage.VoiceIDVieneuMaiAnh, Rate: 1, SynthesisKey: storage.ComputeSynthesisKey(storage.VoiceIDVieneuMaiAnh, 1, 0)},
	} {
		stream := &ttsclient.StreamResult{Reader: strings.NewReader("exact audio"), Provider: profile.SynthesisProvider(), Voice: profile.VoiceID, Cached: true}
		w := httptest.NewRecorder()
		writeAudioStream(w, stream, profile)
		if w.Code != http.StatusOK || w.Body.String() != "exact audio" ||
			w.Header().Get("X-TTS-Provider") != profile.SynthesisProvider() ||
			w.Header().Get("X-TTS-Voice") != profile.VoiceID ||
			w.Header().Get("X-TTS-Fallback") != "false" ||
			w.Header().Get("X-Voice-Synthesis-Key") != profile.SynthesisKey {
			t.Fatalf("incorrect explicit provider response: %d %v", w.Code, w.Header())
		}
	}
}

func TestVoiceRevisionCannotBypassConflictWithMalformedQuery(t *testing.T) {
	for _, revision := range []string{"", "abc", "0", "-1", "01", "9223372036854775808"} {
		req := httptest.NewRequest(http.MethodGet, "/voice/test?profileRevision="+revision, nil)
		if _, err := parseVoiceAudioRequest(req); err == nil {
			t.Fatalf("malformed revision accepted: %q", revision)
		}
	}
}
